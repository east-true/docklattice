package contract

import (
	"context"

	"google.golang.org/protobuf/proto"

	pb "github.com/east-true/docklattice/internal/contract/pb"
	"github.com/east-true/docklattice/internal/transport"
)

// Sink는 Responder가 Caller로 타입 있는 메시지를 밀어 보내는 통로다.
type Sink[T proto.Message] struct {
	out transport.Sender
}

// NewSink adapts a transport Sender for prototype services and focused tests.
func NewSink[T proto.Message](out transport.Sender) Sink[T] { return Sink[T]{out: out} }

// Send는 메시지를 직렬화해 보낸다. Caller가 소비하지 못하면 ctx가 끝날
// 때까지 블록한다.
func (s Sink[T]) Send(ctx context.Context, msg T) error {
	b, err := proto.Marshal(msg)
	if err != nil {
		return transport.Wrap(transport.CodeInternal, err, "marshal %T", msg)
	}
	return s.out.Send(ctx, b)
}

// ID는 하위 Exchange id다.
func (s Sink[T]) ID() transport.ExchangeID { return s.out.ID() }

// AuditSyncStream은 Agent 측에서 본 Audit Sync duplex다.
//
// Agent -> Server : SyncAuditMessage (record / coverage_changed / ack_result)
// Server -> Agent : SyncAuditAck
type AuditSyncStream struct {
	ch transport.Duplex
}

func (s AuditSyncStream) Send(ctx context.Context, msg *pb.SyncAuditMessage) error {
	b, err := proto.Marshal(msg)
	if err != nil {
		return transport.Wrap(transport.CodeInternal, err, "marshal SyncAuditMessage")
	}
	return s.ch.Send(ctx, b)
}

// Recv는 Server가 보낸 다음 ACK를 반환한다. Server가 CloseSend하면 io.EOF다.
func (s AuditSyncStream) Recv(ctx context.Context) (*pb.SyncAuditAck, error) {
	b, err := s.ch.Recv(ctx)
	if err != nil {
		return nil, err
	}
	var ack pb.SyncAuditAck
	if err := proto.Unmarshal(b, &ack); err != nil {
		return nil, transport.Wrap(transport.CodeProtocol, err, "unmarshal SyncAuditAck")
	}
	return &ack, nil
}

// Service는 Agent 측 논리 구현이다. 부록 A.4 목록 그대로이며 이 밖의
// 기능은 정의하지 않는다.
type Service interface {
	Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error)
	Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error)
	CancelOperation(ctx context.Context, req *pb.CancelOperationRequest) (*pb.CancelOperationResponse, error)
	OperationProgress(ctx context.Context, req *pb.OperationProgressRequest, out Sink[*pb.OperationProgressEvent]) error

	SyncAudit(ctx context.Context, stream AuditSyncStream) error
	GetAuditCoverage(ctx context.Context, req *pb.GetAuditCoverageRequest) (*pb.AuditCoverageSnapshot, error)

	Echo(ctx context.Context, req *pb.EchoRequest) (*pb.EchoResponse, error)

	StreamLogs(ctx context.Context, req *pb.StreamLogsRequest, out Sink[*pb.LogChunk]) error
	OperationOutput(ctx context.Context, req *pb.OperationOutputRequest, out Sink[*pb.OperationOutputChunk]) error

	StreamStats(ctx context.Context, req *pb.StreamStatsRequest, out Sink[*pb.StatsSample]) error
}

// NewHandler는 Service를 transport.Handler로 감싼다. Method와 Kind의
// 대응은 specs 표에서만 온다.
func NewHandler(svc Service) transport.Handler { return &handler{svc: svc} }

type handler struct {
	svc Service
}

func (h *handler) Unary(ctx context.Context, call transport.Call) ([]byte, error) {
	if err := checkKind(call, transport.KindUnary); err != nil {
		return nil, err
	}
	switch call.Method {
	case MethodRegister:
		return unary(ctx, call, &pb.RegisterRequest{}, h.svc.Register)
	case MethodHeartbeat:
		return unary(ctx, call, &pb.HeartbeatRequest{}, h.svc.Heartbeat)
	case MethodCancelOperation:
		return unary(ctx, call, &pb.CancelOperationRequest{}, h.svc.CancelOperation)
	case MethodGetAuditCoverage:
		return unary(ctx, call, &pb.GetAuditCoverageRequest{}, h.svc.GetAuditCoverage)
	case MethodEcho:
		return unary(ctx, call, &pb.EchoRequest{}, h.svc.Echo)
	default:
		return nil, transport.Errorf(transport.CodeUnimplemented, "unary method %q", call.Method)
	}
}

func (h *handler) Receive(ctx context.Context, call transport.Call, out transport.Sender) error {
	if err := checkKind(call, transport.KindReceive); err != nil {
		return err
	}
	switch call.Method {
	case MethodOperationProgress:
		return stream(ctx, call, &pb.OperationProgressRequest{}, Sink[*pb.OperationProgressEvent]{out}, h.svc.OperationProgress)
	case MethodStreamLogs:
		return stream(ctx, call, &pb.StreamLogsRequest{}, Sink[*pb.LogChunk]{out}, h.svc.StreamLogs)
	case MethodOperationOutput:
		return stream(ctx, call, &pb.OperationOutputRequest{}, Sink[*pb.OperationOutputChunk]{out}, h.svc.OperationOutput)
	case MethodStreamStats:
		return stream(ctx, call, &pb.StreamStatsRequest{}, Sink[*pb.StatsSample]{out}, h.svc.StreamStats)
	default:
		return transport.Errorf(transport.CodeUnimplemented, "receive method %q", call.Method)
	}
}

func (h *handler) Duplex(ctx context.Context, call transport.Call, ch transport.Duplex) error {
	if err := checkKind(call, transport.KindDuplex); err != nil {
		return err
	}
	switch call.Method {
	case MethodSyncAudit:
		return h.svc.SyncAudit(ctx, AuditSyncStream{ch: ch})
	default:
		return transport.Errorf(transport.CodeUnimplemented, "duplex method %q", call.Method)
	}
}

// checkKind는 Method가 계약된 Kind와 Class로 열렸는지 확인한다.
// 계약 위반을 조용히 수용하면 두 후보의 비교 조건이 어긋난다.
func checkKind(call transport.Call, kind transport.Kind) error {
	spec, ok := SpecOf(call.Method)
	if !ok {
		return transport.Errorf(transport.CodeUnimplemented, "unknown method %q", call.Method)
	}
	if spec.Kind != kind {
		return transport.Errorf(transport.CodeProtocol,
			"method %q expects %s exchange, got %s", call.Method, spec.Kind, kind)
	}
	if spec.Class != call.Class {
		return transport.Errorf(transport.CodeProtocol,
			"method %q expects class %s, got %s", call.Method, spec.Class, call.Class)
	}
	return nil
}

func unary[Req, Resp proto.Message](
	ctx context.Context,
	call transport.Call,
	req Req,
	fn func(context.Context, Req) (Resp, error),
) ([]byte, error) {
	if err := proto.Unmarshal(call.Payload, req); err != nil {
		return nil, transport.Wrap(transport.CodeProtocol, err, "unmarshal %s request", call.Method)
	}
	resp, err := fn(ctx, req)
	if err != nil {
		return nil, err
	}
	b, err := proto.Marshal(resp)
	if err != nil {
		return nil, transport.Wrap(transport.CodeInternal, err, "marshal %s response", call.Method)
	}
	return b, nil
}

func stream[Req proto.Message, Msg proto.Message](
	ctx context.Context,
	call transport.Call,
	req Req,
	sink Sink[Msg],
	fn func(context.Context, Req, Sink[Msg]) error,
) error {
	if err := proto.Unmarshal(call.Payload, req); err != nil {
		return transport.Wrap(transport.CodeProtocol, err, "unmarshal %s request", call.Method)
	}
	return fn(ctx, req, sink)
}
