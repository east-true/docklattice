package contract

import (
	"context"

	"google.golang.org/protobuf/proto"

	pb "github.com/east-true/docklattice/internal/contract/pb"
	"github.com/east-true/docklattice/internal/transport"
)

// Client is the transport-neutral Server-side view of the prototype service.
type Client struct{ caller transport.Caller }

func NewClient(caller transport.Caller) *Client { return &Client{caller: caller} }

func (c *Client) Session() transport.Caller { return c.caller }

func invoke[Req, Resp proto.Message](ctx context.Context, c *Client, spec Spec, req Req, resp Resp) error {
	b, err := proto.Marshal(req)
	if err != nil {
		return transport.Wrap(transport.CodeInternal, err, "marshal %s request", spec.Method)
	}
	out, err := c.caller.Invoke(ctx, spec.call(b))
	if err != nil {
		return err
	}
	if err := proto.Unmarshal(out, resp); err != nil {
		return transport.Wrap(transport.CodeProtocol, err, "unmarshal %s response", spec.Method)
	}
	return nil
}

func (c *Client) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	var resp pb.RegisterResponse
	err := invoke(ctx, c, specs[MethodRegister], req, &resp)
	return &resp, err
}

func (c *Client) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	var resp pb.HeartbeatResponse
	err := invoke(ctx, c, specs[MethodHeartbeat], req, &resp)
	return &resp, err
}

func (c *Client) CancelOperation(ctx context.Context, req *pb.CancelOperationRequest) (*pb.CancelOperationResponse, error) {
	var resp pb.CancelOperationResponse
	err := invoke(ctx, c, specs[MethodCancelOperation], req, &resp)
	return &resp, err
}

func (c *Client) GetAuditCoverage(ctx context.Context, req *pb.GetAuditCoverageRequest) (*pb.AuditCoverageSnapshot, error) {
	var resp pb.AuditCoverageSnapshot
	err := invoke(ctx, c, specs[MethodGetAuditCoverage], req, &resp)
	return &resp, err
}

func (c *Client) Echo(ctx context.Context, req *pb.EchoRequest) (*pb.EchoResponse, error) {
	var resp pb.EchoResponse
	err := invoke(ctx, c, specs[MethodEcho], req, &resp)
	return &resp, err
}

type Receive[T proto.Message] struct {
	stream transport.ReceiveStream
	newMsg func() T
}

func (s *Receive[T]) Recv(ctx context.Context) (T, error) {
	msg := s.newMsg()
	b, err := s.stream.Recv(ctx)
	if err != nil {
		return msg, err
	}
	if err := proto.Unmarshal(b, msg); err != nil {
		return msg, transport.Wrap(transport.CodeProtocol, err, "unmarshal stream message")
	}
	return msg, nil
}

func (s *Receive[T]) Cancel(cause error)                 { s.stream.Cancel(cause) }
func (s *Receive[T]) Outcome() (transport.Outcome, bool) { return s.stream.Outcome() }
func (s *Receive[T]) ID() transport.ExchangeID           { return s.stream.ID() }

func openReceive[Req proto.Message, Msg proto.Message](
	ctx context.Context, c *Client, spec Spec, req Req, newMsg func() Msg,
) (*Receive[Msg], error) {
	b, err := proto.Marshal(req)
	if err != nil {
		return nil, transport.Wrap(transport.CodeInternal, err, "marshal %s request", spec.Method)
	}
	s, err := c.caller.OpenReceive(ctx, spec.call(b))
	if err != nil {
		return nil, err
	}
	return &Receive[Msg]{stream: s, newMsg: newMsg}, nil
}

func (c *Client) OperationProgress(ctx context.Context, req *pb.OperationProgressRequest) (*Receive[*pb.OperationProgressEvent], error) {
	return openReceive(ctx, c, specs[MethodOperationProgress], req, func() *pb.OperationProgressEvent { return &pb.OperationProgressEvent{} })
}

func (c *Client) StreamLogs(ctx context.Context, req *pb.StreamLogsRequest) (*Receive[*pb.LogChunk], error) {
	return openReceive(ctx, c, specs[MethodStreamLogs], req, func() *pb.LogChunk { return &pb.LogChunk{} })
}

func (c *Client) OperationOutput(ctx context.Context, req *pb.OperationOutputRequest) (*Receive[*pb.OperationOutputChunk], error) {
	return openReceive(ctx, c, specs[MethodOperationOutput], req, func() *pb.OperationOutputChunk { return &pb.OperationOutputChunk{} })
}

func (c *Client) StreamStats(ctx context.Context, req *pb.StreamStatsRequest) (*Receive[*pb.StatsSample], error) {
	return openReceive(ctx, c, specs[MethodStreamStats], req, func() *pb.StatsSample { return &pb.StatsSample{} })
}

type AuditSyncClient struct{ stream transport.DuplexStream }

func (c *Client) SyncAudit(ctx context.Context) (*AuditSyncClient, error) {
	b, _ := proto.Marshal(&pb.GetAuditCoverageRequest{})
	s, err := c.caller.OpenDuplex(ctx, specs[MethodSyncAudit].call(b))
	if err != nil {
		return nil, err
	}
	return &AuditSyncClient{stream: s}, nil
}

func (s *AuditSyncClient) Send(ctx context.Context, ack *pb.SyncAuditAck) error {
	b, err := proto.Marshal(ack)
	if err != nil {
		return transport.Wrap(transport.CodeInternal, err, "marshal SyncAuditAck")
	}
	return s.stream.Send(ctx, b)
}

func (s *AuditSyncClient) Recv(ctx context.Context) (*pb.SyncAuditMessage, error) {
	b, err := s.stream.Recv(ctx)
	if err != nil {
		return nil, err
	}
	var msg pb.SyncAuditMessage
	if err := proto.Unmarshal(b, &msg); err != nil {
		return nil, transport.Wrap(transport.CodeProtocol, err, "unmarshal SyncAuditMessage")
	}
	return &msg, nil
}

func (s *AuditSyncClient) Cancel(cause error) { s.stream.Cancel(cause) }
func (s *AuditSyncClient) CloseSend() error   { return s.stream.CloseSend() }
