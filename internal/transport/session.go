package transport

import "context"

// AgentID는 Agent 설치 identity다(ADR §11.4).
type AgentID string

// SessionID는 재연결마다 새로 발급되는 opaque 값이다(ADR §5.3).
// 최소 128-bit cryptographically random이며 영속 저장하지 않는다.
type SessionID string

// ExchangeID는 Session 안에서 하나의 Exchange를 식별한다.
// 상관관계 id(계약 C5)의 전송 계층 표현이며 Session을 넘어 유효하지 않다.
type ExchangeID uint64

// Method는 논리 요청 이름이다. 부록 A.4의 공통 최소 구현 범위에 정의된
// 이름만 사용한다. 전송 기술에 따라 이름이 달라지지 않는다.
type Method string

// Kind는 Exchange의 형태다.
type Kind uint8

const (
	// KindUnary는 요청 1건에 응답 1건이다.
	KindUnary Kind = iota

	// KindReceive는 Caller가 열고 Responder가 스트림으로 밀어 보낸다.
	// Agent -> Server 방향의 logs, stats, operation progress, operation
	// output이 여기 속한다.
	KindReceive

	// KindDuplex는 양방향 스트림이다. Audit Sync가 여기 속한다.
	KindDuplex
)

func (k Kind) String() string {
	switch k {
	case KindUnary:
		return "unary"
	case KindReceive:
		return "receive"
	case KindDuplex:
		return "duplex"
	default:
		return "unknown"
	}
}

// Call은 Exchange 개시 요청이다.
type Call struct {
	// Method는 논리 요청 이름이다.
	Method Method

	// Class는 이 Exchange 전체에 적용되는 트래픽 클래스다.
	// Exchange 도중에 바뀌지 않는다.
	Class Class

	// Payload는 초기 요청 페이로드다. 스트림 Exchange에서도 Responder가
	// 무엇을 보낼지 결정하는 인자로 쓰인다(예: StreamLogs의 대상과 byte_rate).
	Payload []byte
}

// Limits는 Session에 적용되는 상한이다.
type Limits struct {
	// MaxMessageBytes를 넘는 메시지는 CodeMessageTooLarge로 거부한다.
	// 조용히 절단하지 않는다(계약 C8).
	MaxMessageBytes int

	// MaxConcurrentExchanges는 동시에 열려 있을 수 있는 Exchange 수다.
	// 0이면 제한 없음.
	MaxConcurrentExchanges int
}

// DefaultLimits는 두 후보가 동일 조건으로 비교되도록 하는 기본 상한이다.
// 실패를 감추기 위해 이 값을 후보별로 다르게 두지 않는다(부록 A.7).
func DefaultLimits() Limits {
	return Limits{
		MaxMessageBytes:        4 << 20,
		MaxConcurrentExchanges: 256,
	}
}

// ProtocolVersion은 현재 논리 프로토콜 버전이다. 연결 시 협상하며
// Server N은 Agent N-1과 호환된다(계약 C9).
const ProtocolVersion uint32 = 1

// MinCompatibleProtocolVersion은 수용 가능한 최소 상대 버전이다.
const MinCompatibleProtocolVersion uint32 = 1

// SessionInfo는 협상 결과다.
type SessionInfo struct {
	SessionID       SessionID
	AgentID         AgentID
	ProtocolVersion uint32
	Limits          Limits
}

// Session은 Agent가 시작한 지속 연결 위에 수립된 논리 세션이다.
// 양쪽이 공통으로 보는 부분이다.
type Session interface {
	// Info는 협상 결과를 반환한다.
	Info() SessionInfo

	// Done은 Session 종료 시 닫힌다.
	Done() <-chan struct{}

	// Err는 Session 종료 사유다. 종료 전에는 nil이다.
	Err() error

	// Close는 Session을 종료한다. 멱등이다. 진행 중인 Exchange는
	// CodeUnavailable 또는 cause로 종료된다.
	Close(cause error) error
}

// Caller는 Exchange를 시작할 수 있는 쪽(Server)의 Session이다.
type Caller interface {
	Session

	// Invoke는 unary Exchange를 수행한다. ctx 취소는 Responder까지 전파된다.
	Invoke(ctx context.Context, call Call) ([]byte, error)

	// OpenReceive는 Responder가 밀어 보내는 스트림을 연다.
	OpenReceive(ctx context.Context, call Call) (ReceiveStream, error)

	// OpenDuplex는 양방향 스트림을 연다.
	OpenDuplex(ctx context.Context, call Call) (DuplexStream, error)
}

// ReceiveStream은 Caller가 소비하는 스트림이다.
type ReceiveStream interface {
	ID() ExchangeID

	// Recv는 다음 논리 메시지를 반환한다. Responder가 정상 종료하면
	// io.EOF를 반환한다. 메시지 순서는 보장된다.
	//
	// Recv를 오래 호출하지 않으면 Responder의 Send가 결국 진행하지 못해야
	// 한다(bounded memory). 다만 그 정체가 같은 Session의 다른 Exchange를
	// 막아서는 안 된다.
	Recv(ctx context.Context) ([]byte, error)

	// Cancel은 Exchange를 취소하고 Responder까지 전파한다. 멱등이다.
	// Operation 취소가 아니라 스트림 취소다(ADR §9.1).
	Cancel(cause error)

	// Outcome은 종료된 Exchange의 terminal outcome을 반환한다.
	// 종료 전에는 ok=false다. 종료 후 값은 변하지 않는다.
	Outcome() (Outcome, bool)
}

// DuplexStream은 양방향 스트림이다.
type DuplexStream interface {
	ReceiveStream

	// Send는 Responder로 논리 메시지를 보낸다. 상대가 소비하지 못하면
	// ctx가 끝날 때까지 블록한다. 무한 버퍼링을 하지 않는다.
	Send(ctx context.Context, msg []byte) error

	// CloseSend는 더 보낼 것이 없음을 알린다. 수신은 계속한다.
	CloseSend() error
}

// Sender는 Responder가 Caller로 메시지를 밀어 보내는 통로다.
type Sender interface {
	ID() ExchangeID

	// Send는 Caller로 논리 메시지를 보낸다. Caller가 소비하지 못하면
	// ctx가 끝날 때까지 블록한다.
	Send(ctx context.Context, msg []byte) error
}

// Duplex는 Responder 측 양방향 통로다.
type Duplex interface {
	Sender

	// Recv는 Caller가 보낸 다음 메시지를 반환한다. Caller가 CloseSend하면
	// io.EOF를 반환한다.
	Recv(ctx context.Context) ([]byte, error)
}

// Handler는 Exchange를 처리하는 쪽(Agent)의 구현이다.
//
// 각 메서드에 전달되는 ctx는 Caller가 취소하거나 Session이 끊기면 취소된다.
// 반환한 error는 terminal Status로 환산되어 Caller에게 정확히 한 번
// 관찰된다. nil 반환은 정상 종료다.
type Handler interface {
	// Unary는 KindUnary Exchange를 처리한다.
	Unary(ctx context.Context, call Call) ([]byte, error)

	// Receive는 KindReceive Exchange를 처리한다. 반환하면 스트림이 종료된다.
	Receive(ctx context.Context, call Call, out Sender) error

	// Duplex는 KindDuplex Exchange를 처리한다.
	Duplex(ctx context.Context, call Call, ch Duplex) error
}

// Connector는 Agent 측 진입점이다. Server로 지속 연결을 만들고
// 들어오는 Exchange를 Handler에 위임한다.
type Connector interface {
	Connect(ctx context.Context, h Handler) (Session, error)
}

// Acceptor는 Server 측 진입점이다. Agent가 시작한 연결을 받아
// Caller Session으로 넘긴다.
type Acceptor interface {
	// Accept는 다음 Session을 반환한다. Close 이후에는 오류를 반환한다.
	Accept(ctx context.Context) (Caller, error)

	// Close는 더 이상 연결을 받지 않는다. 이미 수립된 Session은 닫지 않는다.
	Close() error
}

// UnimplementedHandler는 모든 Method를 CodeUnimplemented로 거부한다.
// 테스트에서 필요한 메서드만 덮어쓰기 위한 기본값이다.
type UnimplementedHandler struct{}

func (UnimplementedHandler) Unary(context.Context, Call) ([]byte, error) {
	return nil, Errorf(CodeUnimplemented, "unary not implemented")
}

func (UnimplementedHandler) Receive(context.Context, Call, Sender) error {
	return Errorf(CodeUnimplemented, "receive stream not implemented")
}

func (UnimplementedHandler) Duplex(context.Context, Call, Duplex) error {
	return Errorf(CodeUnimplemented, "duplex stream not implemented")
}

// HandlerFuncs는 함수로 Handler를 구성한다. nil 필드는 CodeUnimplemented다.
type HandlerFuncs struct {
	UnaryFunc   func(ctx context.Context, call Call) ([]byte, error)
	ReceiveFunc func(ctx context.Context, call Call, out Sender) error
	DuplexFunc  func(ctx context.Context, call Call, ch Duplex) error
}

func (h HandlerFuncs) Unary(ctx context.Context, call Call) ([]byte, error) {
	if h.UnaryFunc == nil {
		return UnimplementedHandler{}.Unary(ctx, call)
	}
	return h.UnaryFunc(ctx, call)
}

func (h HandlerFuncs) Receive(ctx context.Context, call Call, out Sender) error {
	if h.ReceiveFunc == nil {
		return UnimplementedHandler{}.Receive(ctx, call, out)
	}
	return h.ReceiveFunc(ctx, call, out)
}

func (h HandlerFuncs) Duplex(ctx context.Context, call Call, ch Duplex) error {
	if h.DuplexFunc == nil {
		return UnimplementedHandler{}.Duplex(ctx, call, ch)
	}
	return h.DuplexFunc(ctx, call, ch)
}
