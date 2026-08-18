package transport

// Class는 ADR §5.2의 트래픽 클래스다. 전송 기술과 무관하게 우선순위 정책은
// 애플리케이션 계층이 소유한다. 두 후보 모두 같은 스케줄링 정책을 만족해야
// 비교가 공정하다.
type Class uint8

const (
	// ClassControl(P0)은 cancel, heartbeat, operation phase/final result,
	// protocol error를 운반한다.
	ClassControl Class = iota

	// ClassDurable(P1)은 audit WAL sync와 operation result recovery를 운반한다.
	ClassDurable

	// ClassQuery(P2)는 Docker/Compose query와 file read 같은 대화형 조회다.
	ClassQuery

	// ClassBulk(P3)은 logs와 compose stdout/stderr live relay다.
	ClassBulk

	// ClassLive(P4)는 stats다. latest-wins이며 backlog를 누적하지 않는다.
	ClassLive

	// numClasses는 클래스 개수다. 스케줄러 배열 크기에 쓴다.
	numClasses = iota
)

// NumClasses는 정의된 트래픽 클래스 개수다.
const NumClasses = numClasses

// Priority는 낮을수록 높은 우선순위다. P0가 가장 높다.
func (c Class) Priority() int { return int(c) }

// IsProtected는 굶주려서는 안 되는 클래스인지 여부다. ADR §5.2:
// "P0/P1은 P3/P4에 굶으면 안 된다."
func (c Class) IsProtected() bool { return c == ClassControl || c == ClassDurable }

// IsLatestWins는 backlog를 누적하지 않고 오래된 샘플을 폐기해야 하는
// 클래스인지 여부다.
func (c Class) IsLatestWins() bool { return c == ClassLive }

func (c Class) String() string {
	switch c {
	case ClassControl:
		return "P0.control"
	case ClassDurable:
		return "P1.durable"
	case ClassQuery:
		return "P2.query"
	case ClassBulk:
		return "P3.bulk"
	case ClassLive:
		return "P4.live"
	default:
		return "unknown"
	}
}

// Valid는 정의된 클래스인지 확인한다.
func (c Class) Valid() bool { return c < numClasses }
