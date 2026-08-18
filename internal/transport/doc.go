// Package transport는 Dockpilot Server-Agent 통신의 transport-neutral 논리 계약이다.
//
// 이 패키지는 부록 A.4의 "공통 계약"에 해당한다. 후보 A(Reverse gRPC)와
// 후보 B(WebSocket + Application Multiplexing)는 이 패키지의 인터페이스를
// 구현하며, 코드는 이 인터페이스 뒤에서만 갈라진다.
//
// # 담는 개념
//
//   - Session          : Agent가 시작한 지속 연결 위에 수립된 논리 세션
//   - Exchange         : 하나의 논리적 요청-응답 또는 스트림 단위
//   - Logical Message  : Exchange 위를 흐르는 순서 보장된 바이트 메시지
//   - Traffic Class    : P0~P4 우선순위 클래스
//   - Cancellation     : Exchange 취소와 그 전파 의미
//
// # 담지 않는 개념
//
// HTTP/2 stream, WebSocket frame, header/trailer, HTTP status, window size,
// credit counter, stream weight, channel number, gRPC metadata,
// transport-specific error code는 이 패키지에 등장해서는 안 된다.
// 이것들은 각 Transport Adapter 내부 구현이다.
//
// Backpressure도 특정 메커니즘으로 표현하지 않는다. 이 계약은 결과만 요구한다.
//
//  1. Logical Stream별 순서 보장
//  2. Bounded Memory
//  3. 느린 Stream이 다른 Stream을 막지 않음
//  4. Cancel 전파
//  5. Stream 종료 후 Resource 회수
//  6. P0/P1 Non-starvation
//  7. Terminal Outcome 정확히 한 번 관찰
//
// # 방향
//
// 연결은 Agent가 시작하고 Agent는 inbound port를 열지 않는다(ADR §5.1).
// 반면 Exchange는 Server가 시작하고 Agent가 응답한다. 즉 연결 방향과
// 요청 방향이 반대다. 이 패키지에서 Server 측은 Caller, Agent 측은
// Responder로 부른다.
//
//	Agent  --- 연결 수립(outbound) --->  Server
//	Agent  <-- Exchange 개시(Caller) --  Server
//
// Agent -> Server 방향의 데이터(audit record, log line, stats sample,
// operation progress)는 Server가 연 Exchange 위에서 Responder가 Send하는
// 형태로 흐른다.
package transport
