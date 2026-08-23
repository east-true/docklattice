# Dockpilot 아키텍처 결정 기록

**상태**: 동결 (Frozen)
**동결일**: 2026-08-13
**다음 단계**: 운영 기본값 실제 튜닝과 구현 계획 수립 (부록 B)

---

## 0. 이 문서에 대하여

이 문서는 Dockpilot의 구현 착수 전 설계 결정을 통합한 기록이다. 설계 논의는 시간순으로 누적되었으나 이 문서는 주제별로 재구성했다.

이 문서의 지위:

- **설계 권위**다. 구현 중 구조 변경이 필요한지 판단하는 기준이 된다.
- 프로토타입 이후 다시 열지 않는 결정은 §2에 불변식으로 명시했다.
- 프로토타입 결과로 확정되는 것은 전송 기술과 일부 수치뿐이다.

용어 분류 규칙: 새 기능을 논할 때는 반드시 `CORE` / `OPTIONAL` / `FUTURE` / `DO NOT BUILD` 중 하나로 분류하고, 그 전에 **"Docker가 이미 이 기능을 제공하는가?"** 를 먼저 묻는다.

---

## 1. 프로젝트 정의와 범위

Dockpilot은 여러 Docker 호스트를 중앙에서 조회·제어하고, Docker Compose 기능을 GUI로 수행하며, 감사 이력·실시간 로그·실시간 메트릭·기본 백업을 제공하는 **경량 내부망용 Docker Control Plane**이다.

```
Browser UI
    |
Dockpilot Server          (Control Plane)
    |
    | Agent-initiated persistent connection
    |
    +-- Dockpilot Agent A  (Local Controller)
    |       +-- Docker Engine
    |       +-- Docker Compose CLI
    |       +-- Local Compose Files
    |
    +-- Dockpilot Agent B
            ...
```

하나의 프로젝트, 하나의 바이너리, 두 실행 모드(`dockpilot server` / `dockpilot agent`).

### 1.1 핵심 철학

> Docker가 이미 제공하는 기능은 Docker에 맡기고, Dockpilot은 여러 Docker 호스트를 연결하고 관리하는 얇은 중앙 제어 계층만 제공한다.

목표는 기능이 많은 Docker 플랫폼이 아니라, **Docker + Dockpilot만 있으면 여러 Docker Host의 자주 쓰는 운영 작업을 중앙 GUI에서 안정적으로 수행할 수 있는 작고 명확한 Control Plane**이다.

### 1.2 런타임 의존성 원칙

운영 환경에서 요구하는 것은 다음뿐이다.

```
Docker Host   : Docker + Dockpilot Agent
Central Host  : Dockpilot Server
```

Prometheus, Grafana, Loki, Redis, PostgreSQL/MySQL, Kafka, NATS, 별도 로그 수집 daemon, 별도 백업 daemon, 외부 SaaS, 별도 인증 서버 — 그 무엇도 설치를 요구하지 않는다.

빌드 시점 의존성(Go 라이브러리, Docker 공식 SDK, embedded DB 라이브러리, 프론트엔드 빌드 의존성)은 허용한다. 중요한 것은 **운영자가 Docker와 Dockpilot 외의 서비스를 설치하지 않아도 되는 것**이다.

### 1.3 의도적으로 만들지 않는 것

Kubernetes, Docker Swarm orchestration, 범용 SSH terminal, arbitrary shell execution, CI 파이프라인 에디터, 이미지 빌드 플랫폼, Git 저장소 관리, Prometheus/Grafana/Loki 대체, enterprise RBAC, 알림 플랫폼, 스케줄러, 자동 self-healing, application-aware DB 백업 엔진, 메트릭 시계열 플랫폼.

**복잡한 작업은 SSH로 처리한다.**

### 1.4 라이선스

Apache License 2.0.

의존성 검토 결과:

| 구성요소 | 라이선스 | 판정 |
|---|---|---|
| `docker/docker` (moby) Go SDK | Apache-2.0 | 채택 |
| `docker/compose` (CLI 위임, 링크 아님) | Apache-2.0 | 채택 |
| `grpc-go` | Apache-2.0 | 후보 A에서 채택 |
| `google.golang.org/protobuf` | BSD-3-Clause | 채택 |
| `modernc.org/sqlite` (cgo-free) | BSD-3-Clause | 채택 (cross-compile 용이) |
| `mattn/go-sqlite3` | MIT | 비채택 (cgo 필요) |
| `hashicorp/yamux` | MPL-2.0 | 회피 |
| React / Vite / TypeScript | MIT | 채택 |

- GPL/AGPL 및 LGPL 동적 링크 회피 (단일 정적 바이너리 배포 모델에서 재링크 의무가 실질적 문제)
- `LICENSE` 동봉, `NOTICE` 유지·전파, 수정 파일 변경 표기
- `go-licenses` 등으로 의존성 라이선스 목록을 CI에서 자동 생성하고 릴리스 아티팩트에 포함. **frontend 의존성도 포함**(바이너리에 embed되므로 배포물의 일부)
- Agent 이미지에 docker CLI + compose plugin을 번들하므로 이미지에 `/licenses/` 포함
- 상표: 프로젝트명·로고에 "Docker"를 포함하지 않는다. README에 *"Dockpilot is not affiliated with or endorsed by Docker, Inc."* 명시. Docker 고래 로고 사용 금지.

---

## 2. 아키텍처 불변식

프로토타입 이후에도 다시 열지 않는 결정이다.

| 대상 | 권위 |
|---|---|
| Docker 현재 상태 (container/image/network/volume/health) | **Docker Engine** |
| Compose 설정 파일 내용 | **Host Filesystem** |
| Operation 실행 및 Project Lock | **Dockpilot Agent** |
| 미동기 Audit | **Agent bounded disk WAL** |
| 동기화된 Audit과 Coverage | **Server canonical archive + Coverage Ledger** |
| Server-Agent 통신 신뢰 | **Server Identity State** |

한 줄 요약: **실행의 권위는 Agent, 기록의 권위는 Server.**

추가 불변식:

- Docker Runtime 상태를 Server DB에 복제하지 않는다.
- Compose 파일 내용을 Server DB에 저장하지 않는다.
- Metrics는 viewer-scoped live relay이며 history를 저장하지 않는다.
- Logs는 live relay이며 중앙 로그 저장소를 만들지 않는다.
- 일반 File Write는 파일 1개 단위다.
- Multi-file Transaction은 Configuration Restore에만 허용한다.
- Cancel은 Rollback이 아니다.
- Browser disconnect와 Transport disconnect는 Operation cancel이 아니다.
- Timeout은 CancelOperation과 동일 경로를 쓰며, commit 이후 데이터 정합성을 깨는 강제 종료를 하지 않는다.
- Server 용량 압박을 이유로 Agent Audit ingest를 거부하지 않는다.
- Project Lock force-release API를 만들지 않는다.
- Agent disk pressure에서 수동 Backup을 자동 삭제하지 않는다.
- Docker/Compose가 제공하는 기능을 재구현하지 않는다.

---

## 3. 배포 모델

### 3.1 Container Agent 단독 (v1 공식)

v1 공식 배포 방식은 Container Agent 하나다. Native 바이너리는 릴리스에 포함하되 문서화하지 않으며 테스트 행렬에서도 제외한다(best-effort).

Container를 단독 채택하는 실질 이득: **compose 버전이 이미지에 고정되므로 host compose 버전 탐지·호환 코드가 통째로 사라진다.**

표준 배포 형태:

```
mounts:
  /var/run/docker.sock  -> /var/run/docker.sock       (rw, 필수)
  /var/lib/dockpilot    -> /var/lib/dockpilot         (rw, agent state)
  <discovery_root_N>    -> <동일 절대경로>              (rw = 편집 허용, ro = 조회 전용)
network: bridge (Server로의 outbound만 필요)
restart: unless-stopped
labels:
  io.dockpilot.role: agent
container_name: dockpilot-agent
```

- Agent는 절대경로를 받지 않는다. Server가 보내는 것은 `project_uid + relative_path`뿐이다.
- discovery root를 `ro`로 마운트하면 `fs_write: false`를 보고하고 UI가 편집을 비활성화한다. **read-only 운영은 1급 지원 모드**다.
- state dir은 named volume도 허용하지만 **discovery root는 반드시 identical path bind mount**여야 한다.
- 설치 가이드는 Agent를 관리 대상 Compose 프로젝트와 분리하도록 권장한다.

v1 미지원 환경(명시적 선언): rootless Docker의 비표준 socket 경로, `DOCKER_HOST`를 통한 TCP/socket-proxy 접속, 컨테이너 실행이 정책상 금지된 host.

Agent 업그레이드는 self-protection이 자기 컨테이너 교체를 막으므로 **host에서 수행**한다. 설치 문서에 절차로 포함한다.

### 3.2 Path Identity Self-Check (CORE)

Agent 컨테이너 안에서 compose를 실행하면 compose는 컨테이너에서 본 경로로 bind mount의 상대경로를 절대경로로 확장하고, daemon은 그것을 **host 기준**으로 해석한다. 따라서 identical path mapping은 권장이 아니라 **필수 불변조건**이며 부팅 시 검증한다.

```
1. discovery root를 포함하는 mount를 찾는다
   → Destination이 prefix인 것 중 가장 긴 것 (most-specific wins)
2. mount의 Type이 bind인지 확인 (volume/tmpfs면 실패)
3. host_path = mount.Source + (root - mount.Destination)
4. host_path == root 인지 비교
5. 어떤 mount에도 포함되지 않으면 실패
   (컨테이너 자체 레이어에 있다는 뜻 — 쓰기가 호스트에 반영되지 않는다)
```

5번이 가장 흔한 오설정(마운트 누락)이다. 조용히 성공한 것처럼 보이다가 compose가 빈 디렉터리를 보게 되므로 반드시 잡는다.

검증 실패 시 해당 root를 **조회 전용으로 강등**하고 `fs_write:false`, `compose_exec:false`를 보고한다.

### 3.3 Agent Self-Protection (CORE)

UI에서 Agent 자신을 stop/remove하면 자살하고 진행 중인 operation도 함께 죽는다.

자기 식별 순서:

```
1순위: label io.dockpilot.role=agent 로 조회
       (컨테이너 ID는 생성 후에 확정되므로 env 주입은 부적절)
2순위: 설정값 self_container_id / self_container_name
둘 다 실패: fail-closed
```

한 호스트에서 여러 개가 발견되면 모두 보호 대상으로 처리한다. 다른 컨테이너가 label을 잘못 써도 불필요하게 보호될 뿐 위험한 권한이 생기지 않는다.

자기 컨테이너를 찾은 뒤 `com.docker.compose.project` label을 읽어 **Agent가 속한 Compose 프로젝트 전체**를 보호한다.

fail-closed 시 허용/차단:

```
허용: 조회, 로그, 메트릭
차단: container stop/restart/remove, compose down,
      Agent 자신을 재생성할 가능성이 있는 변경 작업
```

Path Identity 검사는 자기 컨테이너의 `Mounts[]`를 읽어야 하므로 **self-identification에 의존한다.** 둘은 하나의 실패 모드로 묶인다.

---

## 4. 책임 경계

| 관심사 | Docker | Agent | Server | UI |
|---|---|---|---|---|
| container/image/network/volume 상태 | **소유** | 통과(adapter) | 통과(relay) | 표시 |
| container 제어 | **실행** | 호출 | 요청 | 트리거 |
| compose 실행 | **실행(CLI)** | invoke + 출력 스트림 | 요청/relay | 트리거 |
| compose 파일 내용 | filesystem이 소유 | read/atomic write/validate | **저장 안 함** | 편집기 |
| project 존재 사실(discovery) | label 일부 제공 | **raw 사실 수집** | **정규화·identity·merge** | 목록 |
| operation 실행/취소/lock | — | **권위** | 미러 + 요청 발행 | 상태 표시 |
| audit 기록 | events 신호 제공 | 생성 + WAL 버퍼 | **canonical store** | 통합 조회 |
| metrics | **소유** | 구독/통과 | relay(무저장) | 렌더 + 클라이언트 버퍼 |
| backup 바이트 | — | **소유(local)** | 메타데이터 인덱스 | 목록/복원 트리거 |
| agent registry / credential | — | 자기 자격증명 | **소유** | — |

Agent에 "소유"가 붙은 것은 backup 바이트와 operation 실행뿐이다. 나머지는 통과 아니면 Server다.

Agent에서 판단 로직이 있는 곳은 **Project Lock**(실행 주체이므로 필연)과 **Safe File Operations**(신뢰 경계이므로 필연) 두 곳뿐이다.

---

## 5. Server-Agent 통신

### 5.1 방향과 계약

연결은 Agent가 시작하고, Agent는 inbound port를 열지 않는다. NAT/방화벽 뒤 호스트 지원, IP 변동 무관, 공격면 축소가 근거다.

전송 기술은 2026-08-15 Transport Prototype 결과에 따라 **단일 Agent-initiated Reverse gRPC**로 확정했다. Candidate A는 13/13 acceptance group을 통과했고, Candidate B(WebSocket)는 Scenario 3 scale workload group이 1/3 통과에 그쳐 탈락했다. 적어도 한 단일 연결 후보가 합격했으므로 §5.3의 두 연결 후퇴점은 활성화하지 않는다. 공식 원시 결과와 판정 리포트는 `artifacts/transport-prototype/official`에 보존한다.

WSL 업데이트로 공식 행렬의 마지막 WebSocket 3개 trial 전에 커널이 `6.6.87.2`에서 `6.18.33.2`로 변경됐다. 이는 A.7의 동일 커널 통제조건 예외다. 결과를 폐기하고 전체 행렬을 재실행하는 대신 이 예외를 명시적으로 수용했으며, 최종 선택을 결정한 WebSocket scale 실패 2회와 gRPC 합격 증거는 업데이트 전에 수집됐다.

**전송이 만족해야 할 계약은 다음과 같이 확정되어 있다.**

```
C1. Agent-initiated 단일 지속 연결, Agent에 inbound port 없음
C2. 논리 스트림 다중화 — 느린 로그 스트림이 operation 요청을 막지 않을 것 (HOL blocking 금지)
C3. 스트림 단위 취소 — Browser → Server → Agent → Docker 까지 전파
    (단, 변경 Operation은 §9.5의 예외 적용)
C4. 스트림 단위 backpressure — 소비자가 느리면 생산이 느려질 것.
    Agent/Server 어디에도 무한 버퍼가 없을 것
C5. 요청-응답 상관관계 id + operation_id 기반 Agent측 멱등성
C6. audit 동기화는 순서 보장 + 커서 기반 재개
C7. 재연결은 backoff+jitter. 로그/stats 스트림은 자동 재개하지 않음.
    operation 결과는 id로 회수 가능
C8. 메시지 크기 상한. 초과 시 조용한 절단이 아니라 명시적 거부
C9. 연결 시 프로토콜 버전 협상, Server N ↔ Agent N-1 호환
```

### 5.2 트래픽 클래스

전송 기술과 무관하게 우선순위 정책은 **애플리케이션 계층이 소유**한다. Reverse gRPC가 HTTP/2를 사용하더라도 스트림 우선순위는 얻지 못하므로, 선택된 adapter도 프로토타입에서 검증한 공통 P0~P4 스케줄링 정책을 사용한다.

```
P0 — Control            : cancel, heartbeat, operation phase/final result, protocol error
P1 — Durable Sync       : audit WAL sync, operation result recovery
P2 — Interactive Query  : Docker/Compose query, file read
P3 — Bulk Interactive   : logs, compose stdout/stderr live relay
P4 — Disposable Live    : stats
```

정책:

- P0/P1은 P3/P4에 굶으면 안 된다. Audit Sync는 지속적으로 cursor가 전진해야 한다.
- **Logs**: stream별 byte-rate 상한, 느린 소비자에 bounded buffering, 필요 시 dropped bytes/lines 명시 또는 stream 종료.
- **Stats**: latest-wins. 오래된 sample 폐기, backlog 누적 금지.
- **Operation stdout/stderr**: CLI output은 **항상 drain**한다. UI 전송이 느려도 CLI 프로세스를 막지 않는다. Agent에 bounded output tail 저장, 중간 출력 생략 시 `truncated=true`.

Compose 프로세스의 stdout 파이프가 UI 소비 속도 때문에 막히면 Operation 자체가 멈추므로, Operation 출력에 일반적인 backpressure를 그대로 적용해서는 안 된다.

### 5.3 후퇴점: 두 연결 구조

단일 연결이 프로토타입 수용 기준을 통과하지 못하면 **버퍼를 늘려 실패를 숨기지 않고** 두 연결로 후퇴한다.

Transport Prototype에서는 Reverse gRPC 단일 연결이 합격했으므로 이 후퇴점은 **비활성**이다. 아래 계약은 후퇴 조건이 실제로 충족될 때만 적용한다.

```
Connection A — Control / Durable
  Registration, Heartbeat, Cancel, Operation Phase,
  Operation Final Result, Audit Sync, Operation Result Recovery

Connection B — Bulk / Disposable
  Logs, Compose stdout/stderr Live Relay, Stats
```

공통 조건: 둘 다 Agent-initiated, Agent inbound port 없음, 동일 Agent Credential, 독립 reconnect/backoff.

**Session 결속:**

Connection A가 credential 검증 → agent_id 확인 → 프로토콜 버전 협상 → 기존 session 정리 → 새 session 생성을 마친 뒤 Server가 `session_id`를 발급한다.

```
session_id: 최소 128-bit cryptographically random opaque value
            재연결마다 새로 발급, 영속 저장하지 않음
            현재 Connection A에만 유효
```

Connection B는 `Agent Credential + agent_id + session_id + channel_type=BULK`를 제시한다. Server는 해당 session의 A가 ACTIVE인지, credential의 agent_id가 일치하는지, 이미 다른 Bulk 연결이 결속돼 있지 않은지 검증한다.

생명주기:

```
A 종료 → session_id 무효화 → B 종료 (B 단독 생존 금지)
B 종료 → A 유지, Operation 실행 유지, Audit Sync 유지
        Logs/Compose live output/Stats만 종료
        bounded Operation Output Tail은 Agent에 유지
B는 A 없이 먼저 수립할 수 없고, Control Message나 Operation Result를 운반하지 않는다
```

---

## 6. Identity, Credential, Archive

사용자 인증(로그인/RBAC)과 Server-Agent 통신 신뢰는 별개 문제다. 전자는 만들지 않고, 후자는 최소한으로 만든다.

### 6.1 Server Identity State

Audit DB와 **분리된** 영속 상태다. 백업 대상이 정확히 둘로 갈라진다.

```
Server Identity State
├─ server_identity_id          (Server 설치의 장기 identity)
├─ signing keys
│  ├─ current key_id
│  └─ private/public key material
├─ archive_generation          (Archive 생성마다 1 증가, DB가 아니라 여기가 권위)
├─ credential revocation ledger
└─ identity state format version
```

```
백업 대상 1: Server Audit / Operational Database
백업 대상 2: Server Identity State   ← 일반 설정 백업보다 강하게 보호
```

복구 결과가 다르다:

```
Audit DB 손실 + Identity State 유지
  → 기존 Agent 자동 인증 → 새 archive_generation → 자동 Archive Rebind

Identity State 손실
  → 다른 server_identity_id → 기존 Agent가 새 Server로 판단 → 수동 재등록
```

### 6.2 Agent Credential

```
Agent Credential
- version
- server_identity_id
- agent_id
- credential_id
- key_id
- issued_at
- expires_at
- signature          (Server Signing Key로 서명)
```

검증 흐름:

```
TLS Server Identity 확인
→ Credential 서명 검증
→ server_identity_id 확인
→ expires_at 확인
→ credential_id 폐기 여부 확인
```

DB 조회 없이 검증되므로 Audit DB가 비어 있어도 기존 Agent를 인증하고 Agent Registry를 재구성할 수 있다.

**Revocation Ledger**는 Audit Archive의 일부가 아니라 Server-Agent 신뢰 상태이므로 Server Identity State에 둔다. Audit DB에만 두면 재구축 후 폐기된 credential이 되살아난다.

```
credential_id / agent_id / revoked_at / expires_at / reason
```

- 폐기 성공을 반환하기 전에 Revocation Ledger를 영속화하고 `fsync`
- credential 만료 시각이 충분히 지난 폐기 항목은 압축·삭제 가능 (만료가 있으므로 목록이 무한히 자라지 않는다)
- Audit DB 재구축과 무관하게 폐기 상태 유지
- Server Identity State 복원은 Audit DB 복원과 **별도 운영 절차**

### 6.3 Credential 자동 갱신 (CORE)

`expires_at`만 넣고 갱신을 만들지 않으면 전 Agent가 동시에 만료된다.

```
만료 임박
→ 현재 유효 Credential로 연결
→ Agent가 Renewal 요청
→ Server가 새 credential_id로 발급
→ Agent가 임시 파일 저장 → fsync → atomic rename
→ 새 Credential 활성화 보고
→ 그 뒤에 이전 Credential 폐기 또는 짧은 중첩 기간 후 만료
```

활성화 보고 **전에** 이전 credential을 폐기하면 Agent가 저장에 실패했을 때 인증 수단이 사라진다.

```
Credential Lifetime      : 유한
Automatic Renewal        : CORE
Expired while offline    : Join Token을 통한 재등록 필요
key_id                   : v1 credential에 포함, Key Rotation 기능 자체는 FUTURE
```

### 6.4 Archive Identity 3계층

```
server_identity_id   = Server 설치와 신뢰 identity
archive_generation   = 동일 Server Identity 아래 Archive 세대 (단조 증가)
audit_archive_id     = 특정 Archive 인스턴스의 Opaque UUID
```

Agent가 저장하는 binding:

```
bound_server_identity_id
bound_archive_generation
bound_audit_archive_id
archive_acked_through
retired archive/generation 정보
```

판정:

| 조건 | 결과 |
|---|---|
| 같은 identity + 같은 generation + 같은 archive_id | 정상 재연결 |
| 같은 identity + 더 높은 generation + 새 archive_id | **자동 Archive Rebind** |
| 같은 identity + 더 낮은 generation | `ARCHIVE_ROLLBACK_DETECTED` → 거부 |
| 같은 identity + 같은 generation + 다른 archive_id | `AUDIT_ARCHIVE_IDENTITY_INVARIANT_VIOLATION` → 거부 |
| 다른 identity | Archive Rebind가 아니라 Server 재등록 문제 |

generation이 없으면 폐기된 과거 Archive가 재등장했을 때 A↔B 왕복 rebind가 성립한다. `SERVER_CURSOR_REGRESSION`은 **동일 identity·generation·archive_id 안에서 DB가 과거 상태로 복원된 경우에만** 사용한다.

### 6.5 Archive Rebind

중앙 Server를 새 Archive로 재구축할 때 Host마다 수동 조작하게 해서는 안 되므로 자동화한다. WAL 절삭은 항상 ACK를 따르고 새 Archive는 wal_floor부터 읽으므로, 자동 Rebind 자체가 새 유실을 만들지 않는다.

Agent 상태 전환:

```
previous_archive: { generation, archive_id, acked_through }
new_archive:      { generation, archive_id, coverage_begins_at, acked_through: null }

WAL 비어 있지 않음 → coverage_begins_at = wal_floor
WAL 비어 있음      → coverage_begins_at = next_cursor
```

새 Archive의 ACK Watermark는 이전 Archive의 ACK를 물려받지 않는다. 이미 삭제된 Record는 복구할 수 없으며 Server가 `SERVER_COVERAGE_START (reason=NEW_AUDIT_ARCHIVE)`로 기록한다.

Agent는 in-band WAL record를 남긴다:

```
ARCHIVE_REBOUND:
  server_identity_id, previous_archive_generation, previous_archive_id,
  new_archive_generation, new_archive_id, wal_floor_at_rebind, rebound_at
```

동일 generation·archive_id로 rebind 요청이 반복되면 새 record를 생성하지 않고 완료된 binding 상태를 반환한다(멱등).

차단은 프로토콜이 아니라 UI 경고로 처리한다. 프로토콜 차단은 "동일 archive_id에서 ACK Cursor가 이전 Watermark보다 낮음" 하나뿐이다.

### 6.6 UI 접근

```
Server ↔ Agent trust          = CORE     (join token, agent credential, Server identity 확인)
Browser ↔ Server access token = OPTIONAL (배포 환경에서 활성화)
사용자 계정 / 로그인 / 세션 / 역할 = 만들지 않음
```

토큰이 기본 비활성이므로 **Server 기본 bind 주소는 `127.0.0.1`**, `0.0.0.0` 바인드는 명시적 opt-in으로 두고 기동 로그에 경고를 남긴다. 이는 인증 기능이 아니라 기본값 선택이다.

mTLS / 자체 CA / 인증서 로테이션은 v1에서 **DO NOT BUILD**.

---

## 7. Discovery와 Project Identity

### 7.1 두 소스, 하나의 정규화

```
[Docker labels]  ──┐
                   ├─→ Agent: raw 사실만 보고 ──→ Server: 정규화 / identity / merge
[Filesystem scan]──┘
```

Agent가 보고하는 raw 사실:

- **Docker측**: 공개 계약인 `com.docker.compose.project`, `.service`와, 진단용
  구현 라벨 `.working_dir`, `.config_files`, `.config-hash`
- **FS측**: discovery root 하위 compose 파일의 canonical 절대경로, 파일별 `(size, mtime, sha256)`, `docker compose config`로 얻은 project name

merge/identity 판단을 Server로 올려 Agent 로직을 줄이고, 여러 Agent에 걸친 판단(같은 name이 여러 host에 존재)을 가능하게 한다.

### 7.2 Scan 정책

```
discovery roots (명시적 지정)
+ 깊이 제한 없는 재귀 탐색
+ ignore 규칙
+ scan budget
```

`max_depth`를 강제하지 않는 이유: root를 관리자가 명시했고, 디렉터리 깊이는 환경마다 다르며, 임의 depth 제한이 정상 프로젝트를 놓치는 것이 더 나쁘다. 대신 **scan budget**을 둔다.

한 directory에 기본 파일 후보가 여러 개 있으면 Compose의 기본 순서를 그대로
적용해 하나만 선택한다: `compose.yaml`, `compose.yml`, `docker-compose.yml`,
`docker-compose.yaml`. override도
`compose.override.yml`, `compose.override.yaml`,
`docker-compose.override.yml`, `docker-compose.override.yaml` 중 첫 파일 하나만
base 뒤에 둔다. Dockpilot은 이후 명시적 `--file` 인자를 사용하므로 이 선택과
순서를 자체적으로 보존해야 한다.

```
per-scan budget: max_dirs_visited (기본 200,000), max_duration (기본 60s)
초과 시: 스캔 중단 + 지금까지 결과 유지 + truncated=true
UI: "discovery가 예산 초과로 중단됨. root를 좁히거나 ignore를 추가하세요"
    + 마지막 방문 경로 표시
```

budget이 depth보다 우월한 이유는 **문제를 조용히 누락시키지 않고 관측 가능하게 만들기 때문**이다. `max_depth`는 OPTIONAL로 남긴다.

세부 규칙(CORE):

- **symlink 디렉터리는 따라가지 않는다.** 무한 루프와 discovery root 탈출을 동시에 막는다. symlink된 project는 root에 직접 추가하도록 안내한다.
- hidden 디렉터리는 **스캔한다**(`.deploy/`, `.docker/` 같은 실사용 패턴 존재). 기본 ignore에 `.git`을 명시적으로 포함한다.
- 기본 ignore(보수적): `.git`, `node_modules`, `vendor`, `.cache`, `.venv`, `__pycache__`, `target`, `dist`, `build`. `logs`/`tmp`/`backups`는 사용자 프로젝트 디렉터리명과 충돌 가능성이 있어 **기본 목록에서 제외**하고 사용자 설정으로 남긴다.
- 사용자 ignore는 Agent config의 glob 리스트. `.dockpilotignore` 파일 지원은 FUTURE(root 안의 파일을 신뢰하는 새 신뢰 경계가 생긴다).
- 주기: 기본 5분 + on-demand rescan + write/compose operation 직후 targeted rescan.
- I/O: 단일 goroutine, `os.ReadDir` 기반, 초당 디렉터리 상한. Agent가 host I/O를 잡아먹으면 안 된다.

### 7.3 Nested Compose Project

둘 다 발견한다. 부모가 `include:`로 자식을 참조하면 자식에
`included_by: <parent_uid>`를 마킹하고 UI 목록에서 기본 접힘 처리하되 독립
조작은 허용한다. `extends.file`은 서비스 조각 참조일 뿐 별도 Compose project의
부모-자식 관계를 만들지 않는다.

`docker compose config --format json`은 name/services의 권위 있는 결과이지만
Compose CLI v5.3.1에서 `include`/`extends`의 **원본 출처를 보존하지 않고
평탄화한다.** 따라서 Agent는 아래처럼 의도적으로 좁은 source-provenance
parser를 별도로 둔다.

- YAML AST에서 최상위 `include`와 `services.*.extends.file`의 **literal path만**
  추출한다. project name, interpolation, merge, service 선택, 최종 config는
  계산하지 않는다. 이들은 계속 Docker Compose CLI만 담당한다.
- alias/anchor/merge key, 보간 경로, 불명확 타입, 다중 document는 source graph를
  incomplete로 만든다. lifecycle의 의미를 이 parser로 거부하거나 재해석하지
  않되, graph가 incomplete인 동안에는 Compose evaluation cache를 재사용하지
  않는다.
- 재귀 탐색은 최대 64 file, 256 edge, 깊이 16으로 제한하고 cycle을 탐지해
  incomplete로 끝낸다. 그래프와 파일 내용은 Server로 보내지 않고 경로·종류·
  접근 가능 여부만 보낸다.

`include` 대상의 directory가 독립 discovery project이면 Server의 기존
filesystem merge가 `included_by`를 계산한다. 따라서 source graph는 identity를
결정하지 않고, Agent의 raw filesystem fact에 include 관계만 추가한다.

discovery boundary 마커는 **FUTURE** — 실제 요구가 확인되기 전에 만들지 않는다.

### 7.4 Project Name — 재구현 금지

compose project name 규칙은 `-p` > `COMPOSE_PROJECT_NAME` > `name:` 필드 > 디렉터리명 정규화 순이며 `.env`와 override 병합까지 얽힌다. **직접 파싱하지 않는다.**

```
docker compose -f <files> --project-directory <dir> config --format json
```

한 번으로 project name + 서비스 목록 + 병합 결과를 얻는다. **파일 sha256 조합이 바뀔 때만 재호출**하고 결과를 Agent 메모리에 캐시한다.

### 7.5 Stable Identity

```
project_uid = hash(agent_id + canonical_working_dir)
```

- `canonical_working_dir`: symlink 해석 후 절대경로, trailing slash 정규화
- **project name을 identity에 넣지 않는다.** name은 `.env` 한 줄로 바뀌고, 그러면 UI에서 project가 사라졌다 나타난다. name은 표시용 속성이다.
- working_dir이 바뀌면 새 project다(compose 입장에서도 다른 project).
- Docker측 관측을 label `working_dir`로 FS측에 접합한다. 이 구현 라벨이 없거나
  discovery 결과 밖을 가리키면 **"unmanaged compose project"** 로 표시(조회만
  가능, 파일 편집 불가)한다. 공개 `project` 이름만으로 filesystem identity를
  추정하지 않는다. 다른 stack이 같은 이름을 쓸 수 있기 때문이다. 이 상태를
  1급으로 다룬다 — 실제로 흔하다.

### 7.6 Project Name 충돌 감지 (CORE)

한 host에서 서로 다른 디렉터리가 같은 project name을 쓰면 `compose up`이 **다른 디렉터리의 컨테이너를 자기 것으로 인식해 재생성/삭제**한다. 실제 운영 사고의 단골이다.

Server가 merge 시 `(agent_id, project_name)` 중복을 검출해 양쪽 project에 경고 배지 + 변경성 operation 차단(또는 강한 확인)을 적용한다. 탐지 비용은 SQL 한 줄이고 방지 효과는 크다.

### 7.7 외부 파일 변경 감지 (CORE, polling)

목적은 캐시가 아니다. Dockpilot 밖에서 관리 대상 설정이 변경된 사실을 관측해 Audit/UI에 반영하는 것이다.

```
주기적 discovery scan
    ↓
관리 대상 project 핵심 파일의 (size, mtime, sha256) 비교
    ↓
변경 감지 → Observed Audit ("external config change", actor=unknown)
         + project에 "config changed externally" 배지
```

**fsnotify가 아니라 polling**을 쓰는 이유: project 수 × 파일 수만큼 inotify watch가 필요해 시스템 한도(`max_user_watches`)에 부딪힌다. 목적이 실시간 반응이 아니라 사후 관측이므로 5분 지연은 문제되지 않는다.

파일 내용은 저장하지 않는다. 사용자가 열 때 filesystem에서 다시 읽는다. **변경 감지가 자동 재적용을 트리거하지 않는다.**

이 기능이 CORE인 이유는 audit 때문이 아니라 **file safety 때문**이다 — §10.6의 concurrent edit detection이 같은 해시에 의존한다.

### 7.8 Config Drift Indicator (CORE)

`com.docker.compose.config-hash`를 재계산해 비교하는 접근은 **DO NOT BUILD**. 이 label은 compose 내부 구현 세부사항이지 공개 계약이 아니며, `config --hash`와 container label이 불일치하는 사례가 반복 보고됐다.

```
Tier 1 (CORE, 상시, 비용 0)
  근거: 마지막 성공한 managed compose.up이 기록한 파일 sha256 집합
        vs 현재 discovery scan의 sha256 집합
  Docker에 전혀 의존하지 않음 (§7.7의 polling이 이미 해시를 계산한다)

Tier 2 (OPTIONAL, 사용자가 [적용 미리보기]를 눌렀을 때만)
  docker compose --dry-run up -d 실행
  출력은 해석하지 않고 사용자에게 그대로 표시
```

**DO NOT BUILD**: dry-run 출력 파싱으로 in-sync 자동 판정, dry-run exit code로 변경 여부 판정, config-hash 재계산, `config --hash`와 container label 비교.

UI 문구:

```
부정확: "설정이 적용되지 않음"
정확:   "마지막 Dockpilot 적용 이후 설정 파일이 변경됨"
```

SSH에서 외부 `compose up`을 실행했는지는 알 수 없으므로 "미적용"이라고 단정하지 않는다.

상태:

```
in-sync      : applied fingerprint == current
changed      : applied fingerprint != current
no baseline  : Dockpilot 적용 이력 없음
```

`no baseline`을 `changed`로 뭉뚱그리면 Tier 1의 신뢰도가 첫날부터 떨어진다.

**저장 위치**: Server의 project 행. `compose.up` 성공 시 Server가 미러에 결과를 기록하면서 fingerprint 집합도 확정한다. 기록 대상은 선택된 base/override, project `.env`, include/extends source, service/include env file 중 Agent가 discovery root 안에서 안전하게 hash한 입력 집합이다. 해석 또는 hash가 불완전하면 Compose evaluation cache를 재사용하지 않는다.

---

## 8. Operation 모델

### 8.1 Compose 실행: CLI 완전 위임

```
Docker runtime 기능 → Docker Engine API/SDK
Docker Compose 기능 → docker compose CLI
```

`compose-go` 라이브러리를 직접 쓰지 않는다. 버전별 동작 차이를 Dockpilot이 떠안게 되고 재구현 금지 원칙에 정면으로 위배된다.

```
허용: exec docker compose ...   (고정 argv, shell 미경유, 화이트리스트된 인자)
금지: sh -c "<사용자 입력>", 범용 shell command
```

고정 argv exec는 arbitrary shell execution이 아니다. 이 구분을 문서에 명시해야 나중에 "어차피 exec하니까 shell도"라는 스코프 침식을 막을 수 있다.

**출력 처리**: `--progress plain`으로 라인 스트림을 그대로 relay하고 **성패는 exit code로만 판정**한다. 구조화 출력 파싱에 의존하면 compose 마이너 버전 업에서 깨진다.

**Process Group**: compose CLI 자식은 별도 process group으로 실행한다. 취소 시 프로세스 트리 전체를 정리할 수 있고, 세션 정리가 실행 중인 compose에 신호를 흘리지 않는다.

Compose 버전은 "v2"라는 major 이름에 기대지 않고 **Agent 이미지에 Dockpilot이 검증한 plugin 버전을 고정 번들**한다. Agent capability가 실제 bundled version을 보고하고, CI는 `Dockpilot release X → bundled Compose version Y` 조합으로 e2e 검증한다. Docker Engine은 여전히 가변이므로 **API version negotiation과 최소 엔진 버전 선언·기동 시 검사**는 유지한다.

### 8.2 Operation 타입

```
container.start / stop / restart / remove
compose.pull / up / down / start / stop / restart   (project 및 service 단위)
compose.file.write / env.write / override.write
backup.create / backup.restore
discovery.rescan
```

### 8.3 상태 기계

```
requested → dispatched → running → success
                              ├──→ failed
                              ├──→ canceled       (사용자 명시 취소 또는 timeout)
                              └──→ interrupted    (Agent 재시작 등 예상치 못한 중단)
      └──────────────────────────→ unknown        (dispatch 후 연결 소실)
      └──────────────────────────→ rejected       (agent offline / lock 경합 / capability 없음)
```

`canceled`와 `interrupted`는 둘 다 부분 적용 가능 경고를 표시하지만 원인을 다르게 Audit에 남긴다.

Operation 레코드:

```
operation_id, agent_id, target, type,
requested_at, started_at, finished_at,
status, phase, cancel_mode, operation_revision,
cancel_requested_at, cancel_reason, partial_effects_possible, commit_started_at,
result/error, output tail
```

### 8.4 Project Lock

**Lock의 권위는 Agent**다. Server는 UI 표시용 pre-check만 한다. Agent가 실행 주체이므로 Server 재시작·중복 요청·경합에도 안전하다.

```
v1은 project-exclusive lock 하나만 둔다.
  획득: compose.*, *.file.write, backup.create, backup.restore,
        container.* (해당 컨테이너가 속한 project)
  미획득: 모든 read (logs, stats, ps, config, file read)
경합 시: 최대 2초 대기 후 409 PROJECT_BUSY (큐잉하지 않음)
```

container-level lock 세분화는 v1에 불필요한 복잡도다. `container.restart`가 project lock을 200ms 잡는 것은 실무상 문제가 없다. discovery root 밖 unmanaged project의 컨테이너는 pseudo-project 키로 lock을 잡는다.

### 8.5 멱등성 / Timeout / 출력

```
Idempotency  : operation_id를 Server가 생성해 전달.
               Agent는 최근 결과를 ring buffer에 보관하고 같은 id 재요청은
               재실행하지 않고 기존 결과를 반환. 재연결 복구의 전제다.
Timeout      : 타입별 상한(§14 기본값). 별도 종료 메커니즘을 만들지 않고
               CancelOperation(reason=TIMEOUT) 경로를 사용한다.
Output tail  : 마지막 64KB를 operation record에 보존. 전체 로그는 저장하지 않는다.
               이것이 없으면 compose 실패 시 GUI가 쓸모없다.
```

### 8.6 Phase와 Revision

```
PREPARING → EXECUTING → COMMITTING → FINALIZING → terminal
```

각 전이는 단조 증가 `operation_revision`을 가지며 실시간 Progress Event로 Server에 전달된다.

```
Operation Progress Event:
  operation_id, operation_revision, phase, cancel_mode, occurred_at, progress metadata
```

Server는 **가장 높은 revision만** Operation Mirror에 반영한다. Progress stream은 빠른 UI 갱신 수단이고 **실제 권위는 Agent의 현재 Operation Record**다. 연결 단절 또는 Server 재시작 후 `GetOperation(operation_id)` / `ListActiveOperations()`로 현재 phase와 revision을 복구한다.

모든 Operation이 COMMITTING을 거치지는 않는다(§9.2).

### 8.7 Offline / Disconnect / Reconciliation

```
Agent offline 시 요청 → 큐잉하지 않고 즉시 rejected
                       (30분 뒤 살아나서 compose down이 실행되면 사고다)

unknown 복구 → 재연결 시 Server가 unknown operation_id 목록으로 조회
             → Agent가 result ring에서 반환
             → 결과 없으면 unknown 유지. 추측하지 않는다.

Agent 재시작 → running 상태로 남은 것을 interrupted로 마킹 + audit
             → "부분 적용 가능, 현재 상태 확인" 배너
             → 자동 재시도나 자동 롤백을 하지 않는다

Server 재시작 → Agent가 active operation 목록을 보고해 미러 복원
Stream 재개  → 자동 재개하지 않는다. 브라우저가 재요청한다.
```

### 8.8 Stalled Operation과 Lock 탈출

`stalled_warning` 발화 조건:

```
cancel_mode = BEFORE_COMMIT
AND phase = COMMITTING
AND 마지막 commit progress 이후 기본 5분 경과
```

terminal state가 아니라 경고 플래그다. 자동으로 failed 처리하지 않고 Project Lock을 자동 해제하지 않는다.

```
status: running
phase: COMMITTING
stalled_warning: true
```

Progress 판정은 stdout 출력 여부가 아니라 **Operation 단계 revision**을 기준으로 한다(예: restore의 `file 2/4 replaced`, file write의 `rename started` / `rename completed`).

**`force_release_lock` API는 만들지 않는다.** 실행 중인 작업 위에 다른 작업을 얹을 수 있게 되어 lock의 존재 이유가 사라진다. 탈출은 운영 절차로 한다.

```
1차: Host에서 Dockpilot Agent 컨테이너 재시작
     → 실행 중이던 Operation이 interrupted
     → Project Lock 자연 해제
     → Server로 interrupted 상태와 부분 적용 가능성 보고
2차: uninterruptible I/O로 종료되지 않으면
     Host filesystem/network mount 복구 또는 Host 재시작
```

이는 새 기능이 아니라 문서화된 운영 절차이며 "복잡한 작업은 SSH로"와 일치한다.

---

## 9. 취소 모델

### 9.1 원칙

**Cancel은 rollback이 아니다.** `CancelOperation`은 "지금 실행을 멈춰라"이지 "이미 적용된 것을 되돌려라"가 아니다. Dockpilot은 일반 Operation에 대해 자동 rollback을 수행하지 않는다.

**Browser disconnect도 Transport disconnect도 Operation cancel이 아니다.** 변경 Operation은 Browser와 transport session으로부터 독립적으로 실행된다.

```
Browser 종료          → Operation 계속 실행
Server-Agent 연결 단절 → Agent에서 Operation 계속 실행
재연결                → operation_id로 상태와 결과 회수
```

반면 logs / stats / 일반 query는 일시적 Stream이므로 브라우저가 닫히면 즉시 취소하고 자동 재개하지 않는다.

### 9.2 Cancel Mode와 Phase 대응

**A. SAFE** — `discovery.rescan`, `compose.pull`, `backup.create`

```
Phase: PREPARING → EXECUTING → FINALIZING → terminal   (COMMITTING 없음)
취소: 전 구간 가능. 미완성 staging/archive 삭제.
결과: status=canceled, partial_effects_possible=false
```

`compose.pull`은 일부 layer/cache가 남을 수 있으나 실행 중 Compose runtime의 부분 적용 문제는 없다. 여기서 "안전"은 부작용 0이 아니라 **런타임 구성의 일관성을 깨뜨리지 않는다**는 뜻이다.

**B. BEST_EFFORT_PARTIAL** — `compose.up / down / start / stop / restart` (서비스 단위 포함)

```
Phase: PREPARING → EXECUTING → FINALIZING → terminal   (COMMITTING 없음)
       Compose 실행 전체에 단일 비가역 commit point가 존재하지 않는다
취소: process group에 SIGTERM → grace period → 필요 시 SIGKILL
결과: status=canceled, partial_effects_possible=true
```

UI: *"작업이 중간에 취소되었습니다. 일부 서비스에만 변경이 적용되었을 수 있습니다. 현재 Compose 상태를 확인하십시오."*

**C. BEFORE_COMMIT** — `file write`, `backup.restore`, `container.start/stop/restart/remove`

```
Phase: PREPARING → EXECUTING → COMMITTING → FINALIZING → terminal
취소: commit point 이전만 가능. 이후에는 TOO_LATE, 정상 완료 또는 recovery 진행.
```

Commit point:

```
File Write         : target으로 atomic rename을 시작하는 순간
Restore            : 첫 target file 교체를 시작하는 순간
Container mutation : Docker Engine에 mutation request를 전달하는 순간
```

이미 Docker에 전달한 요청을 Dockpilot이 되돌리려 하지 않는다.

### 9.3 Commit 진입과 Cancel 경쟁 조건

Commit 진입과 Cancel 판정은 **동일한 개별 Operation mutex** 아래에서 처리한다(Project Lock이 아니다).

```
operation.mu lock
  if cancel_requested:
      cancel before commit
  else:
      phase = COMMITTING
      commit_started_at = now
      cancelable = false
      operation state 영속화
operation.mu unlock

irreversible action 실행
```

이 규칙이 없으면 "CancelOperation이 ACCEPTED를 반환했는데 이미 rename이 시작된" race가 생긴다. 재현이 거의 불가능한 버그가 되므로 구현 규칙으로 못박는다.

COMMITTING 전이 직후 Agent가 죽으면 실제 commit 수행 여부를 정확히 모르므로 보수적으로 기록한다.

```
status: interrupted
partial_effects_possible: true
last_known_phase: COMMITTING
```

### 9.4 CancelOperation API

```
CancelOperation(operation_id, reason)

응답: ACCEPTED | TOO_LATE | NOT_CANCELABLE | ALREADY_TERMINAL | NOT_FOUND
반복 요청은 멱등해야 한다.

cancel_reason: USER | TIMEOUT | AGENT_SHUTDOWN
```

UI는 `phase`와 `cancel_mode`를 기준으로 Cancel 버튼을 활성화한다.

### 9.5 Timeout

```
Timeout 발생 → CancelOperation(operation_id, reason=TIMEOUT)

Commit 이전 → cancel 수용
Commit 이후 → cancel 거부, 강제 종료 없음, deadline_exceeded 표시,
              완료 또는 recovery까지 계속 실행
```

**Timeout도 데이터 정합성보다 우선하지 않는다.**

`stalled_warning`은 `phase=COMMITTING` 조건이므로 BEFORE_COMMIT 계열에만 발화한다. Compose 계열이 장시간 실행되는 경우는 timeout이 처리하며, compose에는 보호해야 할 단일 commit point가 없으므로 강제 종료가 허용되는 것이 정상이다.

---

## 10. 파일 안전 모델

### 10.1 접근 범위

```
API 계약: Server → Agent 는 (project_uid, relative_path) 만 전달. 절대경로 금지.
허용 파일 화이트리스트:
  compose.yaml | compose.yml | docker-compose.yaml | docker-compose.yml
  compose.override.* | docker-compose.override.* | compose.*.yaml
  .env | .env.*
허용 위치: project의 canonical working_dir 내부
```

### 10.2 참조 파일 (env_file / include / extends)

**읽기·쓰기 허용** — 프로젝트의 직접 관리 파일:

- 기본 compose 파일
- 명시적으로 선택된 override 파일
- Dockpilot이 생성한 override 파일
- 프로젝트 루트의 `.env` / `.env.*`

**읽기만 허용** — compose가 참조하는 파일 중 다음을 **모두** 만족하는 것:

- canonical path가 project working_dir 내부
- discovery root 내부
- Agent에 실제로 mount되어 있음
- symlink가 아님

**working_dir 밖의 참조 파일**: 내용 편집 금지. 경로와 접근 가능 여부만 표시한다.

source-provenance parser의 추가 경계:

- 참조는 literal path를 현재 source file directory 기준으로 canonicalize한 뒤
  discovery root와 검증된 identical-path mount 안인지 확인한다. root 밖 경로는
  표시·읽기 모두 하지 않는다.
- `working_dir` 안의 include/extends source file은 모든 parent component와 target을
  `O_NOFOLLOW`로 다시 확인한 regular file일 때만 읽고, fingerprint에 넣으며,
  project의 temporary `ReadOnly` allowlist에 추가한다.
- `working_dir` 밖이지만 discovery root 안인 참조는 fd-relative no-follow로
  읽고 SHA-256/크기만 fingerprint에 넣는다. 내용은 Server로 보내지 않고 부모
  project의 파일 읽기 allowlist에도 추가하지 않는다. 해당 directory가 별도
  discovery project라면 그 project 화면에서만 정상 파일 접근을 판단한다.
- service `env_file`, include의 기본 `.env`, long-syntax `env_file`도 같은 방식으로
  내용 없는 digest만 fingerprint에 포함한다. 안전하게 해석하거나 hash할 수 없는
  입력이 하나라도 있으면 graph를 incomplete로 표시하고 evaluation cache를
  재사용하지 않는다.

근거: compose 파일은 **사용자가 편집 가능한 콘텐츠**다. 참조 경로를 그대로 신뢰하면 `include: /etc/...` 같은 경로로 Agent를 임의 파일 리더로 만들 수 있다. 또한 `include`는 `../commons/compose.yaml`처럼 부모 프로젝트 밖을 참조할 수 있고 포함된 파일마다 자체 프로젝트 디렉터리 기준으로 상대경로를 해석하므로, 부모 화면에서 이런 파일까지 쓰게 하면 다른 프로젝트와 공유된 설정을 예상치 못하게 변경할 수 있다.

```
Parent Project
  include ../commons/compose.yaml   → fingerprint digest만, 파일 API 접근 금지

Commons Project
  자체 project로 discovery됨        → 자신의 화면에서 정상 편집
```

### 10.3 Path 검증 (TOCTOU 포함)

```
1. relative_path에 절대경로 / ".." / NUL 포함 시 즉시 거부 (문자열 수준)
2. filepath.Clean 후 working_dir과 join
3. 최종 경로가 working_dir prefix인지 재검증
4. 열 때 O_NOFOLLOW (Linux면 openat2 + RESOLVE_BENEATH 선호)
5. 열린 fd에서 다시 stat → 검증 시점과 동일 대상인지 확인
```

**symlink 정책: 거부.** 대상 파일이 symlink면 편집을 거부하고 UI에 이유를 표시한다. symlink를 허용하는 순간 위 검증이 전부 우회 가능해진다. 예외 허용은 FUTURE.

### 10.4 Atomic Write

```
1. 같은 디렉터리에 임시 파일 생성 (0600)   ← 같은 디렉터리여야 rename이 원자적
2. 원본의 mode/uid/gid 복사
3. write → fsync(file)
4. rename(tmp, target)
5. fsync(dir)
```

NFS·일부 overlay 조합에서 rename 원자성이 보장되지 않는다 — 문서에 명시한다.

### 10.5 Validation

```
1. staged 파일을 원본 디렉터리에 임시 이름으로 배치
   (상대경로 해석 컨텍스트를 보존해야 하므로 별도 temp dir로 복사하면 안 된다)
2. docker compose -f <staged 조합> --project-directory <dir> config -q
3. 통과 → atomic rename / 실패 → 임시 파일 삭제 + 에러 그대로 UI 반환
```

`.env` 검증도 이 경로에서 커버된다(변수 치환 실패가 config에서 드러난다).

### 10.6 Concurrent Edit Detection (CORE)

```
read  → Agent가 {content, sha256, mtime} 반환
write → 요청에 expected_sha256 동봉
        불일치 시 409 CONFLICT + 현재 내용 반환 → UI에서 diff 표시
```

§7.7의 외부 변경 감지와 같은 해시를 쓴다. 비용 거의 0, 데이터 손실 방지 효과 최대. SSH에서 누군가 파일을 수정한 직후 오래 열려 있던 화면의 내용을 저장해 덮어쓰는 사고를 막는다.

### 10.7 단일 파일 경계

**일반 파일 쓰기 API는 파일 1개 단위로 한정한다.**

```
WriteProjectFile = Operation 1건당 target file 1개
```

UI에서 `compose.yaml`과 `.env`를 동시에 수정해도 실제 처리는 독립 Operation 2건의 순차 실행이다.

```
compose.yaml: success
.env: failed
```

이 상태를 하나의 트랜잭션 실패로 포장하지 않는다. **첫 번째 성공 파일을 자동으로 되돌리지 않는다.**

각 파일 Operation은 독립적으로: `expected_sha256 확인 → staged validation → pre-write snapshot → atomic single-file rename → file fsync → directory fsync → Audit 기록`.

UI의 `[모두 저장]` 버튼은 제공할 수 있으나 의미는 단순한 순차 호출이며 개별 결과를 표시한다. Server와 Agent에 일반 편집용 multi-file transaction API를 만들지 않는다.

### 10.8 기타 제약

- 파일 크기 상한 1MB (초과는 SSH로)
- 유효 UTF-8 강제, 라인 엔딩 보존
- **저장 ≠ 적용.** UI에서 완전히 분리하고, 저장 후 "변경사항이 아직 적용되지 않았습니다 [Up으로 적용]"을 표시한다.

---

## 11. Audit 모델

### 11.1 두 종류

**Managed Audit** — Dockpilot을 통해 발생한 행위. Operation lifecycle에서 자동 생성한다(operation 완료 = audit 1건). 별도 코드 경로를 만들지 않는다.

`actor`는 표현 가능한 것만 기록한다: `ui:<client_ip>` 또는 `webhook:<provider>`. 없는 정보를 만들어내지 않는다.

**Managed Audit에는 rate limit을 적용하지 않는다. 전량 기록한다.**

**Observed Audit** — Dockpilot 밖에서 발생한 변경. Docker Events를 signal로 쓰되, **Event는 상태 변경의 신호이지 현재 상태의 Source of Truth가 아니다.**

```
구독 필터(화이트리스트):
  container: create, start, die, stop, kill, destroy, health_status, rename
  image:     pull, delete, tag
  volume:    create, destroy
  network:   create, destroy
  제외:      exec_*, attach, top, resize, commit, ...

억제:
  coalescing — 같은 (container, event_type)이 5초 내 반복 시 1건으로 묶고 count 부여
  rate limit — 초당 20건 상한, 초과 시 "event storm 감지" 요약 1건
```

`die`/`health_status`처럼 의미 있는 전이만 inspect 1회를 추가한다. `start`는 event 자체로 충분하다. 재조회를 남발하면 event storm이 daemon 부하로 증폭된다.

- 실제로 관찰할 수 없는 원인을 추측하지 않는다. 누가 SSH 명령을 실행했는지 Docker가 알려주지 않으면 actor는 `unknown`/`external`이다.
- OS-level full audit system을 만들지 않는다.

### 11.2 Reconciliation 수준 (경량)

Docker 상태를 저장하지 않으므로(UI는 항상 on-demand 조회) reconciliation은 오직 **audit 연속성**을 위한 것이다.

```
Agent 시작 시       : last_event_ts로 --since 재구독 (gap 최소화)
                     + 현재 container 목록 스냅샷 1회
                     + 이전 스냅샷과 다르면 "unobserved change" audit (원인 unknown)
event stream 끊김   : 재연결 시 동일 절차
daemon 재시작 감지  : 동일 절차
주기적 full reconcile: 불필요 — 만들지 않는다
```

### 11.3 Agent WAL

Agent는 **인덱스·검색·필터가 없는 bounded append-only disk WAL**을 가진다. 중앙 저장소가 아니라 Server 단절 구간을 견디기 위한 **전달 내구성 계층**이다.

메모리 링 버퍼를 선택하지 않은 이유: incarnation을 추가해도 메모리에서 사라진 Audit의 **내용**은 복원할 수 없다. epoch은 유실을 관측 가능하게 만들 뿐 유실 자체를 막지 못한다. 또한 Agent는 이미 `agent_id`, credential, incarnation, operation result, backup 때문에 영속 state directory가 필요하므로, bounded append-only 파일 하나는 새로운 런타임 의존성도 쿼리 DB도 아니다.

**Record 구조와 복구**

```
record length | record payload | checksum

기동 시: 마지막 segment scan → 불완전한 tail record 제거
        → checksum 불일치 tail 제거 → 마지막 유효 record까지 복구
```

**Write 정책**

```
write()  : 매 record 즉시. partial write는 완료될 때까지 반복.
fsync()  : 1초 또는 64KB 중 먼저 도달. graceful shutdown 시 무조건 최종 fsync.
```

효과: Agent process crash / OOM / container restart에서는 write된 record가 kernel page cache에 있어 복구 가능성이 높다. Host power loss 또는 storage failure에서만 마지막 fsync 이후 구간이 유실될 수 있다. **fsync 전 데이터까지 완전 내구성을 보장한다고 표현하지 않는다.**

### 11.4 Audit Identity와 Cursor

```
Audit key = agent_id + incarnation + seq

agent_id     : Agent 설치 identity
incarnation  : Agent 기동마다 1 증가. 기동 시 한 번 영속화 및 fsync.
seq          : incarnation마다 1부터 증가

Server unique key : UNIQUE(agent_id, incarnation, seq)
Cursor            : (incarnation, seq)
```

incarnation이 필요한 이유: 매 record fsync 없이는 카운터가 되감길 수 있고, 재시작을 seq 되감기가 아니라 **명시적 경계**로 표현해야 한다.

`ReadAuditFrom(cursor, limit)`은 **incarnation 경계를 넘어** (incarnation, seq) 사전순으로 반환한다. Agent가 재시작해도 이전 incarnation의 flushed-but-unacked segment는 유지하며, Server ACK cursor 아래로 완전히 내려간 뒤에만 삭제한다. 그렇지 않으면 재시작이 곧 미ACK 구간 확정 유실이 되어 디스크 WAL을 고른 이유가 사라진다.

Server가 다음 incarnation을 ACK하려면 이전 incarnation에 대해 다음 중 하나가 존재해야 한다: **모든 실제 record / AUDIT_GAP / AUDIT_CONTINUITY_UNCERTAIN.**

**Cursor 연산 규칙**: cursor는 `(incarnation, seq)` 복합값이므로 incarnation 경계에서 `-1` 연산이 정의되지 않는다. **`wal_floor - 1` 같은 표현을 사용하지 않는다.** 범위는 half-open interval `[from, until)`로 기록한다.

```
Read  cursor: inclusive — 반환을 시작할 첫 cursor
ACK   cursor: inclusive watermark — 연속적으로 저장 완료된 마지막 cursor
```

### 11.5 clean_close와 unclean shutdown

```
Agent state 최소 항목:
  current_incarnation
  clean_close: { incarnation, last_durable_seq, closed_at }
```

기동 순서:

```
1. 이전 current_incarnation 확인
2. clean_close.incarnation이 이전 incarnation과 일치하는지 확인
3. WAL의 실제 유효 tail과 clean_close.last_durable_seq 비교
4. clean_close가 없거나 불일치 → 이전 incarnation = UNCLEAN
5. incarnation + 1
6. 새 incarnation을 state에 저장
7. state file fsync
8. state directory fsync
9. 그 후에만 Docker Events 구독과 요청 수신 시작
```

정상 종료 순서:

```
DRAINING 진입
→ 신규 변경 Operation 거부
→ Docker Event 수신 중단
→ 실행 중 Operation 종료·취소 정책 수행
→ 남은 Audit append
→ WAL fsync
→ clean_close 기록
→ state file fsync → state directory fsync
→ 프로세스 종료
```

**clean_close를 기록한 뒤 새로운 Audit이 생성되어서는 안 된다.**

모든 unclean shutdown에서 `AUDIT_CONTINUITY_UNCERTAIN`을 남긴다. SIGKILL, OOM, runtime crash에서도 이벤트 관찰과 WAL write 사이의 작은 구간이 존재하기 때문이다.

### 11.6 AUDIT_GAP과 AUDIT_CONTINUITY_UNCERTAIN

두 개념을 하나의 Audit Error Event로 통합하지 않는다.

```
AUDIT_GAP
- 삭제된 정확한 범위를 알고 있음
- out-of-band coverage state (WAL 공간 부족 때문에 발생할 수 있으므로 WAL에 넣지 않는다)
- 필드: agent_id, incarnation, from_seq, to_seq, reason, precision
- reason: RETENTION | DISK_PRESSURE
- precision: exact | coalesced

AUDIT_CONTINUITY_UNCERTAIN
- 실제 유실 여부·개수를 정확히 모름
- in-band 일반 WAL record (기동 시 공간이 있는 상태에서 발생하므로)
- 필드: previous_incarnation, known_durable_through, reason=UNCLEAN_SHUTDOWN
- 정확한 유실 건수를 기록하지 않는다
```

이 비대칭은 의도된 것이다. 둘을 같은 메커니즘으로 "정리"하려는 시도가 반드시 나오므로 문서에 남긴다.

**Gap 압축 순서:**

```
1. 겹치거나 인접한 exact gap 병합
2. gap interval 개수 상한 적용
3. 상한 초과 시 오래된 여러 gap을 하나의 bounding range로 병합
4. 이때 precision = coalesced
5. 범위를 전혀 특정할 수 없을 때만 incarnation 전체 coverage_unknown
```

coalesced 범위 내부에는 실제로 보존됐던 record가 일부 포함될 수 있으므로 exact gap처럼 표현하지 않는다.

### 11.7 Coverage 동기화 계약

Agent가 제공하는 Audit 조회 계약은 **정확히 두 개**다.

```
ReadAuditFrom(cursor, limit)
- in-band WAL record stream, (incarnation, seq) 사전순, 경계를 넘어 반환
- 검색·필터·날짜 정렬 없음

GetAuditCoverage()
- out-of-band Coverage State 전체 반환
- coverage_revision, AUDIT_GAP interval 목록,
  coverage_unknown incarnation 목록, precision 포함
```

Agent에 **만들지 않는 것**: 날짜 기준 조회, project별 조회, event type별 조회, 검색, 정렬, 임의 페이지 조회. 통합 조회·검색·필터·정렬·페이징은 Server canonical store만 담당한다.

**coverage_revision** (uint64, 영속, 변경 시 1 증가, 변경된 coverage state와 atomic 저장 후 fsync):

gap 생성·병합·압축·coverage_unknown 승격 시 증가한다. 이것이 없으면 Server가 snapshot을 읽은 뒤 ACK하기 전에 새 gap이 생겨 반영되지 않은 채 cursor가 전진하는 race가 발생한다.

```
AuditCoverageSnapshot:
  agent_id, coverage_revision, generated_at,
  gaps[]: { incarnation, from_seq, to_seq, reason, precision },
  coverage_unknown_incarnations[]
```

연결 수립 시: Agent가 현재 coverage_revision을 초기 상태에 포함 → Server가 `GetAuditCoverage()` → Agent가 전체 snapshot 반환 → **Server가 Coverage Ledger와 coverage_revision을 하나의 transaction으로 저장** → 이후에만 ACK 전송 가능.

Coverage 변경 시 Agent가 `CoverageChanged` 이벤트를 Control/Durable 채널로 전송하고 Server가 다시 전체 snapshot을 받는다. **Delta Coverage API는 만들지 않는다** — coverage state는 gap 병합으로 bounded하다.

### 11.8 ACK 규칙

```
AckAudit:
  audit_archive_id
  cursor (incarnation, seq)
  coverage_revision_seen
```

**전역 revision 일치를 요구하지 않는다.** ACK 대상 cursor보다 뒤에서 발생한 무관한 coverage 변경까지 ACK를 차단하기 때문이다. Agent는 Coverage State Lock 아래에서 다음 범위만 검사한다.

```
ACK 검사 범위: (current server_acked_through, proposed_ack_cursor]

검사 항목 — Server가 본 revision 이후 발생한 변경 중:
  - 새 AUDIT_GAP이 검사 범위와 교차
  - 기존 GAP이 검사 범위 안으로 확장
  - precision이 exact → coalesced로 악화
  - 검사 범위에 해당하는 incarnation이 coverage_unknown으로 승격

하나라도 존재 → ACK 거부, STALE_COVERAGE
존재하지 않음 → 전역 revision이 더 높아도 ACK 허용
```

이를 위해 Coverage Entry는 `last_loss_revision`(마지막으로 **손실 범위**가 변한 revision)을 가진다. Server ACK 이후 오래된 entry 정리, ACK cursor보다 뒤의 GAP 변경, 비의미적 정렬 변경은 손실로 보지 않는다.

**STALE_COVERAGE 응답에는 현재 snapshot을 동봉한다.**

```
STALE_COVERAGE:
  current_coverage_revision
  AuditCoverageSnapshot 전량
  blocking_ranges
  current_server_acked_through
```

Server는 별도 `GetAuditCoverage()` 없이 즉시 Ledger를 갱신(하나의 transaction)하고 새 revision으로 재시도한다. 이로써 정합성 회복에 필요한 왕복이 1회로 고정되고, disk pressure 중 RPC 증폭이 완화된다.

**ACK 처리 원자성** — 동일한 Coverage State Lock 아래에서:

```
1. 현재 server_acked_through 읽기
2. proposed_ack_cursor 검증
3. ACK 범위와 교차하는 신규 Coverage Loss 검사
4. ACK 수용 또는 STALE_COVERAGE 결정
5. 수용 시 server_acked_through 갱신
6. 갱신된 ACK State 영속화
```

영속화 실패 시 ACK 성공을 반환하지 않는다. Server는 재시도하고, 이미 저장한 Audit Event는 unique key로 중복을 흡수한다.

ACK의 의미:

> Server가 record를 보유하거나, 보유하지 않는 범위와 이유를 Coverage Ledger에 정확히 기록했다.

### 11.9 CURSOR_BEHIND_FLOOR와 AuditBounds

```
AuditBounds:
  wal_floor              (보유한 첫 record cursor, 비어 있으면 null)
  wal_ceiling            (보유한 마지막 record cursor, 비어 있으면 null)
  next_cursor            (다음 record가 받을 cursor)
  server_acked_through
  acknowledged_archive_id
  current_coverage_revision
```

`ReadAuditFrom`의 start_cursor가 wal_floor 이전이면 **오류가 아니라 정상 typed response**를 반환한다(Agent 측에 새 손실이 발생한 것이 아니다).

```
CURSOR_BEHIND_FLOOR:
  requested_start_cursor, wal_floor, wal_ceiling, next_cursor,
  server_acked_through, acknowledged_archive_id,
  current_coverage_revision, AuditCoverageSnapshot 전량
```

가능한 원인: 신규 Server Archive, Server DB 복원, Server cursor metadata 손실, Agent의 정상 절삭, WAL retention/disk pressure, 또는 이들의 조합.

Server는 `ReadAuditFrom(wal_floor, limit)`로 재개한다.

### 11.10 Server Canonical Archive와 Coverage Ledger

Server canonical archive는 **모든 Audit을 영원히 보유한다는 뜻이 아니다.**

보장하는 것:

- 현재 보유 중인 Audit 범위를 정확히 안다
- 미보유 범위를 정확히 표시한다
- 미보유 원인이 Agent 측인지 Server 측인지 구분한다
- 모르는 원인을 추측하지 않는다
- Coverage가 설명되지 않은 상태에서 ACK하지 않는다

보장하지 않는 것: Agent 설치 전 모든 Docker Event 보유, 무제한 영구 보존, 단 한 건도 유실되지 않음.

**Coverage Ledger source 최종 목록:**

| source | 의미 |
|---|---|
| `AGENT_GAP` | Agent가 exact 또는 coalesced 유실 범위를 보고 |
| `AGENT_CONTINUITY_UNCERTAIN` | Agent unclean shutdown으로 연속성 불확실 |
| `SERVER_RETENTION` | Server 정책에 따라 과거 canonical audit 삭제 |
| `SERVER_COVERAGE_START` | 현재 Archive가 해당 Agent audit 보유를 시작한 하한 |
| `SERVER_CURSOR_REGRESSION` | 동일 Archive에서 cursor가 후퇴해 Agent가 이미 절삭한 record를 복구 불가 |

인접한 coverage interval은 병합해 metadata가 무한 증가하지 않게 한다.

**SERVER_COVERAGE_START** — Loss interval이 아니라 **Lower-bound Marker**다.

```
source = SERVER_COVERAGE_START
audit_archive_id, agent_id, coverage_begins_at, established_at, reason
reason: SERVER_NEVER_HAD | NEW_AUDIT_ARCHIVE | SERVER_DATABASE_REINITIALIZED

wal_floor 존재 → coverage_begins_at = wal_floor
WAL 비어 있음  → coverage_begins_at = next_cursor
```

생성 조건은 **신규 Connection이 아니라 신규 Archive Coverage**다. 해당 `(audit_archive_id, agent_id)`에 Coverage 기록이 없을 때 Archive별 Agent별 1회만 생성한다. 단순 transport 재연결에서는 생성하지 않는다.

**SERVER_CURSOR_REGRESSION**

```
조건: audit_archive_id가 Agent의 acknowledged_archive_id와 동일
      + 복원한 start_cursor가 wal_floor 이전
      + canonical archive의 계산된 cursor가 Agent가 이미 ACK한 구간보다 후퇴

필드: source, audit_archive_id, agent_id,
      unavailable_from, unavailable_until, detected_at, reason, precision
범위: half-open [unavailable_from, unavailable_until),  unavailable_until = wal_floor
reason: DATABASE_RESTORE | ARCHIVE_ROLLBACK | CURSOR_METADATA_LOSS | UNKNOWN
```

원인을 확실히 알지 못하면 `DATABASE_RESTORE`라고 추측하지 않고 `UNKNOWN`을 쓴다.

**Server Cursor 재계산**: `agent_cursors` 행 하나만 신뢰하지 않는다. Server 시작 시 canonical `audit_events`, Agent Coverage Ledger, Server Coverage Ledger, `SERVER_RETENTION_APPLIED`, `SERVER_COVERAGE_START`, `SERVER_CURSOR_REGRESSION`으로 재계산한다. `agent_cursors`는 성능을 위한 materialized state이고 권위는 canonical record와 Coverage Ledger다. cursor row만 뒤로 이동했지만 실제 record가 남아 있는 경우를 regression으로 잘못 기록하지 않는다.

**Unavailable Range 원인 분해** — 하나의 원인으로 뭉뚱그리지 않는다.

```
1. 응답에 동봉된 Agent Coverage Snapshot 저장
2. audit_archive_id와 acknowledged_archive_id 비교
3. Agent server_acked_through 확인
4. canonical archive의 실제 cursor 재계산
5. 남은 미보유 범위를 Server-side source로 기록

불변식: wal_floor 이전의 미보유 범위는 반드시 위 5개 source 중 하나로 설명돼야 한다.
설명되지 않는 구간 → AUDIT_COVERAGE_INVARIANT_VIOLATION
                   → ACK 중단, 운영 경고, 원인 추측해 자동 gap 생성 금지
```

### 11.11 Agent Claim과 Server Effective Coverage 분리

```
Agent AUDIT_GAP    = Agent가 해당 record를 더 이상 보유하지 않는다는 사실
Server Effective Gap = Server Archive에도 없고 Agent에서도 회수 불가능하다는 사실
```

둘은 같은 의미가 아니므로 논리적으로 두 계층으로 나눈다.

```
agent_coverage_claims
  agent_id, coverage_revision, incarnation, from_seq, to_seq,
  reason, precision, reported_at
  → "Agent가 이 범위의 보유 능력에 대해 무엇을 보고했는가"

server_archive_coverage
  audit_archive_id, agent_id, unavailable_from, unavailable_until,
  source, precision, effective, established_at, resolved_at
  → "현재 Server Archive에서 실제로 복구 불가능한 범위는 무엇인가"
```

**Exact Gap 처리:**

```
effective_missing = Agent exact gap range - Server canonical record set

전 구간 보유   → Claim 보존, Effective Coverage 생성 안 함
                (server_holds_range=true, effective=false, UI에 미보유로 표시 안 함)
일부만 보유   → 실제 미보유 부분만 Effective Gap interval로 분리
전혀 없음     → Agent Gap 전체가 Effective Missing
```

**Coalesced Gap 처리:**

`precision=coalesced`는 bounding range 내부의 모든 sequence가 손실됐다는 뜻이 아니므로 수신 즉시 확정하지 않는다.

```
상태: PENDING_RECONCILIATION

1. 현재 보유 record를 range에서 차감
2. ReadAuditFrom을 계속 수행
3. 동기화 traversal이 claim range 뒤까지 진행
4. 더 이상 해당 range의 record가 도착하지 않음이 확인됨
   (전달이 cursor 순서이므로 read cursor가 범위 끝을 지나면 성립)
5. 그 시점의 미보유 부분만 Effective Coverage로 확정
```

이후 record가 중복·지연 수신되면 Effective Gap을 다시 축소할 수 있어야 한다.

Coverage Ledger는 **append-only event history**와 **current effective materialized view**를 구분한다.

**왜 필요한가**: Server가 record를 계속 수집하면서도 ACK가 전진하지 못하는 구간(invariant violation, coverage churn, ledger 저장 실패 반복)에서, Agent가 disk pressure로 미ACK 구간을 삭제하면 Server는 동일 범위에 대해 **실제 record와 GAP 주장을 동시에 보유**하게 된다. 대조 없이는 Ledger가 자기모순 상태가 된다.

이 상황이 관측되면 `audit_ack_blocked_while_ingesting`을 함께 경고한다.

### 11.12 ACK 정체 관측

```
audit_ack_watermark_stalled_seconds
audit_ack_cursor
audit_coverage_revision_seen
audit_coverage_revision_current
audit_stale_coverage_total
audit_ack_retry_total
audit_ack_blocked_while_ingesting            (0|1)
audit_ack_blocked_while_ingesting_seconds
audit_ingested_unacked_records
audit_ingested_unacked_bytes
audit_effective_gap_records
audit_agent_gap_claims_total
```

경고 조건:

```
Agent Online
AND Audit Record 수신은 계속됨
AND audit_ingested_unacked_records 증가
AND ACK Watermark가 5분 이상 정체
```

다음은 경고하지 않는다: 새 Audit Event가 생성되지 않음, Agent가 정상적으로 Offline.

UI: *"Audit Record는 Server에 수집되고 있지만 Agent ACK가 진행되지 않고 있습니다. 이 상태가 지속되면 Agent WAL 증가와 저장 공간 압박으로 이어질 수 있습니다."*

**Dockpilot은 이 상황에서 ACK 정합성 규칙을 완화하지 않는다.** Coverage 정합성을 깨뜨리면 Audit 유실 사실 자체를 정확히 기록할 수 없기 때문이다. WAL 크기 상한 자동 확대도 하지 않는다.

지속적인 Coverage Churn(ACK 대상 범위 안에서 GAP이 계속 생성)은 프로토콜 오류가 아니라 **Agent가 Audit을 생성·유실하는 속도가 Server 동기화 속도를 넘어선 상태**다. 정상 부하에서는 발생하지 않아야 하며 이는 전송 프로토타입 합격 기준으로 검증한다.

### 11.13 Server Audit 저장소 고수위 정책

**Server 저장소 용량 부족을 이유로 Agent Audit ingest를 중단하지 않는다.**

거부하면 다음 연쇄가 발생한다:

```
Server ingest 거부 → server_acked_through 정지 → 모든 Agent WAL 증가
→ Agent disk pressure → 여러 Agent에서 AUDIT_GAP(DISK_PRESSURE)
```

중앙 한 곳의 용량 문제가 모든 호스트의 감사 유실로 확산되며, 되돌릴 수 없는 쪽이 확대되는 방향이다.

> **중앙의 오래된 Audit을 잃는 것이 Agent의 최신 미동기 Audit을 잃는 것보다 낫다.**

```
정상       : 설정된 retention/quota에 따라 오래된 Audit 삭제
80%        : 용량 경고, retention 작업 우선 수행
95%        : aggressive retention 진입, 목표 low watermark까지 정리
             Agent ingest는 계속 허용
quota 도달 : 가장 오래된 것을 밀어내며 최신 ingest 유지 (bounded ring archive)
free-space 위험 : 신규 backup 등 비필수 쓰기 차단,
                 Audit ingest용 emergency reserve 사용, 오래된 중앙 Audit 계속 축출
```

물리 디스크 100%에서 무조건 쓰기를 보장할 수는 없다. 이를 피하려면: configured quota를 실제 filesystem 한계보다 낮게, Audit ingest용 emergency reserve 확보, 80%부터 사전 경고, 95%부터 강제 retention.

**Server "쓰기 보호"는 UI에서의 신규 backup 같은 사용자 행위에만 적용하고 audit ingest에는 적용하지 않는다.**

**Retention 우선순위:**

```
1. ACK 완료된 오래된 Audit Record 축출
2. ACK 완료된 Coverage History 축약
3. 이미 SERVER_RETENTION Coverage가 확정된 구간 정리
4. 수신했지만 아직 Agent에 ACK하지 못한 record는 최대한 보호
5. 물리 한계로 4번까지 축출해야 하면
   source=SERVER_RETENTION, reason=QUOTA_PRESSURE_BEFORE_AGENT_ACK 로 명시 기록
```

이후 Agent가 같은 범위를 disk pressure로 삭제해도 원인을 하나로 덮지 않고 둘 다 history로 보존한다.

Retention으로 삭제한 범위는 `SERVER_RETENTION_APPLIED`로 남기되, **일반 Audit Event가 아니라 Coverage Ledger에** 기록한다(일반 event는 이후 retention으로 다시 삭제될 수 있다).

운영 Server는 현재 Canonical Archive ID만 대상으로 별도 retention worker를 하나 실행한다. 시작 직후와 이후 15분마다 실행하며, 한 번의 실행은 1분 context로 제한된다. 실행 실패·timeout은 다음 주기에 재시도할 뿐 Agent ingest/ACK 요청 경로로 전파하지 않는다. worker 내부의 삭제 transaction은 최대 512 record batch이며, 각 transaction 직전에 현재 Archive identity를 재검증한다.

---

## 12. Metrics

**CORE이지만 v1에서 저장하지 않는다.** 별도 Prometheus/Grafana 없이 현재 상태를 즉시 확인할 수 있어야 한다.

### 12.1 Viewer-scoped

```
아무도 보지 않음        → stats 수집 0
Host/Project 화면 열림  → 그 범위의 container만 stats 구독
마지막 viewer 종료      → stream 종료
```

`docker stats`는 컨테이너당 스트림 + daemon 부하다. 100개를 상시 구독하면 Agent가 monitoring platform으로 변질된다. Viewer-scoped가 이를 구조적으로 막는다.

**one-shot이 아니라 stream을 유지한다.** CPU %는 두 샘플의 delta로만 계산되고 one-shot API는 내부적으로 1초 이상 블록된다.

```
Docker stats stream → Agent(집계 없음, 통과) → Server(저장 없음, 마지막 샘플만) → 브라우저
```

**Server에 stats 캐시를 쌓지 않는다.** 마지막 샘플 1개 외에는 보관 금지 — 여기가 "시계열 DB를 안 만들겠다"는 결심이 무너지는 지점이다.

### 12.2 조회 대상

**Container**: CPU usage, memory usage/limit, network RX/TX, block I/O, restart count, health, uptime

Linux memory usage는 Docker CLI와 같은 working-set 근사값을 사용한다: Engine
API의 cgroup total usage에서 v1 `total_inactive_file` 또는 v2 `inactive_file`을
차감하되 cache가 usage 이상이면 원값을 유지한다. health/restart/start metadata는
stats stream만으로 갱신되지 않으므로 stream 유지 중 10초마다 inspect를 다시
조회하며, 일시적인 inspect 실패는 정상 stats sample을 폐기하지 않는다.

**Host/Docker**: daemon availability, container counts, image counts, running/stopped — `docker info` + `docker ps`로 저렴하게. 대시보드가 열려 있을 때만 10초 주기.

**Host의 CPU/메모리/디스크를 수집하지 않는다** — Docker가 주지 않는 정보이고, 그 순간 Agent는 node_exporter가 된다. **DO NOT BUILD.**

### 12.3 History 없이 쓸 만하게

브라우저 메모리 내 링 버퍼(최근 120 샘플, 약 4분)로 sparkline을 그린다. Server 저장이 아니므로 원칙을 위반하지 않으면서 "지금 CPU가 튀는 중인가"라는 실사용 질문의 대부분을 해결한다. **새로고침하면 사라지는 것이 올바른 동작이다.**

```
Metrics current/live view = CORE
Metrics history/trend     = FUTURE
Host OS metrics 수집       = DO NOT BUILD
```

---

## 13. Backup / Restore

### 13.1 Configuration Backup (CORE)

```
저장 위치: <agent_state>/backups/<project_uid>/<timestamp>/
형식:      files.tar.gz + manifest.json
manifest:  { project_uid, project_name, working_dir, created_at, trigger,
             operation_id, files:[{ rel_path, sha256, mode, size }] }
권한:      디렉터리 0700, 파일 0600   (.env가 들어간다)
```

바이트를 Server로 옮기지 않는 이유: restore 실행 주체가 Agent이고, 파일이 원래 Agent 쪽에 있으며, `.env` secret을 네트워크로 왕복시킬 이유가 없다. **메타데이터만 Server에 인덱싱**하고 내용 조회는 요청 시 fetch한다.

자동 snapshot 트리거(CORE): 모든 file write **직전**, restore **직전**.

Retention: 자동 snapshot은 프로젝트당 최근 20개, 수동 backup은 별도 카운트. **정책 결정은 Server가 하고 Agent는 지시받은 삭제를 수행한다.**

### 13.2 Docker Data Backup

```
Named volume backup  = OPTIONAL (v1 제외)
Bind mount backup    = FUTURE
```

v1에서 제외하는 근거:

1. **일관성 보장 불가.** 실행 중인 PostgreSQL의 데이터 디렉터리를 tar하면 복구 불가능한 백업이 나올 수 있다. "백업이 있다고 믿었는데 없었다"는 백업이 없는 것보다 나쁘다.
2. **bind mount는 discovery root 밖의 임의 경로를 가리킨다.** 백업하려면 Agent에 "임의 경로 읽기" 능력을 줘야 하고 §10의 file safety 경계가 무너진다. 이 이유만으로도 bind backup은 v1에서 배제해야 한다.
3. 크기/시간/디스크 압박이 Agent를 backup product로 만든다.

> Dockpilot이 application-consistent database backup을 자동으로 보장하려 하지 않는다.

나중에 넣는다면 최소 형태로만: named volume만, 명시적 opt-in, "애플리케이션 일관성을 보장하지 않습니다"를 회피 불가능하게 표시, DB류는 문서에서 "dump를 compose service로 돌리세요"로 안내.

### 13.3 Restore Safety

| 장치 | 판정 |
|---|---|
| restore 전 현재 상태 자동 snapshot | CORE |
| project exclusive lock | CORE |
| staging 준비 후 교체 (partial 방지) | CORE |
| 실패 시 rollback | CORE |
| audit + operation status | CORE |
| **restore 후 자동 compose up 하지 않음** | CORE — 복원과 적용을 분리 |
| 복원 전 diff 미리보기 | OPTIONAL (가치 높고 비용 낮음, v1.1 후보) |
| 2단계 확인 / 프로젝트명 타이핑 | 불필요 — 단일 확인 다이얼로그로 충분 |
| 승인 워크플로 / 시간 지연 / dry-run 실행 모드 | DO NOT BUILD |

### 13.4 Restore Transaction Journal

여러 파일은 하나의 atomic rename으로 동시에 교체할 수 없다. Agent가 두 번째 파일 교체 직후 죽으면 무엇을 복원해야 하는지 알아야 한다. **Restore에 한해** 작은 영속 journal을 둔다.

```
restore_transaction:
  operation_id, project_uid, phase, pre_restore_snapshot_id
  files: [{ target, staged_path, status: pending | replaced }]
```

기동 시 복구 규칙:

```
PREPARING                    → staging 삭제, canceled/interrupted 처리
COMMITTING + 교체 파일 없음   → staging 삭제, interrupted 처리
COMMITTING + 일부 교체됨      → pre-restore snapshot으로 rollback 시도
  rollback 성공 → interrupted, rolled_back=true
  rollback 실패 → RESTORE_RECOVERY_REQUIRED
                 해당 Project의 변경 Operation 차단
                 SSH를 통한 수동 복구 안내
```

이는 Compose 상태를 추측해 자동 롤백하는 기능이 **아니다.** Dockpilot이 자신이 시작한 다중 파일 트랜잭션의 원자성 경계를 닫는 것이다.

---

## 14. 디스크 압박과 자원 관리

### 14.1 Agent Disk Budget

Dockpilot 때문에 호스트 디스크가 가득 차서 서비스가 죽는 것은 최악의 시나리오다.

**축출 순서** — 되돌릴 수 없는 데이터를 가장 늦게 버린다:

```
1. 중단된 temp/staging 파일
2. Server ACK가 완료된 WAL segment
3. retention이 끝난 Operation result와 output tail
4. 보존 개수를 초과한 자동 Snapshot
5. 오래된 자동 Snapshot (단, Project별 최신 snapshot 최소 1개는 보호)
6. 미ACK WAL → AUDIT_GAP(DISK_PRESSURE) 생성
7. 더 이상 자동 삭제하지 않음 → DEGRADED_STORAGE 진입
```

**수동 Backup은 자동으로 삭제하지 않는다.** 사용자가 명시적으로 생성한 backup을 Dockpilot이 조용히 삭제해서는 안 된다.

### 14.2 Emergency Reserve (기본 64MB)

Audit만을 위한 공간이 아니라 **내구성 핵심 metadata**를 위한 공간이다.

```
허용: Audit WAL append, AUDIT_GAP coverage metadata,
      AUDIT_CONTINUITY_UNCERTAIN,
      최소 Operation 상태 (operation_id, phase, revision, terminal status,
                          commit_started, partial_effects_possible, error_code),
      Restore Transaction Journal, clean_close, incarnation state

금지: Operation stdout/stderr tail, 실시간 Logs, Metrics, Backup bytes,
      자동 Snapshot bytes, staging 파일, UI cache
```

Operation output tail은 disk pressure에서 축출 가능하되 최소 결과(`operation_id, status, phase, error_code, output_truncated:true`)는 보존한다.

목적은 새로운 기능을 계속 수행하는 것이 아니라 **이미 시작된 작업의 복구 경계를 닫고, 발생한 사건을 설명할 최소 기록을 남기는 것**이다.

### 14.3 DEGRADED_STORAGE

```
진입 (OR):  filesystem free < max(1GB, 5%)
           OR Agent state usage > configured budget (기본 2GB)

해제 (AND): filesystem free >= max(1.2GB, 6%)
           AND Agent state usage <= configured budget의 90%
```

진입은 OR, 해제는 AND. 히스테리시스를 두는 이유는 임계점 근처의 플래핑과 cleanup 직후 재진입을 막기 위해서다.

**이 절이 허용/거부의 권위 목록이다.** 다른 절의 "신규 쓰기 Operation 거부" 문구를 이 목록보다 넓게 해석하지 않는다.

**허용되는 조회·스트림**: Docker/Compose 조회, File Read, Logs, Live Metrics, Audit Sync, Operation 상태·결과 조회, Backup 목록 조회, 기존 Backup 수동 삭제

**허용되는 변경 Operation**: `compose.up/down/start/stop/restart`, `container.start/stop/restart/remove`

허용 근거: Agent configuration file을 새로 작성하지 않고, 자동 snapshot이 필요 없으며, Host 장애 대응이나 공간 확보에 필요할 수 있고, Emergency Reserve에서 최소 Operation 상태와 Managed Audit을 기록할 수 있다. 디스크가 찬 상황에서 서비스 재시작 수단까지 차단하면 Dockpilot이 장애 대응을 방해하게 된다.

단, 수락 전에 **Durable Admission Check**를 통과해야 한다: 최소 Operation Record / Managed Audit Record / terminal·interrupted 상태를 기록할 공간이 있는가. 이 최소 공간조차 없으면 거부한다.

**거부되는 Operation**: `compose.pull`, file write, `backup.create`, `backup.restore`, 새 수동 Backup, 자동 Snapshot이 필요한 변경, 대용량 staging이 필요한 작업

`compose.pull`을 거부하는 이유: Docker Storage에 큰 이미지 데이터를 추가해 free-space 부족을 악화시킬 수 있다. `compose.up`은 compose 설정에 따라 image pull이 발생해 실패할 수 있으나 **Dockpilot은 이를 자동으로 변형하지 않는다.** storage warning을 표시하고 Docker/Compose 결과를 그대로 반환한다.

**원인 구분 (필수)**:

```
storage_degraded_reason: FILESYSTEM_FREE_LOW | AGENT_STATE_BUDGET_EXCEEDED | BOTH
```

두 원인은 사용자의 조치가 다르므로 반드시 구분한다. Dockpilot과 무관한 파일 때문에 Host filesystem이 가득 찬 경우 Agent가 자기 데이터를 정리해도 DEGRADED_STORAGE가 유지될 수 있으며 **이는 의도된 동작**이다. 해결은 Host filesystem 정리이며 Dockpilot이 임의의 Host 파일을 삭제해서는 안 된다.

`DEGRADED_STORAGE`는 Compose 같은 허용 capability를 false로 만들지 않는다. 대신 Agent heartbeat의 capability reason으로 보고하며, Server API와 Web UI는 capability가 enabled여도 그 reason을 보존해 경고로 표시한다. 사용자는 작업 버튼을 계속 사용할 수 있고, 어떤 저장소 압박이 남아 있는지도 확인할 수 있어야 한다.

### 14.4 Memory 관측

**Process RSS와 Container cgroup Memory Limit을 같은 값으로 두지 않는다.** Agent 컨테이너 안에서는 Agent process, docker CLI, compose plugin, compose 자식 프로세스, TLS/stream buffer, page cache가 함께 cgroup에 계산된다. 256MB를 그대로 hard limit으로 강제하면 정상 compose 작업 중 OOM Kill이 발생한다.

**관측의 네 가지 목적을 분리한다:**

```
Implementation Weight : Process RSS, Go Heap, Goroutine, FD count, Buffer bytes
Leak Detection        : Process RSS / anon / buffer의 시간 추세, stream 종료 후 회수 여부
Hard Limit Safety     : memory.current, memory.max,
                        memory.events.local.max / .oom / .oom_kill
Diagnostics           : memory.stat breakdown, memory.pressure, latency/throughput
```

**`memory.events.local.max`는 실패 조건이 아니다.**

```
max      : memory.max 경계를 넘으려 해 reclaim이 수행된 횟수.
           파일 I/O가 있는 제한된 cgroup에서는 정상. 진단 지표.
oom      : reclaim으로도 공간을 확보하지 못함. 실패 조건.
oom_kill : 프로세스가 종료됨. 실패 조건.
```

Dockpilot Agent는 WAL write, backup/snapshot 파일 쓰기, compose CLI의 파일 읽기로 page cache를 지속 생성하므로 `max`는 부하 중 계속 증가한다. 이를 실패로 판정하면 정상 구현이 불합격 처리된다.

다만 다음 표현도 쓰지 않는다: "max 증가 = 항상 정상", "max 증가는 page cache 때문이라고 단정". `max`는 anon/file/kernel 어떤 charge로도 증가할 수 있으므로 breakdown과 시간 추세를 함께 본다.

```
max 증가 + oom=0 + oom_kill=0 + anon 안정 + RSS 안정 + P0/P1 지연 없음
  → 합격 가능, reclaim pressure를 진단 정보로 기록

max 증가 + anon 지속 증가 또는 memory.pressure 지속 증가
          또는 Control/Audit Sync latency 악화
  → 조사 대상 (leak 또는 지나치게 낮은 hard limit)

max 증가 + oom 또는 oom_kill 증가  → 실패
```

**File Memory 감소 동반 조건은 두지 않는다.** reclaim과 sampling 시점이 일치하지 않고, kernel이 어떤 page를 회수할지는 시점마다 다르며, tmpfs/shmem이 `memory.stat.file`에 포함될 수 있다. 개별 event와 개별 sample의 1:1 대응을 요구하지 않고 관측 구간 전체의 추세로 판단한다.

**UI 표시** — 두 값을 구분한다:

```
Raw Cgroup Usage      = memory.current          → Capacity / OOM Safety 판단
Approximate Working Set = memory.current - inactive_file (0 하한)
                                                 → 사용자 참고 표시, leak 진단 보조
```

Working Set만 보고 Hard Limit 안전성을 판정하지 않는다.

주의: `memory.stat`의 `kernel`에는 `kernel_stack`, `pagetables`, `slab` 등이 포함될 수 있으므로 단순 합산하지 않는다. 각 항목은 진단용 breakdown으로만 쓴다.

**실패 조건 최종:**

```
실패:
  memory.events.local.oom 증가 > 0
  memory.events.local.oom_kill 증가 > 0
  Process RSS 지속 증가
  Go Heap이 GC 이후에도 지속 증가
  anon Memory 지속 증가
  Stream 종료 후 Buffer Memory 미회수
  Audit Sync Buffer 무제한 증가
  느린 Log Consumer로 인해 다른 Stream이 지연
  Memory Pressure로 P0/P1 처리 보장이 깨짐

실패 아님:
  memory.events.local.max 증가 자체
  file / inactive_file 증가 자체
  WAL write 중 일시적 dirty page 증가
  Backup/Snapshot 중 일시적 file memory 증가
  테스트 종료 직후 page cache 미감소

경고/진단:
  memory.current 또는 memory.peak가 memory.max의 80% 초과
  memory.events.local.max의 지속적 높은 증가율
  memory.pressure의 some/full 증가
  file_dirty / file_writeback 지속 증가
  slab_unreclaimable 또는 sock Memory 지속 증가
```

---

## 15. Persistent State

### 15.1 Server (SQLite 단일 파일, `modernc.org/sqlite`)

| 테이블 | 성격 |
|---|---|
| `agents` | 권위. id, display_name, first/last_seen, metadata, capabilities. **retired 상태 전환, 삭제 금지** |
| `join_tokens` | 권위. 값은 해시로만 |
| `operations` | 미러 + 이력. 상태/시각/요약/output tail |
| `audit_events` | **canonical.** UNIQUE(agent_id, incarnation, seq). **agents에 cascade delete 금지** |
| `agent_coverage_claims` | Agent가 보고한 사실 |
| `server_archive_coverage` | Server가 계산한 effective coverage |
| `agent_cursors` | materialized state (권위 아님) |
| `projects` | 식별 캐시. project_uid, agent_id, working_dir, name, applied_fingerprints, flags |
| `backup_index` | 메타만 |
| `settings` | server config |

**저장하지 않는 것**: container/image/network/volume 목록, container 상태, stats 샘플, 로그, compose 파일 내용, `.env` 내용.

Server Identity State는 **이 DB와 별도**다(§6.1).

### 15.2 Agent (state dir)

```
agent_id, server credential, bound archive identity, incarnation, clean_close
audit WAL (bounded, append-only) + coverage state (out-of-band)
operation result ring
restore transaction journal
backups + manifests
discovery 파일 해시 캐시 (외부 변경 감지 연속성)
```

**저장하지 않는 것**: compose 파일 사본(백업 제외), Docker 상태, metrics, 로그, discovery 결과 영구 저장.

---

## 16. Web UI

프론트엔드 기술에 제약을 두지 않는다(React + TypeScript + Vite 등). 중요한 것은 Server API와 UI의 분리이며, Server binary에 빌드 결과를 embed해 단일 binary로 배포한다.

```
Dashboard
Hosts → Overview / Containers / Images / Networks / Volumes
       / Compose / Metrics / Audit / Backups
Compose Project → Overview / Services / Logs / Metrics / Environment
                 / Compose Files / Override / Backups / Activity
```

- capability가 false인 기능은 **숨기지 말고 비활성 + 이유 툴팁**. 숨기면 사용자가 버그로 오해한다.
- enabled capability의 reason은 비활성 사유가 아니라 **운영 경고**다. UI는 기능을 막지 않고, reason을 호스트 capability 목록과 관련 동작의 툴팁에 표시한다.
- `compose down`은 v1 UI에서 `--volumes`를 노출하지 않는다. 일반 `down` 화면에 **"볼륨은 삭제되지 않습니다"** 를 명시한다(반대 방향 오해보다 이쪽이 흔하다).
- `--remove-orphans`는 기본 비활성 OPTIONAL 플래그.
- `.env` 편집 화면은 값을 기본 마스킹하고 명시적 토글로 표시한다.

**Secret 취급 (CORE)**: audit payload와 operation record에 파일 내용을 절대 넣지 않는다(sha256과 파일명만). compose 로그 스트림에 secret이 나올 수 있으나 이는 Docker의 동작이므로 Dockpilot이 개입하지 않는다(문서에 명시).

---

## 17. Optional Git CD

Dockpilot의 중심은 Docker이며 Git/CD가 아니다. Git integration이 없어도 모든 Docker 관리 기능이 동작해야 한다.

```
Manual Trigger ----\
                   -> 동일한 compose.pull / compose.up operation -> Agent
Git Trigger -------/
```

Manual deploy와 Git-triggered deploy가 서로 다른 deployment engine을 가지지 않도록 한다. **OPTIONAL.**

---

## 18. 기능 분류

### CORE

```
Agent-initiated 연결, TLS + join token + agent credential
Server Identity State (signing key + revocation ledger + archive_generation)
Credential 자동 갱신
self-registration, heartbeat, capability(연결·Docker·Compose 분리)
Path Identity Self-Check / Agent Self-Protection
Docker read + 제어 (start/stop/restart/remove)
Compose discovery (labels + fs scan + scan budget + ignore)
compose config 위임을 통한 project name 결정
project_uid identity + merge + name 충돌 감지
compose ps/pull/up/down/start/stop/restart/logs/config (project & service)
Config Drift Tier 1
파일 read/edit (화이트리스트, 단일 파일, atomic, validate, symlink 거부)
concurrent edit detection (sha256)
외부 파일 변경 감지 (polling)
pre-write / pre-restore 자동 snapshot
config backup 목록 / restore / retention / restore transaction journal
operation 모델 + 멱등성 + timeout + output tail + project exclusive lock
취소 모델 (SAFE / BEST_EFFORT_PARTIAL / BEFORE_COMMIT)
managed audit + observed audit (events, coalescing, rate limit)
Agent bounded disk WAL + Server canonical archive + Coverage Ledger
실시간 로그 relay + 취소 전파
실시간 metrics (viewer-scoped, 무저장)
offline/unknown/interrupted 상태 모델 + 재연결 복구
Web UI + 단일 바이너리 embed
disk budget + emergency reserve + DEGRADED_STORAGE
.env secret 취급 정책
```

### OPTIONAL

```
Browser ↔ Server access token
Agent 승인(pending) 모드
read-only discovery root 모드
사용자 정의 ignore 목록
Config Drift Tier 2 (--dry-run 미리보기, 출력 미해석)
restore 전 diff 미리보기
compose 플래그 노출 (--remove-orphans / --force-recreate / --pull)
Git/GitHub/GitLab webhook CD
named volume backup (일관성 미보장 명시)
docker system df / prune (얇은 래핑)
```

### FUTURE

```
metrics history / 시계열
discovery max_depth, discovery boundary 마커, .dockpilotignore
Native Agent 정식 지원
Key Rotation
원격 backup 스토리지, 알림, 인증/RBAC, CLI/TUI
audit 외부 export
bind mount 디렉터리 backup
symlink 허용 정책
container-level lock 세분화
socket-proxy / DOCKER_HOST 지원
```

### DO NOT BUILD

```
arbitrary shell / SSH 대체 / exec 터미널
host OS 메트릭 수집
전체 filesystem watcher / OS audit 연동
application-aware DB backup
자동 self-healing / 자동 재시도 / 자동 재적용
image build 플랫폼 / CI 파이프라인 에디터
Prometheus/Grafana/Loki 대체
Agent self-update
mTLS + 자체 CA + 인증서 로테이션
Kubernetes / Swarm orchestration
Project Lock force-release API
config-hash 재계산 / dry-run 출력 파싱으로 drift 자동 판정
Delta Coverage API
전송 프로토타입용 임시 Metric 이름
```

---

## 19. 기본값

프로토타입과 운영에서 튜닝 가능한 출발값이다.

| 항목 | 값 |
|---|---|
| WAL 크기 / 기간 상한 | 256MB / 14일 (먼저 도달하는 조건) |
| WAL fsync | 1초 또는 64KB (먼저 도달) |
| Operation result buffer | 24시간 / 500건. active operation은 절대 축출 안 함 |
| Operation output tail | 결과당 최대 64KB |
| Agent state dir 총량 | 2GB |
| Filesystem 여유 하한 | max(1GB, 5%) |
| Emergency Reserve | 64MB |
| 자동 snapshot 보존 | Project당 20개 (disk budget 우선) |
| 편집 파일 크기 상한 | 1MB |
| timeout — container.* | 60초 |
| timeout — compose.up | 15분 |
| timeout — compose.restart | 10분 |
| timeout — compose.down | 5분 |
| timeout — compose.pull | 45분 |
| timeout — file.write | 30초 |
| timeout — backup.create / restore | 5분 / 5분 |
| timeout — discovery.rescan | 10분 |
| cancel grace period | SIGTERM → 10초 → SIGKILL (process group) |
| stalled_warning 임계 | COMMITTING 진입 후 progress 없이 5분 |
| discovery 주기 | 5분 + on-demand + operation 후 targeted |
| scan budget | 200,000 디렉터리 / 60초 |
| stats 샘플 주기 | 2초 (viewer 있을 때만) |
| 브라우저 sparkline 버퍼 | 120 샘플 (약 4분, 서버 저장 없음) |
| heartbeat / offline 판정 | 30초 / 90초 |
| event coalescing 윈도우 | 5초 (동일 container+type) |
| Observed audit 생성률 상한 | 초당 20건 (초과 시 storm summary) |
| Managed audit | **rate limit 금지, 전량 기록** |
| Server operation retention | 90일 |
| Server audit retention | 365일 또는 10GB (먼저 도달). 경고 80%, aggressive 95%. 무제한은 명시적 opt-in |
| ACK 정체 경고 | 5분 |
| Agent Process RSS 목표 | 256MB |
| Agent Container hard limit | 512MB (설정 가능) |
| Server Process RSS 목표 | 512MB (Agent 20대 기준) |
| Server Container hard limit | 1GB |

---

## 부록 A. Transport Prototype 실행 사양

> 이 부록은 ADR의 일부가 아니라 전송 기술 확정을 위한 1회성 실험 사양이다. 확정 후 폐기 대상이다.

### A.1 목적

답하는 질문: 단일 Agent-initiated 지속 연결에서 Control / Durable Sync / Bulk 트래픽이 공존할 수 있는가. 느린 소비자 하나가 다른 클래스를 굶기지 않는가. Audit Sync가 지속 전진하는가. Memory가 상한 안에서 안정되는가.

답하지 않는 질문: Dockpilot의 기능적 정확성, Docker/Compose 동작, Discovery 성능, 파일 안전성, UI.

### A.2 검증 가설

**후보 A (Reverse gRPC)**: HTTP/2는 스트림 단위 flow control과 함께 **연결 단위 window**도 가진다. 정지한 Log Stream이 연결 window를 소진하면 다른 스트림까지 정체될 수 있다. 스트림 단위 격리는 얻지만 연결 단위 HOL blocking은 자동으로 해결되지 않는다.

또한 HTTP/2 우선순위 체계는 실질적으로 사용하지 않으므로 **P0/P1 보호는 두 후보 모두 애플리케이션 레벨에서 구현해야 한다.** 이 점에서 두 후보는 동등한 조건에서 비교된다.

**후보 B (WebSocket + Application Multiplexing)**: flow control과 스케줄러를 직접 구현하므로 credit 회수 누락으로 인한 교착, 무한 버퍼링, 취소 시 goroutine/credit 누수, 스케줄러 기아(P3가 P1을 굶김)가 실패 지점이다.

### A.3 후보 정의

**후보 A**

```
1. Agent가 Server로 TLS dial
2. Server가 accept한 net.Conn을 보관
3. Server가 해당 Conn만 반환하는 custom dialer로 gRPC client 생성
4. Agent가 단일 Conn Listener 위에서 gRPC server 실행
5. 연결 방향과 RPC 방향이 반대
```

의존성: `google.golang.org/grpc`, `google.golang.org/protobuf`

**후보 B**

```
1. Agent가 Server로 WSS 연결
2. 단일 연결 위에 자체 프레이밍
3. 스트림 ID 기반 다중화
4. credit 기반 스트림 단위 flow control
5. 클래스 기반 송신 스케줄러

프레임: stream_id | frame_type (OPEN|DATA|CREDIT|CLOSE|CANCEL|PING)
       | traffic_class | length | payload
```

의존성: WebSocket 라이브러리 1종 + 동일한 protobuf

### A.4 공통 최소 구현 범위

두 후보는 완전히 동일한 논리 인터페이스를 구현하며, **코드는 이 인터페이스 뒤에서만 갈라진다.**

```
P0 Control
  Register(agent_id, protocol_version) -> session
  Heartbeat() -> capability
  CancelOperation(operation_id, reason) -> ack
  OperationProgress (stream, Agent -> Server)

P1 Durable Sync
  SyncAudit (bidirectional stream)
    Agent -> Server: audit records
    Server -> Agent: AckAudit(cursor, coverage_revision_seen)
  GetAuditCoverage() -> snapshot

P2 Interactive Query
  Echo(payload_size) -> payload      (Docker query 대체, 지연 측정용)

P3 Bulk Interactive
  StreamLogs(stream_id, byte_rate)   (stream, Agent -> Server)
  OperationOutput                    (stream, Agent -> Server)

P4 Disposable Live
  StreamStats(targets, interval)     (stream, Agent -> Server)
```

이 목록 밖의 기능은 구현하지 않는다.

**공통 계약에 두는 개념**: Session, Logical Message, Traffic Class, Stream, Cancellation Semantics.

**공통 계층에 넣지 않을 것**: HTTP/2 stream, WebSocket frame, header/trailer, HTTP status, window size, credit counter, stream weight, channel number, gRPC metadata, transport-specific error code.

Backpressure도 특정 메커니즘으로 표현하지 않는다. 공통 계약은 **결과만** 요구한다.

```
- Logical Stream별 순서 보장
- Bounded Memory
- 느린 Stream이 다른 Stream을 막지 않음
- Cancel 전파
- Stream 종료 후 Resource 회수
- P0/P1 Non-starvation
- Terminal Outcome 정확히 한 번 관찰
```

`credit`, HTTP/2 window, WebSocket scheduler는 각 Transport Adapter 내부 구현이다.

**공통 계약 리뷰 기준**: 후보 A와 B 중 어느 구현을 삭제하더라도 공통 패키지의 타입명과 의미가 어색하지 않아야 한다. 공통 계층에 `HTTP2StreamWindow`, `WebSocketChannel`, `TrailerStatus` 같은 것이 등장하면 실패다.

### A.5 스텁 경계

```
Docker Engine       : 호출하지 않음
Compose             : 실행하지 않음. Simulated Operation 사용
                      (지속 시간 고정, stdout 라인 생성률 고정, 종료 코드 고정)
Audit WAL           : 디스크 WAL 미구현. 고정 크기 레코드 in-memory queue로 대체
                      (디스크 성능이 아니라 전송 성능을 측정)
Server Audit Store  : SQLite 없이 카운터와 cursor만 유지
인증                : TLS만. join token / credential 검증 없음
Discovery/File/Backup: 구현하지 않음
```

실험 종료 후 1회에 한해 실제 `docker compose up`으로 smoke 확인을 수행한다. 판정 기준이 아니라 시뮬레이션이 현실과 극단적으로 어긋나지 않았는지 확인하는 용도다.

### A.6 부하 시나리오

**시나리오 1 — 단일 Agent 간섭 시험 (주 판정)**

```
Agent 1대
Log Stream 4개, 각 200 KB/s (200B 라인, 약 1000 lines/s)
Simulated Operation 1건, 지속 120초, stdout 50 lines/s
Stats Stream 1개, 대상 6개, 2초 간격, 샘플당 약 1KB
Audit 생성 20 records/sec, 레코드 512B
Query(Echo) 2초마다 1회

T+0    전체 스트림 시작
T+120  warmup 종료, 측정 시작
T+180  Log Stream #1 소비자 정지
T+360  Log Stream #1 소비자 재개
T+420  Operation 시작
T+540  Operation 종료
T+600  종료
```

**시나리오 2 — Audit 부하**: 시나리오 1과 동일하되 Audit 생성률만 20 / 50 / 100 records/sec 3단계, 각 5분 steady-state. Audit Sync 처리율이 생성률을 따라가는 한계점 확인.

**시나리오 3 — Server 확장**: 시뮬레이션 Agent 20대, Agent당 Log Stream 1 + Stats Stream 1 + Audit 5 records/sec, 10분 유지. Server Memory의 Agent 수 대비 증가 특성 및 Agent당 incremental memory 산출.

**시나리오 4 — 취소 및 누수**: 스트림 개설/취소 200회, Operation 개설/취소 50회, 각 회차 사이 5초 대기. goroutine / credit·window / memory 복귀 확인.

### A.7 통제 변수

```
동일 머신, 동일 커널, 동일 CPU 제한, GOMAXPROCS 고정
동일 Go 버전, GC 설정 동일 (GOGC 기본값 고정)
동일 TLS 버전/cipher suite, 동일 인증서, 동일 직렬화 형식(protobuf)
동일 레코드/라인 크기
의미가 대응되는 버퍼는 동일한 초기값 (A: 초기 window size / B: 초기 credit)
  — 실패를 감추기 위해 버퍼를 키우지 않는다

cgroup 조건 동일:
  memory.max, memory.high (미사용이면 양쪽 모두 미설정),
  memory.swap.max, CPU Limit, FD Limit
  ※ Docker의 --memory-swap=0 또는 Compose의 memswap_limit: 0을
     swap 비활성 의미로 사용하지 않는다.
     swap 비활성은 cgroup memory.swap.max = 0을 명시적으로 적용하거나
     Memory+Swap Limit을 Physical Memory Limit과 동일하게 설정한다.
     "기본값이 0일 것"이라고 가정하지 않는다.

네트워크: 조건 1 loopback / 조건 2 RTT 20ms + loss 1% (tc netem)
         두 조건 모두에서 시나리오 1·2 수행
반복: 각 시나리오 3회, 중앙값으로 판정
     3회 중 1회라도 OOM Kill 발생 시 해당 시나리오 불합격
```

### A.8 계측

**Metric 이름을 프로토타입 전용으로 새로 만들지 않는다.** §11.12와 §14.4의 운영 지표를 그대로 사용한다(prototype/production 의미 불일치 방지, 임시 metric 제거 비용 방지, 동일 dashboard·assertion 재사용).

```
공통:
  audit_generated_total / audit_synced_total / audit_sync_cursor
  audit_sync_lag_records / audit_sync_lag_seconds
  audit_ack_watermark_stalled_seconds
  operation_progress_event_latency_ms / cancel_ack_latency_ms
  query_echo_rtt_ms (p50/p95/p99)
  log_bytes_sent_total / log_bytes_dropped_total
  stats_samples_sent_total / stats_samples_dropped_total
  operation_output_truncated_total

Agent/Server: §14.4의 RSS, Go Heap, goroutines, cgroup memory 지표 전체
후보별 추가: A는 연결/스트림 window 잔량, B는 클래스별 송신 큐 길이와 스트림별 credit 잔량

샘플링: 1초 간격, 전 구간 기록
```

**합성 Audit 생성기 (test-only)**: 실제 Docker Events로는 안정적 부하를 만들 수 없다. 생성률 10/20/50/100 events/sec, payload small/medium, mode managed-like/observed-like. 프로덕션 실행 모드에 노출하지 않는다.

### A.9 합격 판정

시나리오 1 기준, T+180 ~ T+360 정지 구간을 포함해 판정한다.

```
1. Operation 지연 없음
   완료 시각이 정지 구간 유무에 관계없이 기준 대비 5% 이내
2. Control 응답성
   cancel_ack_latency_ms p99 <= 500ms
   operation_progress_event_latency_ms p99 <= 1000ms
3. Audit 전진
   정지 구간 전체에서 audit_sync_cursor 계속 증가
   audit_ack_watermark_stalled_seconds 최대값 <= 10초
4. Audit 처리율
   5분 steady-state에서 audit_synced_rate >= audit_generated_rate
   audit_sync_lag_records의 기울기가 지속 양수가 아닐 것
5. Stats
   backlog 누적 없음, latest-wins 동작
6. Log 격리
   정지한 #1을 제외한 #2~4의 처리율이 정지 전 대비 10% 이내 유지
7. Memory
   §14.4의 판정 계약을 그대로 적용
   Agent Process RSS <= 256MB, Server Process RSS <= 512MB (시나리오 3)
   oom = 0, oom_kill = 0
   memory.events.max는 실패 조건이 아님
8. 누수 (시나리오 4)
   goroutine 수가 기준선의 105% 이내 복귀
   window/credit 잔량이 초기값으로 복귀
   RSS가 기준선의 120% 이내 복귀
```

모든 항목을 통과해야 합격이다.

### A.10 동점 시 2차 판정

```
1. 전송 계층 구현 규모 (인터페이스 뒤 후보별 코드 라인 수) — 적은 쪽
2. 직접 구현해야 하는 정합성 로직의 양 (flow control / 취소 / 재연결) — 적은 쪽
3. 의존성 (모듈 수, Apache-2.0 호환, copyleft 회피) — 적고 단순한 쪽
4. 관측 가능성 (표준 도구로 스트림 상태 확인, 장애 원인 파악 난이도)
5. 프로토콜 버전 협상 및 skew 처리 용이성
6. 네트워크 조건 2에서의 성능 저하 폭 — 적은 쪽

1번과 2번이 충돌하면 2번을 우선한다.
직접 구현한 정합성 로직은 장기 유지보수 비용이 더 크다.
```

### A.11 실패 시 절차

```
한 후보만 합격     → 해당 후보 확정
두 후보 모두 불합격 → 버퍼 확대나 상한 완화로 재시도하지 않는다
                    §5.3의 두 연결 후퇴점을 적용해 동일 시나리오 재수행
                    (§5.3의 Session 결속 규칙도 함께 구현·검증)
후퇴 구성에서도 불합격
                  → 부하 목표치 재검토
                    Log Stream 동시 개수 상한을 제품 제약으로 도입하는 방안 검토
                    이 경우에만 ADR을 다시 연다
```

### A.12 실행 순서

```
1. transport-neutral logical types
2. Session / Stream interface
3. 공통 conformance tests
4. Synthetic workload generator (audit / logs / stats / operation)
5. 공통 metrics와 acceptance assertions
6. Candidate A 구현
7. Candidate B 구현
8. 동일 cgroup/CPU/FD/부하 조건으로 비교
9. 단일 연결 합격 여부 판정
10. 둘 다 실패하면 두 연결 후퇴점 검증
```

**첫 번째 커밋부터 후보 A의 구조를 반영해서는 안 된다.**

### A.13 산출물과 폐기 대상

```
산출물:
  후보 A / B 프로토타입 (Agent, Server)
  공통 논리 인터페이스 정의
  부하 드라이버, Synthetic Audit Generator
  계측 수집 및 리포트 생성기
  시나리오별 원시 측정 데이터
  판정 리포트 (항목별 합격 여부, 실패 지점과 관측된 원인, 2차 판정, 최종 권고)
  결정 메모 (확정된 전송 기술, 후퇴점 적용 여부, ADR의 어느 절을 갱신하는지)

제품 코드로 이관하지 않음:
  Synthetic Audit Generator, 부하 드라이버, 시뮬레이션 Operation,
  스텁 Audit Queue, 스텁 Server Audit Store, 탈락한 후보의 전송 구현

이관 대상:
  공통 논리 인터페이스 정의, 선택된 후보의 전송 구현,
  계측 지점 정의, 클래스 기반 송신 정책 (P0~P4)
```

Git/GitHub 보관은 제품 코드 이관 여부와 별개로 다음처럼 고정한다. 프로토타입
소스, 실행·리포트 생성 스크립트, 완료 표식, 판정 리포트, 결정 메모, 환경 예외
기록과 release asset checksum은 일반 Git에 보존한다. trial별 JSONL·로그·설정,
계측 원본과 보존 실행 파일을 포함한 공식 원시 산출물 전체는 하나의 immutable
GitHub Release asset으로 묶고 일반 Git history에는 넣지 않는다. Release asset은
버전, SHA-256과 원본 경로를 저장소 문서에 기록하며, checksum 검증 전에는 판정
근거로 사용하지 않는다. 비밀키와 자격 증명은 어느 보관 채널에도 포함하지 않는다.

계측 지점은 프로토타입 전용이 아니라 운영에서도 동일한 이름으로 유지한다. **프로토타입에서 실패를 관측한 지표가 운영에서 사라지면 같은 문제를 다시 진단할 수 없다.**

### A.14 실행 결과 (2026-08-15)

- 공식 행렬 78/78 trial과 26개 3회 반복 group의 집계를 완료했다.
- Candidate A(Reverse gRPC)는 13/13 group을 통과했다.
- Candidate B(WebSocket)는 12/13 group을 통과했으며 Scenario 3 scale의 `workload and logical-contract integrity`가 1/3 통과로 실패했다. 실패한 두 trial에서는 Echo 처리율이 0이었다.
- 최종 권고는 `REVERSE_GRPC`이며 두 연결 후퇴점 trigger는 `false`다.
- 실제 `docker compose up` smoke는 120초 동안 200-byte Operation output 6,000/6,000 lines와 완료 marker를 확인했다. 이 결과는 `acceptance_input=false`로 전송 판정에는 사용하지 않았다.
- 환경 예외: WSL 업데이트로 마지막 WebSocket 3개 trial 전에 커널이 `6.6.87.2`에서 `6.18.33.2`로 변경됐다. 전체 재실행 대신 이 예외를 수용했으며 상세 기록은 공식 artifact root의 `ENVIRONMENT-EXCEPTION.md`에 둔다.

### A.15 Prototype과 Integration Test 분리

Transport Prototype은 Audit WAL을 in-memory로 스텁하므로 운영 대비 page cache 발생량이 작다. **프로토타입이 512MB에서 통과했다는 사실이 운영에서도 512MB로 충분하다는 근거가 되지 않는다.**

```
Integration Resource Test (별도 단계):
  실제 bounded disk WAL, record별 write(), 1초/64KB fsync
  Configuration Snapshot, Backup Write
  실제 docker compose CLI child process
  실제 Discovery Scan
  실제 cgroup Hard Limit

초기 Hard Limit (Agent 512MB / Server 1GB)의 최종 적정성은 여기서 재확인한다.
```

---

## 부록 B. 남은 작업과 미확정 항목

### B.1 남은 작업

Transport Prototype, 동시성/Memory/Audit non-starvation 검증, 조건부 후퇴점 판정, 전송 기술 확정은 2026-08-15 완료했다. Reverse gRPC 단일 연결이 합격했으므로 두 연결 후퇴점 검증은 실행 대상이 아니었다.

```
완료:
  구현 계획 수립 → docs/implementation-plan.md
  §19 및 B.2 값을 provisional v1 defaults로 코드화

남음:
  구현 계획 Phase 1~7 제품 구현
  A.15 Integration Resource Gate에서 운영 기본값 실제 튜닝·확정
  구현 계획 Phase 9 v1 release gate
```

기본값의 수치와 상호관계 검증은 `internal/config`가 담당하고, 검증 상태와
실제 자원 행렬은 `docs/defaults-validation.md`에 기록한다. 단위 테스트에서
숫자가 일치한다는 사실만으로 `validated`라 부르지 않으며, 실제 WAL·Backup·
Compose·Discovery를 함께 실행하는 A.15를 통과해야 최종 운영 기본값으로 승격한다.

프로토타입 이후 변경 가능한 것은 전송 기술, Memory Limit 수치, Timeout, Retention, Disk Budget, Scan Budget, Sampling Interval뿐이다.

### B.2 미확정 (권고안만 있음)

**Credential lifetime과 갱신 임계** — Credential lifetime은 Agent가 offline으로 버티도록 설계한 기간보다 충분히 길어야 한다. WAL retention 14일은 "그 정도 끊겨 있어도 audit을 잃지 않는다"는 의미인데, credential lifetime이 짧으면 그 offline 내구성이 인증 만료로 무효화된다(WAL은 멀쩡한데 붙지를 못한다).

권고: lifetime 90일 이상, 잔여 수명 50% 지점부터 갱신 시도. 만료된 채 복귀하면 join token 재등록이라는 규칙은 그대로 유지.

### B.3 프로토타입 단계 코드 작성 범위 (완료)

Transport Prototype 단계에서 코드 작성 범위는 다음으로 제한했고, 이 단계는 2026-08-15 완료했다.

```
허용:
  transport-neutral Go interface / message types
  conformance test suite
  synthetic audit / logs / stats / operation workload
  Candidate A adapter, Candidate B adapter
  공통 metrics / assertions
  cgroup-constrained benchmark harness

작성하지 않음:
  Docker API 연동, docker compose 실행, 실제 Agent 등록,
  SQLite, 실제 Audit WAL, 실제 Backup, Web UI, File editing,
  Dockpilot production server/agent
```

전체 Dockpilot 구현은 다음 단계의 구현 계획을 수립한 뒤 시작한다. 프로토타입 전용 workload, driver, stub queue/store와 탈락한 WebSocket adapter는 제품 코드로 이관하지 않는다.
