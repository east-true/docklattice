# Dockpilot UI — Implementation Gap Report

**Status:** Approved implementation gate and Compose build policy; slices A and
B implemented and validated, slice C deferred.

**Initial comparison at:** `docs/final-ui-design@a4205c0`

**Revalidated against:** merged branch revision
`58ad1dc50af1fbfda2550c2fd88c7aba7a6a890b`, including
`main@22f21678f0400b1a81a1ff9fab083a90f011774a`

**Scope:** `DESIGN.md`, `docs/design/web-ui.md`,
`docs/design/web-ui-acceptance.md`, frozen product/interface contracts, and the
current Server, Agent, API, and `internal/webui/**` implementation.

This report is the implementation gate required by
`docs/prompts/implementation-handoff.md`. A design request does not authorize an
API, protobuf, persistence, Docker-operation, or product-policy change.
Implementation was constrained to the externally approved slice A then slice B
boundary below. Slice C remains excluded; the separately approved Compose build
policy is the only added mutation-policy decision.

## Classification

| Class | Meaning |
|---|---|
| **Supported** | The current Agent/Server/API contract already returns or performs the required fact/action. The final UI still needs to render it correctly. |
| **Derivable** | The browser can derive or present it from current authoritative fields without parsing free text or inventing state. No backend semantic change is required. |
| **Minimal extension** | An additive, bounded Agent/Server/API field or endpoint is required. Existing authority and storage rules can remain unchanged. |
| **Product decision** | The requested behavior is unsupported, optional/P1, or would choose unresolved policy. It must not be implemented without explicit approval. |

“Minimal extension” identifies the smallest expected contract change for
review. Only the items assigned to externally approved slices A and B are
approved by this gate; slice C and product-decision items are not.

## Pre-implementation baseline

The current browser assets are the pre-final-design implementation:

- one long page with top-level Dashboard/Hosts/Compose/Logs/Metrics/Audit/Backups links;
- dark-only card grids rather than the required light, host-first workbench;
- manual Agent/Project/Container ID forms rather than route-driven object context;
- a per-Container CPU chart, which conflicts with the final no-chart Live
  Metrics design;
- no client-side route model, Inspector, global search, or persistent Operation
  Center.

The backend is substantially ahead of that UI. It already has strict HTTP
routes for dashboard/host snapshots, Docker inventory, Compose reads and
operations, safe files, backups, stored Audit/Activity, live logs/stats, and the
host-wide Live Metrics matrix. Replacing the UI must preserve those contracts;
it must not turn live data into stored data or parse CLI/error text into facts.

## 1. App shell, navigation, and visual contract

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| Dockpilot wordmark, Search, Home, registered Docker hosts only | **Derivable** | `GET /api/v1/dashboard` returns all registered hosts; shell composition is client-only. |
| Current Agent session availability in sidebar | **Supported** | Use `Host.capabilities.connection.enabled` as the current availability fact. `Host.state` is only the session lifecycle (`ACTIVE`, `OFFLINE`, or `CLOSED`) and must not make a failed heartbeat look connected. |
| Current session source IP, only while a session exists | **Minimal extension** | The registry exposes no peer/source address to `webui.Host`. Capture the gRPC peer address in current in-memory session metadata; never persist it. |
| Exact host and Compose-project tabs | **Derivable** | Client routing only; Service must remain a filter/context, not a route depth. |
| Explicit first-column object links | **Derivable** | Client routing and semantic HTML only. No whole-row-only navigation is needed. |
| Route-aware desktop Inspector; full page when narrow | **Derivable** | URL/query-state and responsive behavior are client-only once object detail endpoints exist. |
| Back/refresh restores Inspector selection | **Derivable** | Browser history/query state; missing objects must resolve to a real not-found/unavailable state. |
| Light palette, density, typography, tables, no gradients/badge spam | **Derivable** | CSS/markup replacement only. Current dark card UI must be removed. |
| Checked/live/stored/last-known time labels | **Derivable** for response/sample times; **Minimal extension** for per-host heartbeat observation | Audit events, scan timestamps, metrics sample timestamps, and file mtimes exist. Dashboard has no per-host heartbeat/checked timestamp; browser receipt time may label the whole snapshot but not an authoritative Agent observation. |

## 2. Home

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| Search, compact state filters, Needs attention, Docker-host table | **Derivable** | Dashboard hosts/projects contain the required base facts. These are controls/tables, not KPI cards. |
| Host Agent, Docker Engine, Docker Compose, Discovery columns | **Supported** | Agent availability comes from `capabilities.connection.enabled`; Docker/Compose/Discovery use their own capabilities and `project_scan`. `Host.state` may be secondary lifecycle evidence only. Enabled capability reasons remain warnings rather than disabled state. |
| Engine unavailable, Compose unavailable, discovery failure/truncation | **Supported** | Capability and `ProjectScan` fields carry exact state/reason. |
| Restore recovery, collision, missing/stale project, config changed/no baseline | **Supported** | `Project` exposes these explicit flags and drift vocabulary. |
| Host Audit gap/continuity in Needs attention | **Derivable** | A bounded `GET /hosts/{id}/audit?limit=1` returns host-wide coverage even offline. The UI must show partial loading if one coverage request fails. A later dashboard aggregate endpoint is an optimization, not required semantics. |
| Optional collapsed Compose-project disclosure | **Derivable** | Dashboard projects carry `agent_id`; group by host without inventing inventory. |
| No host may disappear when its live probe fails | **Supported** | Dashboard reconciliation keeps registered hosts and reports capabilities unavailable. |
| “Checked” timestamp for the whole Home snapshot | **Derivable** | Use browser response time and label it as request completion, not Agent observation. |

## 3. Global Search

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| Immediate Docker-host search | **Derivable** | Search `Dashboard.hosts` by display name and stable Agent ID. |
| Immediate Compose-project search with host/path context | **Derivable** | Search `Dashboard.projects` by name, UID, and working directory; join host by `agent_id`. |
| Known deterministic exception in a result | **Derivable** | Use explicit capability/project flags only. Do not search arbitrary reason/error text as state. |
| Cross-host live Container search with partial coverage | **Product decision** | No global runtime index exists. This is an optional live fan-out feature; coverage semantics and concurrency bounds must be adopted before implementation. |

## 4. Global Operation Center

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| Create, fetch, and cancel one Operation | **Supported** | `POST /api/v1/operations`, `GET /api/v1/agents/{agent}/operations/{id}`, and cancel are implemented. |
| Status, phase, revision, partial-effects flag | **Supported** | Exact Agent-authoritative fields are mirrored by the Server. `unknown` is non-terminal and must stay unknown. |
| Bounded `Output tail` and truncation evidence | **Supported** | `output_tail` and `output_truncated` are persisted and returned. It must never be labeled full output. |
| Global active/recent Operation listing | **Minimal extension** | The Server stores operations but exposes no list endpoint. Add a bounded/cursor-based Server read endpoint; do not query every Agent or create a new history store. |
| Operation context: kind, Agent, project, target | **Minimal extension** | The Server row already stores these facts, but `webui.Operation` omits them. Add them to the list/read view. |
| Requested/started/finished times and accurate elapsed duration | **Minimal extension** | Server has `requested_at`; Agent records all three times, but the transport/API response omits them. Add optional timestamps without changing lifecycle semantics. |
| Phase stepper | **Derivable** | Render the frozen phase order and current phase/revision. Do not invent percentage completion. Operations without COMMITTING must not imply that phase will occur. |
| Whether Cancel is currently offered | **Derivable** after kind is exposed | The kind-to-cancel-mode mapping and terminal/COMMITTING rules are frozen. The actual cancel response remains authoritative. |
| Persistence across navigation | **Derivable** | One application-level store plus polling/list refresh. Navigation or browser disconnect must not cancel mutation. |
| Long-lived history link | **Derivable** | Route to stored Audit using the returned `operation_id`; no second operation-history product is needed. |

## 5. Host Summary

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| Agent connection and capabilities | **Supported** | Connection, Docker, Compose, Discovery, Metrics, operation recovery, FS read/write are exposed with reasons. |
| Last completed discovery scan and truncation evidence | **Supported** | Timestamp, stop reason, directories visited, and last path are present. |
| Container total/running and CPU/memory capacity | **Minimal extension** | `MatrixHostRow` has these facts, but opening Matrix starts O(running Containers) viewer-scoped collection. Host Summary needs a bounded non-stream Engine-info snapshot; it must not subscribe to Live Metrics implicitly. |
| Image count | **Derivable** | Count the current Host Images response and label it with that inventory check time. Do not cache it as host state. |
| Docker API version and bundled Compose version | **Minimal extension** | Heartbeat already transports both, but `serverapi.liveHost` discards them. Add optional `Host` fields only. |
| Docker Engine version | **Minimal extension** | `dockeradapter.Probe` knows it, but product heartbeat does not carry it. Add an optional capability/detail field. |
| Storage/logging/cgroup drivers, default runtime, OS type, architecture, kernel, Docker root dir | **Minimal extension** | Docker Engine can provide these via `docker info`; the current `EngineInfo` deliberately exposes only capacity/counts. Extend the bounded info query and Web API without storing it. |
| Compose exceptions and Open Compose | **Derivable** | Filter dashboard projects for this Agent using explicit exception fields. |

No Host OS utilization field is permitted. Engine capacity is not utilization.

## 6. Host → Compose and project Summary

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| Refresh current view | **Derivable** | Re-request dashboard/host data; this is not Discovery rescan. |
| `Rescan projects` | **Supported** | `discovery.rescan` is an existing asynchronous Operation. |
| Project name, working directory, managed/read-only state | **Supported** | Exact fields are present. |
| Config drift, last verified, collision/stale/recovery attention | **Supported** | Exact explicit fields are present; never infer from Compose config-hash. After `main@22f21678`, the current fingerprint safely includes selected Compose/default override inputs plus readable include/extends/`env_file` inputs, and incomplete inputs disable cache reuse. |
| Observed attached Container count | **Supported with freshness qualification** | `container_ids` comes from the latest project observation. Call it current only when `present=true`, `stale=false`, and the observation is current; otherwise label it `Last known` with `last_verified_at` or omit a current count claim. |
| Defined service count | **Minimal extension** | The Agent evaluates defined services, but the Server’s public `Project.services` currently stores runtime label-attached services and loses the defined set. Preserve separate `defined_services` and observed runtime services. |
| Runtime summary: no Container/profile inactive/one-off/orphan | **Minimal extension** | Current Compose `ps` is bounded CLI text and must not be parsed. Add a structured Docker/Compose runtime view based on Docker facts plus the authoritative evaluated service/profile model. |
| Project name/directory | **Supported** | Project fields are exact. |
| Compose files and merge order | **Minimal extension** | After `main@22f21678`, Agent discovery selects Docker Compose’s single default base plus optional default override and keeps base-before-override order. `Project.ComposeFiles` is authoritative but the project transport/storage/Web API still omits it; expose bounded content-free relative metadata. |
| Included applications | **Derivable** for discovered child projects | Invert `included_by` across dashboard projects. Non-project include references remain source references, not invented applications. |
| Include/extends references and source-graph completeness | **Supported** | Content-free `source_references` and `source_graph_complete` exist. `main@22f21678` additionally hashes safe include/extends inputs and marks unreadable/out-of-root/ambiguous input incomplete rather than reusing cache. |
| Active profiles | **Minimal extension** | Not represented in Compose evaluator result or API. Docker Compose must remain authority; do not parse files in the browser/Server. |
| Known source references | **Supported** | Include/extends path, accessibility, and read-only facts exist. |
| Pull/Up/Restart/Start/Stop/Down operations, project or service where valid | **Supported** | Generic operations and fixed Compose argv support project/service targeting; Down remains project-only. |
| Pull success follow-up (“running Containers unchanged”) | **Derivable** | Show only after `compose.pull` succeeds; it is explanatory product copy, not inferred runtime state. |
| Restart does not apply config; save recommends Up, not Restart | **Derivable** | Frozen semantics and client copy. |
| Down confirmation and retained Volume wording | **Derivable** | Current argv does not pass `--volumes`; describe exactly that behavior. |
| Image-backed/build-only/build-required Service facts and mutation policy | **Minimal extension — approved slice A** | Extend the effective Compose model with content-free `image`, `build` presence, and `pull_policy` facts. Pull explicitly targets image-backed Services. Every Up uses `--no-build`; build-only or `pull_policy: build` Services cannot be Pull/Up targets and block Project Up rather than being silently skipped. |

## 7. Compose project → Containers and Host → Containers

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| Container ID/name, image, Docker state/status | **Supported** | Host inventory returns all Containers directly from Docker Engine. |
| Compose project/service per Container | **Minimal extension** | Agent has raw Compose labels, but the Server intentionally removes them from the browser inventory and exposes no per-Container mapping. Add a curated mapping; do not infer from names. |
| Health and ports in tables | **Minimal extension** | Adapter knows summary health but Web inventory omits it; published ports are not modeled. |
| `No container`, `Profile inactive`, `One-off`, `Orphan` | **Minimal extension** | Requires the structured runtime/effective Compose model described above. Name/label heuristics and CLI text parsing are forbidden. |
| Standalone Containers | **Supported** | Host inventory includes every Container. Compose context can be `—` only after the curated mapping fails. |
| State/Compose/search filters | **Derivable** | Client filtering over the complete current host response. |
| Protected Dockpilot Agent remains visible | **Supported** for visibility | Inventory does not hide it. |
| Proactive disabled destructive actions with protection reason | **Minimal extension** | Agent enforces protection at mutation time, but public inventory does not expose protected identity/reason. Add a curated, non-sensitive mutation policy field. |
| Start/stop/restart/remove | **Supported** | Existing asynchronous container operations enforce Agent-side self-protection. |

## 8. Container Inspector

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| ID, image, state, exit code, health summary, labels, mounts | **Minimal extension** | Agent `dockeradapter.Inspect` has a subset, but there is no inspect query/HTTP route and the list API strips labels/mounts. |
| Created/started/finished, OOM killed, restart count | **Minimal extension** | Docker inspect supplies them; current adapter/Web types do not. |
| Restart policy, stop signal/timeout, logging driver, command/entrypoint | **Minimal extension** | Not modeled today. Add a curated inspect response, not raw unbounded inspect JSON. |
| Published host IP/port → target/protocol | **Minimal extension** | Port bindings are absent. Keep them distinct from image exposed ports. |
| Network IPv4/IPv6/MAC/aliases | **Minimal extension** | Network attachment details are absent. |
| Mount type/source/destination/read-only | **Minimal extension** | Adapter already has the facts; expose only through the curated detail route. Preserve actual mount types. |
| One-off/orphan/Compose context | **Minimal extension** | Depends on structured runtime mapping, not name inference. |
| Environment/raw inspect reveal | **Product decision** | Not required by the final contract. If adopted, it needs an explicit sensitive reveal and bounded curated response; never expose raw inspect by default. |

## 9. Compose project → Files

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| Read whitelisted managed file by relative path | **Supported** | Safe-file API enforces project boundary, no symlink/traversal, UTF-8, and size limit. |
| Mask/reveal potentially sensitive file content | **Supported** | `reveal` is explicit; `.env` values are masked by default. |
| Expected hash/concurrent edit protection | **Supported** | PUT requires `expected_sha256`; conflicts are refused. |
| Atomic single-file save and automatic snapshot | **Supported** | Agent owns validation/snapshot/rename/fsync semantics. UI must not claim multi-file transaction. |
| Read-only view/copy and disabled Save reason | **Supported** | Project/host capabilities and source reference metadata provide state/reason; actual safe-file read remains authoritative. |
| Save is not apply; offer Up after success | **Derivable** | UI copy/action only. Operation completion—not 202 acceptance—must trigger success follow-up. |
| Source navigator: ordered Compose files | **Minimal extension** | Default selection/order is now Docker-compatible in the Agent, but ordered `ComposeFiles` is still not exposed, as noted above. |
| Included applications and extends references | **Supported/Derivable** | Source references exist; discovered include children can be joined by `included_by`. |
| Interpolation `.env` entries | **Supported** | `/environment` returns masked/revealed Agent-fetched entries. It does not expose a source taxonomy beyond the managed environment. |
| Service `env_file` metadata | **Minimal extension** | Agent Catalog computes content-free path/readability facts and `main@22f21678` includes safely readable inputs in the fingerprint, but project query/Web API still drops the metadata. |
| Compose secrets/configs source metadata | **Minimal extension** | Not modeled. Add only content-free source metadata from Docker Compose’s authoritative evaluated model. |
| Resolved/interpolated configuration reveal | **Minimal extension** | Current Compose config endpoint always uses `--no-interpolate` and returns bounded transient text. Add an explicit reveal option if resolved output is adopted; never store it. |
| Merge/include/extends categories remain distinct | **Derivable** after missing metadata is exposed | Category is presentation, but source kind and merge order must stay separate. |

## 10. Compose project → Logs

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| Project/service-filtered live Docker Compose logs | **Supported** | SSE endpoint selects a discovered project and optional validated service names. |
| Container-filtered project logs | **Minimal extension** | Current project log request has service filters only. A host Container route can stream one Container, but the project viewer has no safe multi-Container selector. |
| Tail, timestamps, follow | **Supported** | Bounded validated options exist. |
| Since/Until over Docker Engine-retained logs | **Minimal extension** | No request/transport/argv fields exist. Add fixed validated time options; do not describe them as Dockpilot retention. |
| Follow/range contradiction prevention | **Derivable** | Client control-state validation once Since/Until exists. |
| Find in loaded logs, Clear view | **Derivable** | Bounded browser-buffer interactions only; do not imply server search/deletion. |
| Pause auto-follow when scrolled; Jump to latest | **Derivable** | Client scroll state only. Current implementation always jumps and must change. |
| Dropped bytes/lines | **Supported** | Exact counters are included in log events. |
| Reconnect without continuity promise | **Supported/Derivable** | Streams are non-resumable; UI must say a new request is a new live view. |
| Logging-driver unavailable reason | **Partially supported; Minimal extension for preflight detail** | Stream opening returns a capability/error reason, but terminal Agent errors are reduced to generic “log stream ended.” Preserve a bounded safe reason if the UI must diagnose the driver after stream start. |
| Multi-Container merge order disclaimer | **Derivable** | Static semantics; no causal/audit order may be claimed. |

## 11. Compose project → Backups

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| List backup metadata | **Supported** | ID, created time, trigger, file count, bytes, and manifest digest are available. Backup bytes remain Agent-local. |
| Manual configuration backup | **Supported** | Existing asynchronous backup create accepts approved relative paths. |
| Restore with current-state snapshot and no automatic Up | **Supported** | Agent restore contract and project lock implement this. |
| Restore confirmation copy, included/not-included explanation | **Derivable** | Render frozen scope and the selected managed paths; never imply Volume/application-data backup. |
| Exact archived file list per existing backup | **Minimal extension** | Current metadata returns only file count, not manifest entries. Add bounded manifest metadata only if the confirmation must enumerate archived paths. |
| `restore_recovery_required` severe blocking state | **Supported** | Exact project field exists and all mutations are refused. |
| Cancel is not rollback | **Derivable** | Required copy; actual cancel outcome remains authoritative. |
| Restore diff preview | **Product decision** | Architecture classifies it OPTIONAL; no current endpoint. Not required for the initial final UI. |
| Volume/bind/database backup | **Product decision — out of scope** | Explicitly outside v1; must not appear as a gap to “fill.” |

## 12. Compose project Activity and Host Audit

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| Stored host Audit and project-filtered Activity | **Supported** | Canonical Server history remains queryable while Agent is offline. |
| Time, kind, action, resource, actor | **Supported** | Curated fields exist; raw metadata/file contents are not exposed. |
| Related Operation and cursor evidence | **Supported** | `operation_id` and `(incarnation, seq)` are returned. |
| Project Activity uses host-wide coverage | **Supported** | Project activity response carries the owning Agent’s coverage, not fabricated project coverage. |
| Confirmed gap vs continuity uncertainty | **Supported** | Coverage gap types/sources and unknown incarnations/continuity records are separate. Current UI wording must stop collapsing them into one warning paragraph. |
| Cursor pagination | **Supported** | Bounded `limit` and cursor exist. |
| From/Until filter | **Minimal extension** | API rejects those parameters today. Add indexed Server-side time filtering without changing Agent WAL query contracts. |
| Resource/Kind/Actor filters | **Minimal extension** | API currently accepts only limit/cursor. Browser filtering of one loaded page must not masquerade as archive filtering. |
| Event Inspector with timezone/technical evidence | **Derivable** | Existing event fields are sufficient. |
| Actor as technical provenance only | **Supported** | Values are `ui:<ip>`, webhook/system where real, or unknown; no user identity exists. |
| OOM/restart/pause/unpause observed events | **Product decision** | Requires review of the frozen observed-event whitelist. A UI design cannot expand Audit collection. |

## 13. Host → Images

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| One row per Image ID, tags/digests, created, virtual size | **Supported** | Current inventory returns all these summary fields. Label size as virtual size, not unique disk use. |
| Untagged state | **Derivable** | Empty tags are authoritative for “untagged.” |
| Unused vs referenced by stopped/running Containers | **Minimal extension** | Current image summary count cannot prove running/stopped references and Host Containers expose image names, not image IDs. Add explicit reference mapping. |
| Image Inspector: config/platform/metadata/size detail | **Minimal extension** | No image inspect query exists. Use curated Docker inspect/history facts, not raw JSON. |
| Shared/unique disk size | **Product decision** | Optional Docker disk-usage query can be expensive. Only add with explicit query UX and unavailable/calculating states. |

## 14. Host → Networks

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| Name, driver, scope, internal/attachable/ingress | **Supported** | Current inventory summary includes these fields. |
| Multiple IPAM configs/subnets | **Minimal extension** | Not exposed; do not assume one subnet. |
| Connected Containers and their Docker addresses/MAC | **Minimal extension** | No network inspect route or attachment model exists. |
| Options, labels, Compose ownership | **Minimal extension** | Add curated inspect fields; Compose context only where actual labels prove it. |

## 15. Host → Volumes

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| Name, driver, scope, created time | **Supported** | Current inventory summary includes these fields. |
| Compose project ownership | **Minimal extension** | Volume labels are currently not exposed. Do not infer from names. |
| Referenced Containers, running/stopped counts, destinations | **Minimal extension** | Requires joining volume inspect/list facts to current Containers/mounts. |
| Driver options, labels, mountpoint | **Minimal extension** | No Volume inspect route exists. Mountpoint must remain driver-qualified. |
| Size with Calculating/Unavailable semantics | **Product decision** | Docker disk-usage support is OPTIONAL and potentially expensive; unknown must never become zero. |

## 16. Host → Live Metrics

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| Viewer-scoped collection lifecycle | **Supported** | `/api/v1/live/matrix` shares one Agent stream per watched host and stops after the last viewer. |
| Host → project → service → Container hierarchy | **Supported** | Matrix frames provide the full joined hierarchy, including pending/unmapped rows. |
| CPU, memory, cumulative network/block I/O, health/restarts/uptime | **Supported** | Exact live sample and aggregate fields exist. CPU may exceed 100%; unbounded memory is explicit. On Linux, `main@22f21678` now matches Docker CLI memory usage by subtracting cgroup v1 `total_inactive_file` or cgroup v2 `inactive_file`. |
| Engine capacity and managed-filesystem capacity | **Supported** | Host row is Docker workload against Engine capacity; it is not Host OS utilization. |
| Top Containers from the same frame | **Derivable** | Flatten current frame client-side; throttle/reorder less often than sample updates. No second collector. |
| Network and block `/s` rates | **Derivable** | Compute delta over actual `observed_at` between frames; first/reset/decreasing counters are unavailable, not zero. |
| Stable hierarchy order | **Derivable** | Key rows by stable IDs and keep order while updating values. |
| Membership/workload/context stale reasons | **Supported** | Three independent flags/reasons exist and must not be collapsed. |
| Agent/server dropped-frame evidence | **Supported** | Separate counters exist and must never be added together. |
| Shared network namespace aggregate limitation | **Derivable as an explicit limitation** | Current aggregation sums Container counters and has no namespace de-dup metadata. State the limitation; do not imply de-duplicated host totals. A de-duplicated aggregate would require a new design. |
| Processes/threads metric | **Minimal extension** if displayed | No PIDs/process count exists in Stats/Matrix. It is not required by the final table and should be omitted initially. |
| Charts/history/threshold colors | **Product boundary — do not build** | Remove the current browser CPU chart. The final view is current hierarchy/top comparison only. |

## 17. Error, empty, partial, accessibility, and responsive states

| Requested field or interaction | Class | Repository finding |
|---|---|---|
| No objects vs no search matches | **Derivable** | Distinct client states from successful empty response vs local filter result. |
| Engine/capability unavailable | **Supported** | Stable HTTP code plus capability reasons. |
| Query failed vs never scanned vs truncated | **Supported** | API errors and `project_scan` facts distinguish them. |
| Stale last-known data | **Supported** for projects/matrix; **Derivable** for a browser-held prior inventory response | Project/matrix freshness is explicit. A browser may keep its own prior successful inventory response and timestamp, then label it `Last known` if refresh fails. The Server must not persist Docker inventory; without a prior browser snapshot, show unavailable with no rows. |
| Partial cross-host result | **Product decision** | Only applies if a live cross-host query feature is adopted. |
| Uniform error explanation and technical disclosure | **Derivable** | Use machine code + bounded message/reason; never branch on human reason text. |
| Semantic tables, links, buttons, forms, dialogs | **Derivable** | Frontend implementation. |
| Visible focus, non-color status, reduced motion | **Derivable** | CSS/markup implementation. |
| Modal focus trap/return and non-modal Inspector no trap | **Derivable** | Client interaction implementation. |
| Metrics updates do not spam live regions | **Derivable** | Do not place every numeric sample in an assertive/polite live region. |
| 1440/1280/1024/768/375 layouts | **Derivable** | Responsive frontend work and manual acceptance testing. |

## 18. Explicit product boundaries and open decisions

The following are not implementation gaps:

- Host OS CPU/RAM/network monitoring;
- retained metrics, charts, thresholds, or alerting;
- arbitrary Docker mutations merely because Docker supports them;
- authentication, users, roles, avatars, or generic Settings in v1;
- New Project;
- central log storage/search or gap-free log resume;
- Volume, bind-mount, database, or application-data backup;
- raw shell/exec terminal or image-build platform.

### Compose build policy — approved v1 decision

Dockpilot v1 does not build Images and exposes no Build action or build tooling.
Every Up invocation adds `--no-build`. Pull is issued with an explicit list of
effective Services that declare `image`; it must not use `--ignore-buildable`,
because mixed `image` + `build` Services remain image-backed. Pull failure never
falls back to build.

Build-only Services and Services with `pull_policy: build` are build-required:
their Pull and Service Up actions are unavailable. If any build-required Service
is in the effective Project Up target set, the whole Project Up is unavailable;
Dockpilot never silently skips it. The effective Compose model is authoritative
for this classification.

### P1 candidates requiring separate adoption

- live cross-host Container search with explicit partial coverage;
- host-scoped multi-Container logs launched from selected Containers;
- observed Audit expansion for OOM/restart/pause/unpause;
- optional Docker disk-usage queries for Image/Volume size;
- restore diff preview.

## Recommended implementation boundary after review

1. **Frontend foundation:** replace the old page with the final host-first
   shell, route model, Home, host/project navigation, deterministic states,
   semantic tables, accessibility, and responsive behavior.
2. **Use existing contracts first:** files, backups, Compose operations,
   project/service logs, Audit/Activity cursor paging, basic live inventory,
   and host-wide Live Metrics matrix.
3. **Add bounded Server-only/API views:** Operation listing/context, heartbeat
   version exposure, and indexed Audit filters.
4. **Add bounded Agent read extensions:** Engine details, curated Docker object
   inspectors, ordered Compose/source metadata, structured Compose runtime
   facts, and safe log time options. Do not store Docker runtime/config content
   on the Server.
5. **Apply only the separately approved Compose build decision and defer all
   other product-decision/P1 items.** The UI must show honest
   unavailable/not-adopted states, not placeholders that imply support.

## Minimal-extension approval slices

The following slices prevent “Minimal extension” from becoming one implicit
approval batch. Each extension remains additive and bounded; none changes the
authority or persistence model.

### A. Required to complete the final v1 screen contract

- current in-memory Agent session source IP and observation time;
- bounded global Operation listing with context, timestamps, phase, output-tail,
  and cancelability facts;
- bounded non-stream Host Summary Engine snapshot plus Docker Engine/API and
  bundled Docker Compose versions;
- distinct defined-service and observed-runtime facts, including
  `No container`, `Profile inactive`, `One-off`, and `Orphan`;
- effective image-backed/build-only/build-required Service classification and
  enforcement of explicit Pull targets plus always-`--no-build` Up;
- authoritative ordered Compose file metadata, active profiles, `env_file`,
  Compose secret/config source metadata, and explicit resolved-config reveal;
- curated Container table context: Compose project/service, health, published
  ports, and proactive self-protection reason;
- project log Container selection and validated Since/Until controls;
- bounded backup manifest path metadata for exact restore scope;
- indexed Audit From/Until/Resource/Kind/Actor filters.

These are the only extension items proposed for the implementation-completion
gate. Product-policy items are excluded even when they also need fields.

### B. Progressive detail and Inspector acceptance hardening

- advanced Docker Engine detail: storage/logging/cgroup drivers, runtime, OS,
  architecture, kernel, and Docker root directory;
- curated Container Inspector diagnostics beyond the required table facts:
  exit code, labels, lifecycle timestamps, OOM, restart count/policy,
  stop signal/timeout, logging driver, command/entrypoint, mounts, and detailed
  network attachments;
- Image usage/reference mapping and curated Image Inspector;
- Network multi-IPAM, attachment, options, labels, and Compose context;
- Volume references/destinations, options, labels, mountpoint, and Compose
  context;
- bounded post-open log terminal reason where a safe diagnostic is available.

This slice is required before claiming every Inspector/detail acceptance item,
but it can follow the route/table foundation and must be reviewed separately
from slice A.

### C. Optional, P1, or product-decision work — deferred

- cross-host live Container search and host-scoped multi-Container logs;
- observed Audit whitelist expansion;
- Image/Volume disk-usage queries;
- restore diff preview;
- raw Container environment/inspect reveal;
- Processes/threads metrics.

## Review decisions requested

Before implementation, confirm:

1. whether this revalidated report and five-step implementation boundary are
   approved as the implementation gate;
2. whether slice A extensions are approved for the final v1 screen contract;
3. whether slice B should be included in the same implementation campaign or
   reviewed as a separate follow-up;
4. that slice C, Compose build policy, and all P1 candidates remain deferred
   unless separately approved.

## Review judgment — 2026-08-23

**Verdict:** Conditional approval of the analysis method and most findings; this
revision is **not yet approved as the implementation gate**. Implementation
should remain paused until the items below are corrected and the report is
revalidated.

### Why approval is conditional

The report was produced against `docs/final-ui-design` revision `a4205c0`, but
`main` has since advanced to `22f21678` (`fix: align Docker and Compose safety
behavior (#6)`). That commit changes areas directly covered by this report,
including Compose file ordering/source handling, `env_file` fingerprinting and
source-graph completeness, and Docker-compatible Linux memory semantics. The
branch must first incorporate current `main`, then this gap analysis must be
rerun against the merged state.

### Required corrections before implementation

1. **Sidebar Agent availability must not be derived from `Host.state == ACTIVE`.**
   `Host.state` is the session lifecycle state. A session may remain `ACTIVE`
   while heartbeat fails and every capability is unavailable. Sidebar
   Connected/Available semantics must use the explicit connection capability
   (`Capabilities.Connection.Enabled`) or an equivalent authoritative current
   connection fact.
2. **Host Summary counts/capacity must not open Live Metrics implicitly.**
   `Container total/running` and Engine CPU/memory capacity are available in the
   Matrix host row, but subscribing to Matrix starts viewer-scoped collection.
   A normal Host Summary must therefore use a bounded non-stream host/Engine
   snapshot. Reclassify this requirement from `Supported through Live Metrics
   matrix` to **Minimal extension** unless an existing non-stream authoritative
   query is confirmed after rebasing.
3. **`container_ids` must not be described as unqualified current runtime
   state.** The project snapshot can be stale. Treat it as an observed attached
   Container count and only label it current when the project observation is
   itself current. Otherwise surface the freshness qualifier or avoid a current
   count claim.
4. **Re-run every classification affected by `main@22f21678`.** In particular,
   re-check ordered Compose files, source/env metadata, source-graph completeness,
   fingerprints, and Live Metrics memory semantics before keeping their current
   Supported/Derivable/Minimal-extension labels.

### Approval boundary

After the rebase and corrections above, the five-step implementation boundary
is acceptable in principle. However, `Minimal extension` items should not be
approved as one undifferentiated batch. The refreshed report should separate:

- **required for the final v1 UI contract**;
- **required only for progressive detail/Inspector quality**; and
- **optional/P1 or product-decision work**.

Compose build policy and every existing P1 candidate remain deferred until
separately approved. The authority/storage rules remain unchanged: Docker stays
the authority for current runtime state, the filesystem for Compose file
content, the Agent for execution/locks, and the Server canonical Audit archive
for synchronized Audit history.

## Revalidation response — 2026-08-23

The conditional-review requirements above have been applied to the report, but
this response does not self-approve the implementation gate.

- `main@22f21678` was integrated into `docs/final-ui-design` by merge commit
  `58ad1dc50af1fbfda2550c2fd88c7aba7a6a890b`; the previously published review
  commit remains in history unchanged.
- Sidebar availability now names
  `capabilities.connection.enabled` as authority and treats `Host.state` only
  as session lifecycle evidence.
- Host Summary counts/capacity are reclassified to a bounded non-stream
  **Minimal extension**; Matrix must not be opened implicitly.
- `container_ids` is now described as an observed project attachment set with
  `present`/`stale`/`last_verified_at` freshness qualification.
- Docker-compatible default Compose base/override selection and order were
  confirmed in Agent discovery. Ordered `ComposeFiles` remains a UI/API gap
  because the project response still omits it.
- Include/extends and service `env_file` inputs are safely digested into the
  current fingerprint where readable; unsafe/incomplete inputs prevent cache
  reuse. Source-reference metadata and `source_graph_complete` remain
  supported, while `env_file` metadata exposure remains a Minimal extension.
- Linux Live Metrics memory usage now follows Docker CLI-compatible cgroup v1
  and v2 inactive-file subtraction, so the Supported classification remains.
- Minimal extensions are now divided into final-contract, progressive-detail,
  and optional/product-decision slices.

Implementation remains paused pending an external review of this refreshed
gate and explicit scope selection for slices A and B.

## External review decision — 2026-08-23

**Verdict:** Approved as the implementation gate, with slices A and B approved
and slice C deferred.

The revalidation response resolves the prior conditional-review findings. The
design branch now incorporates `main@22f21678`; sidebar availability uses the
explicit connection capability rather than `Host.state`; Host Summary Engine
facts are correctly separated from viewer-scoped Live Metrics; project
Container attachment counts are freshness-qualified; and the Compose
source/fingerprint and Docker-compatible memory semantics affected by the
updated main revision have been revalidated.

Slice A is approved as required work for the final v1 screen contract.

Slice B is also approved for the same implementation campaign, but should
follow slice A as a separately validated implementation phase. The advanced
Engine and Docker object Inspector reads are part of the accepted
progressive-disclosure design and are required before the final UI can claim
full Inspector/detail acceptance. They remain bounded, read-only extensions and
do not change Dockpilot's authority or persistence model.

Slice C remains deferred. No cross-host live Container search, host-scoped
multi-Container logs, Audit whitelist expansion, Image/Volume disk-usage
queries, restore diff preview, raw Container environment/inspect reveal, or
Processes/threads metrics should be added without a separate product decision.

The Compose build policy remains unresolved and is not implicitly approved by
this gate. Implementation of the surrounding UI and slices A/B may proceed,
but the final availability and semantics of build-capable Compose Pull/Up
actions must not be invented by the implementation and require a separate
explicit product decision.

Authority and storage boundaries remain unchanged: Docker Engine is
authoritative for current runtime state, the host filesystem for Compose
configuration content, the Agent for execution and project locks, and the
Server canonical Audit archive for synchronized Audit history.

Implementation may proceed with slice A followed by slice B. Final v1
acceptance remains contingent on the separately resolved Compose build policy
and successful acceptance validation.

## Compose build policy decision — 2026-08-23

**Verdict:** Approved for v1. The policy dependency named by the external review
is resolved.

Dockpilot executes Compose only when the effective selected Service model can
be satisfied from declared Images without invoking a build. It never builds,
never falls back to build, and never silently skips a build-required Service.

- Every Up invocation uses `--no-build`.
- An `image` Service and an `image` + `build` Service support Pull and Up through
  the declared Image only.
- A `build`-only Service does not support Pull or Service Up and blocks Project
  Up when it belongs to that effective target set.
- `image` + `build` + `pull_policy: build` is explicitly build-required and has
  the same blocked behavior.
- Pull passes the effective image-backed Service names explicitly; it does not
  use `--ignore-buildable` and does not build on failure.
- Dockpilot v1 exposes no separate Build action, Dockerfile/build-context
  editing, build arguments, or BuildKit control.

This decision moves the read-only classification and bounded mutation-policy
facts into approved slice A. Slice C remains deferred unchanged otherwise.

## Implementation completion — 2026-08-23

**Verdict:** Approved slices A and B and the v1 Compose build policy are
implemented. Slice C remains absent from the release path.

Slice A was completed through additive bounded contracts for current session
provenance, global Operations, one-shot Engine facts, effective Compose and
runtime metadata, explicit no-build mutation admission, curated Container
inventory, log range/Container selection, exact backup manifest paths, and
indexed Audit filters. Docker runtime/config contents remain transient and
Agent-authoritative; the Server stores only the already approved operational
and Audit records plus content-free project facts.

After the slice A contract tests passed, slice B was completed through curated
Container, Image, Network, and Volume Inspector queries, advanced one-shot
Engine details, running/stopped reference context, multi-IPAM and attachment
facts, and bounded safe log terminal diagnostics. These reads fail closed for
unsupported N-1 Agents and do not persist Docker inspect payloads.

The browser implementation now follows the final host-first route hierarchy,
light visual contract, exact host/project tabs, durable Operation visibility,
current-vs-stored labeling, no-chart viewer-scoped Live Metrics, explicit
first-column object links, and route-aware responsive Inspector behavior.
Playwright is included as a development-only acceptance dependency so the real
embedded assets can be inspected at the five required viewports with captured
screenshots and an HTML report.

Validation evidence:

- `go test ./...`: pass;
- `go test -count=1 -race ./...`: pass;
- `go vet ./...`, `node --check internal/webui/assets/app.js`, and
  `git diff --check`: pass;
- every `scripts/verify-*.sh` static release/harness gate: pass, including the
  release-scope proof that Slice C/FUTURE and Image-build behavior are absent;
- installed Docker Compose v5.5.0 confirms `up --no-build`, Pull's explicit
  Service arguments, `--ignore-buildable` semantics, and the evaluated fixture
  model used by the implementation;
- Playwright Chromium acceptance at 1440/1280/1024/768/375: 27 passed, with 3
  intentional skips for the collapsible-navigation test outside its applicable
  narrow viewports.

The human checklist in `web-ui-acceptance.md` remains the source of record for
real-Agent representative-state and destructive-action review; automated
fixture evidence does not self-approve those manual states.
