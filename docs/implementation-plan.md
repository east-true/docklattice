# Dockpilot v1 implementation plan

Status: active  
Authority: `docs/architecture.md`  
Scope: v1 `CORE` only; `OPTIONAL`, `FUTURE`, and `DO NOT BUILD` are not release
requirements.

## Completion rule

v1 is complete only when every phase below has its implementation, focused
tests, failure-path evidence, and release-gate evidence. Appendix A proves the
transport selection only; it does not prove product behavior or resource
fitness.

The values in architecture section 19 and the credential recommendation in
Appendix B.2 are adopted as **provisional v1 defaults**. Their behavioral
meaning is fixed now. Workload-dependent values become **validated defaults**
only after the Integration Resource Gate passes with production components.

## Phase 0 — Prototype closure

Status: complete

- Reverse gRPC is the selected single Agent-initiated transport.
- The two-connection fallback is inactive.
- Preserve the neutral transport contract, P0-P4 scheduler, selected gRPC
  adapter, conformance suite, and operational metric names.
- Keep synthetic workload, acceptance driver, stub stores, and WebSocket
  adapter outside product code.
- Retain compact reports in Git and publish the complete raw evidence as one
  immutable release asset with a repository-tracked SHA-256.

Evidence: Appendix A.14, the official final report and decision memo, and
`go test ./...` for the prototype packages.

## Phase 1 — Product foundation and configuration

Status: implementation complete; release evidence pending

- Define one product binary with `server` and `agent` modes; the Container
  Agent is the only documented v1 Agent deployment.
- Adopt typed provisional defaults for memory, timeouts, retention, disk and
  scan budgets, sampling, heartbeat, and credentials.
- Separate product protocol/schema from the prototype package.
- Define Server SQLite, separate Server Identity State, and Agent state-dir
  schema/migrations before writing feature stores.
- Keep Server bind at `127.0.0.1` unless explicitly opted into a public bind.

Gate: exact-value and validation tests for defaults, clean build of both
modes, restart/migration tests, and no dependency on external operational
services.

## Phase 2 — Identity and connection lifecycle

Status: complete. Package recovery matrix
(`internal/serverbootstrap/recovery_matrix_test.go`); Agent-replacement case in
the clean-host install E2E (Agent container removed and recreated with no Join
Token, reconnecting with an identical Agent ID, project UID, and backup
metadata); real-container recovery matrix for all three Server-side loss
outcomes ([`docs/release/recovery-matrix-e2e.md`](release/recovery-matrix-e2e.md)).

- Implement Server identity, signing keys, archive generation, join tokens,
  Agent credentials, and the durable revocation ledger.
- Implement 90-day credentials and renewal beginning at 50% remaining life;
  activate an atomically stored new credential before revoking the old one.
- Implement registration, heartbeat, separate connection/Docker/Compose
  capabilities, reconnect backoff with jitter, and N-to-N-1 protocol skew.
- Implement archive identity binding, automatic forward rebind, and rollback
  and invariant-violation rejection.
- Productize the selected reverse-gRPC adapter without the prototype ALPN or
  unauthenticated handshake.

Gate: identity-loss/database-loss recovery matrix, credential fault tests,
archive rebind/rollback tests, network disconnect/reconnect E2E, oversized
message rejection, and transport conformance.

## Phase 3 — Agent boot safety and Docker discovery

Status: Server Tier-1 drift reconciliation complete
(`internal/projectmodel`, `internal/serverapi`); real-Docker/Compose fixture
matrix pending

- Implement identical-path bind-mount self-check and safe capability downgrade.
- Support explicitly configured read-only discovery roots as a v1 deployment
  mode; they report `fs_write:false` and keep read-only discovery available.
- Identify and protect the Agent container and its Compose project; fail closed
  for mutations when identity is uncertain.
- Implement Docker read/control adapters and minimum Engine-version checking.
- v1 declares Docker Engine 19.03 / Engine API 1.40 as the minimum. The
  official Moby client negotiates the highest mutually supported API and Agent
  startup reports Docker capability unavailable below that boundary.
- Implement label and filesystem discovery, bounded scans, project UID/name
  resolution through Compose, merge rules, collision handling, and drift Tier 1.

Gate: real Docker/Compose fixtures; missing, nested, volume, tmpfs, symlink-loop,
hidden-path, scan-budget, project rename, and collision tests; self-stop/remove
and self-project-down rejection. Explicit read-only roots and safety-degraded
roots must remain distinguishable in capability reasons.

## Phase 4 — Operation and cancellation engine

Status: package operation-engine matrix complete (project lock, idempotency,
result ring, cancel/disconnect separation); browser/network/process recovery
E2E matrix pending

- Implement the full operation state machine, phase/revision, Agent-owned
  project lock, idempotency, timeout, result ring, and 64 KiB output tail.
- Implement restart/disconnect reconciliation without treating browser or
  transport disconnect as mutation cancellation.
- Implement SAFE, BEST_EFFORT_PARTIAL, and BEFORE_COMMIT cancellation.
- Serialize commit entry and cancellation on the same operation mutex.
- Run Compose with fixed argv and no shell, in its own process group; always
  drain output and determine success only from exit status.

Gate: concurrent lock/idempotency tests, cancel/commit race tests, process-tree
termination, timeout matrix, Agent/Server/browser/network restart fault tests,
and output truncation evidence.

## Phase 5 — Safe files and configuration backup

Status: package crash matrix complete for both restore journal phases
(`PREPARING`, `COMMITTING`); release-gate crash evidence pending

- Accept only `project_uid + relative_path`; enforce file/location allowlists,
  traversal and symlink/TOCTOU defenses, size/UTF-8 limits, and SHA conflicts.
- Use same-directory temporary files, file fsync, atomic rename, and directory
  fsync; validate Compose before replacement.
- Implement external-change polling without automatic application.
- Implement pre-write/pre-restore snapshots, local backup manifests, retention,
  restore locking, rollback, durable restore journal, and boot recovery.

Gate: traversal/symlink-swap fuzz and race tests, invalid-Compose preservation,
concurrent-edit conflicts, mode/hash verification, and a crash matrix at every
restore replacement point.

## Phase 6 — Durable audit and storage pressure

Status: global eviction executor complete
(`internal/agentstorage/eviction.go`, `internal/diskbudget`); quota/free-space
fault matrix complete against a disposable size-limited tmpfs
(`internal/agentstorage/quota_matrix_linux_test.go`)

- Implement the bounded segmented Agent WAL with framed checksummed records,
  per-record writes, 1 s/64 KiB fsync, tail recovery, incarnations, clean-close,
  gaps, and continuity uncertainty.
- Implement coverage snapshots/revisions, range-aware atomic ACK, typed
  cursor-behind-floor recovery, and archive rebind semantics.
- Implement Server canonical audit storage, coverage ledger sources, cursor
  recomputation, Agent claim/effective-gap separation, reconciliation, and ACK
  stall observation.
- Apply the exact disk eviction order, emergency reserve, admission checks,
  degraded-storage reasons, and recovery hysteresis to every write path.
- Continue current Audit ingest under Server high-water pressure and record
  retention as coverage evidence.

Gate: torn-write/checksum recovery, incarnation and gap property tests,
coverage/ACK concurrency tests, duplicate ingest, archive restore/regression,
quota/free-space fault injection, manual-backup protection, and a verifier that
every unavailable range has an allowed source.

## Phase 7 — Live logs, metrics, API, and Web UI

Status: package relay/stats matrix complete (slow-consumer isolation, stream
cancellation, bounded rings); browser E2E, accessibility, and bounded-memory
soak pending

- Connect project/service Compose operations and bounded per-stream log relay.
- Implement viewer-scoped Docker stats, latest-wins delivery, one Server sample,
  and the browser-only 120-sample ring; persist no log or metric history.
- Implement the Server API and the architecture section 16 screens, capability
  disabled states, destructive-action wording, and secret masking.
- Embed the frontend build in the product binary.

Gate: slow-consumer isolation, stream cancellation, zero-viewer collection
tests, bounded-memory soak, UI/API E2E and accessibility checks, capability
snapshots, and secret non-disclosure tests across DB, API, logs, and Audit.

## Phase 8 — Integration Resource Gate

Status: passed 2026-08-18. Three-trial production-image matrix returned
`status=PASS` in every trial (`docs/release/resource-gate.md` records the environment,
per-trial resource summaries, and the Appendix A A.9 bounds measured).
`operation_progress_event_latency_ms` and the Appendix A prototype acceptance
items 1/4/5/6 are not measured by this gate; the one-hour and overnight soaks
remain outstanding.

Run production Agent and Server together under real cgroup limits with:

- the real disk WAL and fsync policy;
- configuration snapshots and backup writes;
- real `docker compose` child processes;
- a real bounded discovery scan;
- the Appendix A traffic mix and connection churn.

Collect process RSS, Go heap, cgroup anon/file/current/peak/events/pressure,
goroutines, FDs, bounded buffers, and P0/P1 latency. Repeat each case three
times. Any OOM/OOM-kill, persistent resource trend, broken P0/P1 guarantee, or
contract violation fails the gate. `memory.events.max` alone is diagnostic, not
a failure.

This gate validates or adjusts only the architecture-authorized values:
memory limits, timeouts, retention, disk budget, scan budget, and sampling
interval. In particular it must validate Agent 512 MiB and Server 1 GiB hard
limits rather than inheriting the prototype result.

Harness and current execution status: [`docs/release/resource-gate.md`](release/resource-gate.md).

## Phase 9 — v1 release gate

Status: passed 2026-08-19 at revision `f1d4087`. The project status is v1
complete.

- Race suite: 739 tests, 54 packages, 0 data races.
- Server persistence schema and filesystem audits
  (`internal/serverstore/persistence_audit_test.go`,
  `internal/serverbootstrap/filesystem_audit_test.go`).
- Release-scope audit over the release binary's dependency graph
  (`scripts/verify-release-scope.sh`, 29 checks, 40 packages): no OPTIONAL,
  FUTURE, or DO NOT BUILD behaviour reaches `./cmd/dockpilot`.
- Go dependency license material (`scripts/generate-license-inventory.sh`, 33
  modules).
- Reproducible multi-platform release build: two independent runs of
  `scripts/build-release-images.sh` produced byte-identical `linux/amd64` +
  `linux/arm64` OCI archives for both targets, with no target-architecture
  emulation required on the build host
  ([`docs/release/distribution.md`](release/distribution.md)).
- Clean-host container installation E2E
  ([`docs/release/clean-host-install-e2e.md`](release/clean-host-install-e2e.md)).
- Real-container recovery matrix for all three Server-side loss outcomes
  ([`docs/release/recovery-matrix-e2e.md`](release/recovery-matrix-e2e.md)).
- Adversarial hardening matrix: Agent and Server `SIGKILL`, an operation killed
  mid-flight, a network partition, a cancelled Compose run, concurrent and
  racing project writes, a rolled-back Server database, and an Agent filesystem
  too small to hold its WAL
  ([`docs/release/hardening-matrix-e2e.md`](release/hardening-matrix-e2e.md)).
- Abuse matrix over the untrusted surface: path escapes, secret exposure,
  operation ID rebinding, a replayed Join Token, a foreign CA, a tampered backup
  archive, a discovery root at a non-identical path, the project lock and result
  ring bounds, self-directed container and Compose operations, malformed and
  oversized requests, and a Compose project name collision
  ([`docs/release/abuse-matrix-e2e.md`](release/abuse-matrix-e2e.md)).
- Operator documentation: install, backup/restore, identity-state recovery,
  Agent upgrade, degraded-storage recovery
  ([`docs/operations/degraded-storage.md`](operations/degraded-storage.md)), and
  supported/unsupported environments
  ([`docs/operations/supported-environments.md`](operations/supported-environments.md)).

Only this gate changes the project status to v1 complete.
