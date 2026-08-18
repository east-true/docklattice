// Package contract는 부록 A.4의 "공통 최소 구현 범위"다.
//
// Method 이름, Exchange 형태, 트래픽 클래스의 대응이 여기서 한 번만 정의되고
// 두 후보가 이것을 그대로 쓴다. 이 목록 밖의 기능은 구현하지 않는다.
package contract

import "github.com/east-true/dockpilot/internal/transport"

// 부록 A.4의 Method 이름이다. 전송 기술에 따라 달라지지 않는다.
const (
	// P0 Control
	MethodRegister          transport.Method = "Register"
	MethodHeartbeat         transport.Method = "Heartbeat"
	MethodCancelOperation   transport.Method = "CancelOperation"
	MethodOperationProgress transport.Method = "OperationProgress"

	// P1 Durable Sync
	MethodSyncAudit        transport.Method = "SyncAudit"
	MethodGetAuditCoverage transport.Method = "GetAuditCoverage"

	// P2 Interactive Query
	MethodEcho transport.Method = "Echo"

	// P3 Bulk Interactive
	MethodStreamLogs      transport.Method = "StreamLogs"
	MethodOperationOutput transport.Method = "OperationOutput"

	// P4 Disposable Live
	MethodStreamStats transport.Method = "StreamStats"
)

// Spec은 한 Method의 전송 계약이다.
type Spec struct {
	Method transport.Method
	Kind   transport.Kind
	Class  transport.Class
}

// specs는 Method -> Spec 대응이다. 이 표가 유일한 권위다.
var specs = map[transport.Method]Spec{
	MethodRegister:          {MethodRegister, transport.KindUnary, transport.ClassControl},
	MethodHeartbeat:         {MethodHeartbeat, transport.KindUnary, transport.ClassControl},
	MethodCancelOperation:   {MethodCancelOperation, transport.KindUnary, transport.ClassControl},
	MethodOperationProgress: {MethodOperationProgress, transport.KindReceive, transport.ClassControl},

	MethodSyncAudit:        {MethodSyncAudit, transport.KindDuplex, transport.ClassDurable},
	MethodGetAuditCoverage: {MethodGetAuditCoverage, transport.KindUnary, transport.ClassDurable},

	MethodEcho: {MethodEcho, transport.KindUnary, transport.ClassQuery},

	MethodStreamLogs:      {MethodStreamLogs, transport.KindReceive, transport.ClassBulk},
	MethodOperationOutput: {MethodOperationOutput, transport.KindReceive, transport.ClassBulk},

	MethodStreamStats: {MethodStreamStats, transport.KindReceive, transport.ClassLive},
}

// SpecOf는 Method의 Spec을 반환한다.
func SpecOf(m transport.Method) (Spec, bool) {
	s, ok := specs[m]
	return s, ok
}

// Methods는 정의된 모든 Method를 반환한다(순서 비보장).
func Methods() []transport.Method {
	out := make([]transport.Method, 0, len(specs))
	for m := range specs {
		out = append(out, m)
	}
	return out
}

// call은 Spec에 맞는 Call을 만든다.
func (s Spec) call(payload []byte) transport.Call {
	return transport.Call{Method: s.Method, Class: s.Class, Payload: payload}
}
