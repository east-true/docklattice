package transport

import (
	"context"
	"errors"
	"fmt"
)

// Code는 transport-neutral 오류 분류다. 각 Adapter는 자신의 오류를 반드시
// 이 코드 중 하나로 환산해서 반환한다. 상위 계층에 HTTP status나 gRPC code,
// WebSocket close code가 새어 나가면 안 된다.
type Code uint8

const (
	// CodeOK는 정상 종료다.
	CodeOK Code = iota

	// CodeCanceled는 Caller 또는 Responder의 명시적 취소로 종료됐다.
	CodeCanceled

	// CodeDeadlineExceeded는 Exchange의 deadline이 지났다.
	CodeDeadlineExceeded

	// CodeUnavailable은 Session이 끊겨 Exchange를 계속할 수 없다.
	// Operation의 실패를 뜻하지 않는다(ADR §9.1: transport disconnect는
	// Operation cancel이 아니다).
	CodeUnavailable

	// CodeMessageTooLarge는 메시지가 상한을 넘었다. 조용한 절단이 아니라
	// 명시적 거부여야 한다(계약 C8).
	CodeMessageTooLarge

	// CodeProtocol은 프로토콜 위반이다. 알 수 없는 프레임, 잘못된 순서,
	// 미협상 버전 등.
	CodeProtocol

	// CodeUnimplemented는 Responder가 해당 Method를 모른다.
	CodeUnimplemented

	// CodeResourceExhausted는 수용 한계(동시 Exchange 수 등)에 도달했다.
	CodeResourceExhausted

	// CodeInternal은 위 어디에도 속하지 않는 구현 오류다.
	CodeInternal
)

func (c Code) String() string {
	switch c {
	case CodeOK:
		return "ok"
	case CodeCanceled:
		return "canceled"
	case CodeDeadlineExceeded:
		return "deadline_exceeded"
	case CodeUnavailable:
		return "unavailable"
	case CodeMessageTooLarge:
		return "message_too_large"
	case CodeProtocol:
		return "protocol"
	case CodeUnimplemented:
		return "unimplemented"
	case CodeResourceExhausted:
		return "resource_exhausted"
	case CodeInternal:
		return "internal"
	default:
		return "unknown"
	}
}

// Status는 Exchange의 종료 사유다.
type Status struct {
	Code   Code
	Reason string
	// Cause는 진단용 원본 오류다. 비교나 분기의 근거로 쓰지 않는다.
	Cause error
}

// Err는 Status를 error로 변환한다. CodeOK면 nil이다.
func (s Status) Err() error {
	if s.Code == CodeOK {
		return nil
	}
	return &StatusError{Status: s}
}

func (s Status) String() string {
	if s.Reason == "" {
		return s.Code.String()
	}
	return s.Code.String() + ": " + s.Reason
}

// StatusError는 Status를 운반하는 error다.
type StatusError struct {
	Status Status
}

func (e *StatusError) Error() string { return e.Status.String() }
func (e *StatusError) Unwrap() error { return e.Status.Cause }

// Errorf는 주어진 코드의 StatusError를 만든다.
func Errorf(code Code, format string, args ...any) error {
	return &StatusError{Status: Status{Code: code, Reason: fmt.Sprintf(format, args...)}}
}

// Wrap은 원본 오류를 보존한 채 코드를 부여한다.
func Wrap(code Code, cause error, format string, args ...any) error {
	return &StatusError{Status: Status{
		Code:   code,
		Reason: fmt.Sprintf(format, args...),
		Cause:  cause,
	}}
}

// StatusOf는 error에서 Status를 뽑는다. nil이면 CodeOK,
// StatusError가 아니면 CodeInternal로 환산한다.
func StatusOf(err error) Status {
	if err == nil {
		return Status{Code: CodeOK}
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	if errors.Is(err, context.Canceled) {
		return Status{Code: CodeCanceled, Reason: err.Error(), Cause: err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Status{Code: CodeDeadlineExceeded, Reason: err.Error(), Cause: err}
	}
	return Status{Code: CodeInternal, Reason: err.Error(), Cause: err}
}

// Outcome은 Exchange의 terminal outcome이다. 계약상 Exchange마다
// 정확히 한 번 관찰 가능해야 한다.
type Outcome struct {
	Status Status
	// Messages는 해당 Exchange에서 관찰한 논리 메시지 수다(방향 무관 아님:
	// 관찰 주체가 수신한 메시지 수).
	Messages uint64
	// Bytes는 그 메시지들의 payload 합계다.
	Bytes uint64
}
