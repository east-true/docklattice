package grpcadapter

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/east-true/docklattice/internal/candidate/grpcadapter/pb"
	"github.com/east-true/docklattice/internal/transport"
)

const (
	serviceName   = "docklattice.prototype.grpc.v1.ReverseTransport"
	unaryMethod   = "/" + serviceName + "/Unary"
	receiveMethod = "/" + serviceName + "/Receive"
	duplexMethod  = "/" + serviceName + "/Duplex"
)

type reverseService interface {
	Unary(context.Context, *pb.OpenRequest) (*pb.UnaryResponse, error)
	Receive(*pb.OpenRequest, grpc.ServerStream) error
	Duplex(grpc.ServerStream) error
}

type agentService struct {
	handler    transport.Handler
	limits     transport.Limits
	dispatcher *sendDispatcher
}

func (s *agentService) call(req *pb.OpenRequest, kind transport.Kind) (transport.Call, error) {
	if req.Kind != uint32(kind) {
		return transport.Call{}, transport.Errorf(transport.CodeProtocol, "exchange kind mismatch")
	}
	class := transport.Class(req.TrafficClass)
	if !class.Valid() {
		return transport.Call{}, transport.Errorf(transport.CodeProtocol, "invalid traffic class")
	}
	if len(req.Payload) > s.limits.MaxMessageBytes {
		return transport.Call{}, transport.Errorf(transport.CodeMessageTooLarge, "message exceeds limit")
	}
	return transport.Call{Method: transport.Method(req.Method), Class: class, Payload: req.Payload}, nil
}

func (s *agentService) Unary(ctx context.Context, req *pb.OpenRequest) (*pb.UnaryResponse, error) {
	call, err := s.call(req, transport.KindUnary)
	if err != nil {
		return nil, toGRPC(err)
	}
	payload, err := s.handler.Unary(ctx, call)
	if err != nil {
		return nil, toGRPC(err)
	}
	if len(payload) > s.limits.MaxMessageBytes {
		return nil, toGRPC(transport.Errorf(transport.CodeMessageTooLarge, "response exceeds limit"))
	}
	return &pb.UnaryResponse{Payload: payload}, nil
}

func (s *agentService) Receive(req *pb.OpenRequest, stream grpc.ServerStream) error {
	call, err := s.call(req, transport.KindReceive)
	if err != nil {
		return toGRPC(err)
	}
	if err := stream.SendHeader(metadata.MD{}); err != nil {
		return err
	}
	return toGRPC(s.handler.Receive(stream.Context(), call, &serverSender{id: transport.ExchangeID(req.ExchangeId), class: call.Class, stream: stream, limits: s.limits, dispatcher: s.dispatcher}))
}

func (s *agentService) Duplex(stream grpc.ServerStream) error {
	var first pb.StreamFrame
	if err := stream.RecvMsg(&first); err != nil {
		return toGRPC(fromGRPC(err))
	}
	open, ok := first.Body.(*pb.StreamFrame_Open)
	if !ok {
		return toGRPC(transport.Errorf(transport.CodeProtocol, "first duplex frame must open exchange"))
	}
	call, err := s.call(open.Open, transport.KindDuplex)
	if err != nil {
		return toGRPC(err)
	}
	return toGRPC(s.handler.Duplex(stream.Context(), call, &serverDuplex{serverSender: serverSender{id: transport.ExchangeID(open.Open.ExchangeId), class: call.Class, stream: stream, limits: s.limits, dispatcher: s.dispatcher}}))
}

type serverSender struct {
	id         transport.ExchangeID
	class      transport.Class
	stream     grpc.ServerStream
	limits     transport.Limits
	dispatcher *sendDispatcher
}

func (s *serverSender) ID() transport.ExchangeID { return s.id }
func (s *serverSender) Send(ctx context.Context, msg []byte) error {
	if len(msg) > s.limits.MaxMessageBytes {
		return transport.Errorf(transport.CodeMessageTooLarge, "stream message exceeds limit")
	}
	return s.dispatcher.send(ctx, s.class, func() error {
		return s.stream.SendMsg(&pb.StreamFrame{Body: &pb.StreamFrame_Payload{Payload: msg}})
	})
}

type serverDuplex struct{ serverSender }

func (s *serverDuplex) Recv(ctx context.Context) ([]byte, error) {
	type result struct {
		frame *pb.StreamFrame
		err   error
	}
	done := make(chan result, 1)
	go func() {
		var frame pb.StreamFrame
		err := s.stream.RecvMsg(&frame)
		done <- result{&frame, err}
	}()
	select {
	case <-ctx.Done():
		return nil, transport.Wrap(transport.StatusOf(ctx.Err()).Code, ctx.Err(), "receive canceled")
	case r := <-done:
		if errors.Is(r.err, io.EOF) {
			return nil, io.EOF
		}
		if r.err != nil {
			return nil, fromGRPC(r.err)
		}
		payload, ok := r.frame.Body.(*pb.StreamFrame_Payload)
		if !ok {
			return nil, transport.Errorf(transport.CodeProtocol, "expected duplex payload")
		}
		if len(payload.Payload) > s.limits.MaxMessageBytes {
			return nil, transport.Errorf(transport.CodeMessageTooLarge, "stream message exceeds limit")
		}
		return payload.Payload, nil
	}
}

func registerService(server *grpc.Server, service reverseService) {
	server.RegisterService(&serviceDesc, service)
}

var serviceDesc = grpc.ServiceDesc{
	ServiceName: serviceName,
	HandlerType: (*reverseService)(nil),
	Methods: []grpc.MethodDesc{{MethodName: "Unary", Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		var req pb.OpenRequest
		if err := dec(&req); err != nil {
			return nil, err
		}
		if interceptor == nil {
			return srv.(reverseService).Unary(ctx, &req)
		}
		info := &grpc.UnaryServerInfo{Server: srv, FullMethod: unaryMethod}
		return interceptor(ctx, &req, info, func(ctx context.Context, req any) (any, error) {
			return srv.(reverseService).Unary(ctx, req.(*pb.OpenRequest))
		})
	}}},
	Streams: []grpc.StreamDesc{
		{StreamName: "Receive", ServerStreams: true, Handler: func(srv any, stream grpc.ServerStream) error {
			var req pb.OpenRequest
			if err := stream.RecvMsg(&req); err != nil {
				return err
			}
			return srv.(reverseService).Receive(&req, stream)
		}},
		{StreamName: "Duplex", ServerStreams: true, ClientStreams: true, Handler: func(srv any, stream grpc.ServerStream) error {
			return srv.(reverseService).Duplex(stream)
		}},
	},
	Metadata: "candidate-a-private",
}

func toGRPC(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	s := transport.StatusOf(err)
	code := codes.Internal
	switch s.Code {
	case transport.CodeCanceled:
		code = codes.Canceled
	case transport.CodeDeadlineExceeded:
		code = codes.DeadlineExceeded
	case transport.CodeUnavailable:
		code = codes.Unavailable
	case transport.CodeMessageTooLarge, transport.CodeResourceExhausted:
		code = codes.ResourceExhausted
	case transport.CodeProtocol:
		code = codes.InvalidArgument
	case transport.CodeUnimplemented:
		code = codes.Unimplemented
	}
	return status.Error(code, s.Reason)
}

func fromGRPC(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	s, ok := status.FromError(err)
	if !ok {
		if errors.Is(err, context.Canceled) {
			return transport.Wrap(transport.CodeCanceled, err, "context canceled")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return transport.Wrap(transport.CodeDeadlineExceeded, err, "deadline exceeded")
		}
		return transport.Wrap(transport.CodeUnavailable, err, "gRPC transport")
	}
	code := transport.CodeInternal
	switch s.Code() {
	case codes.Canceled:
		code = transport.CodeCanceled
	case codes.DeadlineExceeded:
		code = transport.CodeDeadlineExceeded
	case codes.Unavailable:
		code = transport.CodeUnavailable
	case codes.ResourceExhausted:
		code = transport.CodeMessageTooLarge
	case codes.InvalidArgument, codes.FailedPrecondition:
		code = transport.CodeProtocol
	case codes.Unimplemented:
		code = transport.CodeUnimplemented
	}
	return transport.Wrap(code, err, "%s", s.Message())
}
