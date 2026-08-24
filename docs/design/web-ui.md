# Dockpilot Web UI — Final Screen Contract

**Status:** Design-ready; implementation gaps may remain.  
**Normative scope:** UI structure, terminology, state presentation, interaction rules.  
**Not authorized by this document:** silent changes to frozen architecture/API semantics.

## 1. Information architecture

```text
Dockpilot
├─ Home
├─ Global Search
├─ Global Operation Center
└─ Docker hosts
   └─ Docker host
      ├─ Summary
      ├─ Compose
      │  └─ Compose project
      │     ├─ Summary
      │     ├─ Containers
      │     ├─ Files
      │     ├─ Logs
      │     ├─ Backups
      │     └─ Activity
      ├─ Containers
      ├─ Images
      ├─ Networks
      ├─ Volumes
      ├─ Container stats
      └─ Audit
```

Object home remains under its Docker host. Global Search routes directly to that real context.

### Shared table interaction

Every repeated-object list table allows its columns to be resized from the
header boundary. Pointer users drag the boundary; keyboard users focus the
separator and use Left/Right Arrow. The chosen widths are browser-local
preferences, shared by tables with the same column contract, and are never
Server or Agent state. Moving a boundary redistributes width between its two
adjacent columns while the table remains inside its existing maximum width; it
must not introduce horizontal table or page scrolling.

## 2. Home

Purpose:

> Compare Docker hosts, discover deterministic exceptions, and route quickly to the host or Compose project that needs attention.

Order:

1. Search
2. Compact clickable state filters
3. Needs attention
4. Docker hosts table

Suggested compact filters:

```text
All hosts 8
Docker Engine unavailable 1
Docker Compose unavailable 1
Discovery incomplete 2
```

These are filters, not KPI cards.

### Host table

```text
DOCKER HOST | AGENT | DOCKER ENGINE | DOCKER COMPOSE | DISCOVERY
```

Host rows may optionally disclose their Compose projects. Keep project disclosure collapsed by default when many hosts exist.

### Needs attention

Only deterministic Dockpilot-known exceptions belong here, for example:

- Docker Engine unavailable
- Docker Compose unavailable
- discovery failure/truncation/incomplete
- restore recovery required
- project name collision
- missing/stale project
- configuration changed
- Audit gap / continuity issue

Do not infer attention from CPU %, exited Container, unused Volume, or arbitrary message text.

## 3. Global Search

Immediate indexed search:

- Docker host
- Compose project

Results include name, host context, secondary identity/path, and known exception when useful.

Container cross-host search may require a live query across hosts. If implemented, it must report partial coverage explicitly:

```text
5 of 6 Docker hosts searched · 1 unavailable
```

Never imply a complete global runtime index if one does not exist.

## 4. Global Operation Center

Visible when there are active/recent relevant operations.

Compact trigger:

```text
Operations 2
```

Drawer item:

```text
compose.up
backend · production-01

EXECUTING · 2m 14s

PREPARING     completed
EXECUTING     active
COMMITTING    pending
FINALIZING    pending

Output tail
Creating backend-api-1...
Starting backend-api-1...

Earlier output was truncated.
```

Rules:

- Persist across navigation.
- Never invent percent progress without an authoritative source.
- Preserve `unknown` rather than converting it to failed.
- Cancel is not rollback.
- Show Cancel only when the operation contract allows it.
- Bounded output is called `Output tail`, never `Full output`.
- Long-lived history remains in Audit.
- Mutation Toasts distinguish request acceptance from terminal outcome:
  blue/info `Started`, green `Completed`, amber `Completed with attention`,
  `Canceled`, or `Interrupted`, and red `Failed` or `Rejected`.
- A `202 Accepted` response never produces a success Toast. The browser polls
  the Agent-authoritative operation endpoint and updates the same Toast only
  after observing a terminal status.
- Every operation Toast links directly to its operation details. Errors use
  an assertive alert; all other updates use a polite status and never rely on
  color alone.
- When a details panel is open above the mobile breakpoint, Toasts shift left of
  it instead of covering its content.

## 5. Host Summary

Role:

> What is this Docker Engine, what can Dockpilot currently do on it, and what requires attention?

Sections:

Use three separate outer panels in this order: `Host`, `Docker Engine`, and
`Compose projects`. Do not mix Agent/Dockpilot management facts with Docker
Engine facts or Compose project state.

### Host

Show current session/discovery metadata first, followed by a visually separated
capability-state section:

```text
Session source IP
Session observed
Compose discovery

Capabilities
Agent connection
Docker Engine
Docker Compose
Compose discovery
File read / File write
Container stats
Operation recovery
```

### Docker Engine

```text
Engine version
Containers        total / running / stopped
Images
CPU used / total
Memory used / total
Usage observed
Storage driver
```

The numerator is a current aggregate of running Docker Containers; it excludes
Host processes outside Docker. CPU usage is expressed as logical CPUs used,
with 100% of Docker stats CPU equal to one logical CPU. The denominator is the
logical CPU count or memory capacity reported by Docker Engine. Memory bytes
are rendered with IEC units such as GiB. `Usage observed` exposes the snapshot
time so a live value is never presented without freshness.

Opening Summary requests Container stats only long enough to obtain a current
workload snapshot, then closes the viewer. The snapshot remains browser-only
and is neither retained nor reused as Host OS utilization.

#### Engine technical details

Show these facts as a separately spaced subsection in the same Docker Engine
panel, without adding a second divider next to the overview's row boundaries.
Use two columns on wide screens and one column on narrow screens. Do not repeat
overview fields such as the Engine version or storage driver:

```text
Engine API version
Docker Compose version
Logging driver
Cgroup driver/version
Default runtime
OS type
Architecture
Kernel
Docker root dir
```

### Compose projects

Show only exceptional projects, then `Open Compose`.

## 6. Host → Compose

The page header keeps the selected Docker host as the level-one title, matching
every other Host tab. `Compose` remains the active Host tab and the list panel
is titled `Compose projects`; navigating to this tab must not replace the Host
identity with a new level-one `Compose` page.

List subtitle:

> Compose projects discovered on this Docker host

Actions:

- Refresh current view
- `Rescan Compose projects` as a distinct discovery operation

Suggested table:

```text
PROJECT | SERVICES | CONTAINERS | LAST OBSERVED | COMPOSE CONFIG | NEEDS ATTENTION
```

`Containers` contains only a current attachment count. If the Project is stale
or missing, show `Unavailable` instead of presenting the cached count as
current. `Last observed` separately shows the Agent's `last_observed_at`
timestamp, or `Never` when no observation exists; it is not Container age or
creation time.

Project cell contains project name + working directory.

Service/Container counts must not look authoritative if the Compose file graph is incomplete.

No `New Project`.

## 7. Compose project → Summary

Header:

```text
backend
/srv/compose/backend
[ Pull ] [ Up ] [ Down ] | [ Start ] [ Stop ] [ Restart ]
```

Tabs:

```text
Summary | Services | Containers | Files | Logs | Backups | Activity
```

Sections:

Use two separate outer panels in this order: `Project` and `Containers`. Compose
metadata and observed Compose Containers must not be mixed into one split card.
The effective Service model and its per-Service mutations belong to the
separate `Services` tab.

Do not add a project `Images`, `Networks`, or `Volumes` tab. Those are
Engine-wide Docker objects without Compose ownership and remain in Host scope.
Compose project depth exists for the effective model, source configuration,
project-scoped logs/history, backups, and Compose operations.

### Project

Show effective Compose metadata first:

```text
Project directory
Compose files and merge order
Included applications
Active profiles
Compose file graph complete/incomplete
```

Then show a separately spaced `Dockpilot management` subsection:

```text
Managed/unmanaged
Dockpilot discovery record and freshness
Compose operation availability
File access
Compose config state
Last verified
```

### Containers

- Services in the Compose model
- observed Containers by Docker state
- Services with no Container
- Services excluded by inactive profiles
- one-off/orphan count when relevant

The panel description includes the Compose Container observation time or says
that current Compose Container state is unavailable.

### Services needing attention

Finish Summary with a compact exception-only Service list. It includes active
Services without a current Container, unhealthy or abnormal Container states,
and Services blocked by the v1 no-build policy. It does not repeat the full
effective model and does not classify an intentionally inactive profile as a
failure. Each entry links to the Services tab for the complete model and
Service-level operations.

Severe exceptions appear above all sections.

## 8. Compose project → Services

This is a project-level collection route, not an individual Service detail
depth. It shows the complete effective Service model in a table with:

```text
Service | Status | Containers | Health | Image | Build | Pull policy | Profiles | Depends on | Ports | [unlabeled utility]
```

Join two explicitly different sources: the effective model supplies Image,
build, Pull policy, Profiles, and Depends on; observed Compose Containers
supply Docker state, Container count, Health, and published ports. Each
column owns one kind of information instead of combining several facts in one
cell. The unlabeled final utility column contains a quiet `…` trigger. It opens
a state- and policy-aware menu directly below the row for
`Pull / Up | Start / Stop / Restart`. At narrow widths, the row becomes a
labeled vertical record so all data and the action control remain visible
without horizontal scrolling. Every value remains on one line; long values
use ellipsis and expose the complete value as a tooltip instead of increasing
row height. `Down` remains project-wide.
Build-only and other build-required Services show explicit unavailable reasons.
The table does not duplicate project-wide action buttons; those remain in the
project header.

## 9. Compose operations semantics

### Pull

Downloads service images. Does not update running Containers.

Success follow-up:

```text
Images pulled successfully.
Running containers were not changed.
[ Run Up to apply the pulled images ]
```

### Up

Creates/recreates/starts Containers using the effective Compose configuration.

### Restart

Restarts existing Containers. Does not apply Compose configuration/environment changes.

### Start / Stop

Start existing stopped Containers; Stop does not remove them.

### Down

Confirmation must describe the actual invocation semantics. With default Compose down behavior:

```text
This will remove:
• Service containers
• Compose-created networks

Named volumes will be retained.
External networks and volumes will not be removed.
```

### Compose build policy — v1

Dockpilot v1 never builds Images. It has no Build action and does not expose
Dockerfile/build-context editing, build arguments, or BuildKit controls.
`build` is read-only Compose metadata, not a mutation feature.

`Up` always invokes Compose with `--no-build`:

```text
docker compose ... up --detach --no-build [service...]
```

Classify every Service from Docker Compose's effective model:

| Effective Service model                  | Pull                                  | Service Up                  | Project Up                  |
| ---------------------------------------- | ------------------------------------- | --------------------------- | --------------------------- |
| `image`                                  | Available                             | Available with `--no-build` | Included                    |
| `image` + `build`                        | Available for the declared Image only | Available with `--no-build` | Included                    |
| `build` only                             | Unavailable                           | Unavailable                 | Blocks the whole Project Up |
| `image` + `build` + `pull_policy: build` | Unavailable                           | Unavailable                 | Blocks the whole Project Up |

`pull_policy: build` is an explicit build requirement and must not be silently
overridden. More generally, Dockpilot executes Compose only when the selected
effective Service set can be satisfied from declared Images without invoking a
build.

For Pull, resolve the Services whose effective model declares `image` and pass
their names explicitly:

```text
docker compose ... pull api db worker
```

Do not use `pull --ignore-buildable`: it would also exclude a Service that has
both `image` and `build`. Pull never falls back to build. A failed Image pull is
reported as `Pull failed`.

Project Up is unavailable when any Service in its effective target set is
build-only or otherwise build-required. Never silently skip that Service,
because doing so would change the application and may break dependencies.
Service-level Up remains available only for a non-build-required Service with
a declared `image`.

Example unavailable reason:

```text
This Compose project contains 1 build-only Service: worker.
Dockpilot v1 does not build Images. Provide an Image for this Service before
running the whole project with Dockpilot.
```

## 10. Compose project → Containers

Primary table:

```text
SERVICE | CONTAINER | STATE | HEALTH | IMAGE | PORTS | [unlabeled utility]
```

Represent these explicitly:

- ordinary Container state (`Running`, `Exited`, `Paused`, `Restarting`, `Created`, `Removing`, `Dead`)
- `No container`
- `Excluded by profile`
- `One-off`
- `Orphan`

`One-off` and `Orphan` should normally be secondary labels under the Container identity rather than extra table columns.

Service names are filters/shortcuts, not separate pages.

The unlabeled final utility cell uses the same quiet `…` row menu as Services.
It opens state-aware operations for only the selected Container: Start, Stop,
Restart, and Remove. Restart and Remove require confirmation after menu
selection. Remove is never forced and does not request Volume removal; running
Containers must be stopped first. Protected Agent Containers keep the menu
visible but disable Stop, Restart, and Remove with the protection reason.

## 11. Compose project → Files

Split layout:

```text
Source navigator | Editor / resolved view
```

Navigator groups:

```text
COMPOSE FILES — MERGE ORDER
INCLUDED APPLICATIONS
EXTENDS REFERENCES
INTERPOLATION ENVIRONMENT
SERVICE ENVIRONMENT FILES
COMPOSE SECRETS / CONFIGS
```

Category headings use a distinct section treatment; source items are indented
and carry a separate item marker. A file name must not look like another
category heading, especially in the narrow single-column layout.

Rules:

- Only whitelisted project-related files; not a general filesystem browser.
- `.env` is not called a Docker Secret.
- Potentially sensitive values are masked until explicit reveal.
- Read-only files remain viewable/copyable; Save is disabled with reason.
- Write uses expected hash/concurrency protection.
- External modification must not be overwritten silently.
- Saving does not apply Compose changes.
- On save success, offer `Run Up to apply configuration changes` when appropriate.
- Before mutation, Dockpilot may create its existing automatic managed-file snapshot according to the backup contract.

### `docker compose config`

Explicit reveal is required because resolved/interpolated output can contain sensitive values.

## 12. Compose project → Logs

Header:

```text
Docker Engine logs · Not retained by Dockpilot Server
```

Controls:

```text
Services
Containers
Tail
Since
Until
Timestamps
Follow
Find in loaded logs
```

Keep these controls compact. The Agent identity is implied by the selected
project and is not shown as a filter row. At desktop widths, scope and time
filters use a dense grid, display options and stream actions use one short
footer row, and browser-only Find sits immediately above the log output. The
layout may stack only as the viewport requires it.

Rules:

- Time range means querying logs still retained by Docker Engine, not Dockpilot historical storage.
- Follow and bounded range controls must not enter contradictory states.
- Scrolling away pauses auto-follow; `Jump to latest` resumes.
- `Clear view` clears browser display only.
- Preserve and show dropped lines/bytes from Dockpilot relay.
- On reconnect, do not promise gap-free resume unless a separate continuity contract exists.
- If Docker logging configuration cannot provide readable logs, show disabled/unavailable with reason.
- Multi-Container merge order is observational, not causal/audit ordering.

## 13. Compose project → Backups

Title copy:

> Dockpilot-managed configuration backups

Explain exactly what is included and not included.

Typical Included:

- approved Compose files
- approved interpolation/environment files that Dockpilot manages

Typical Not included unless explicitly supported:

- service env files outside allowed scope
- secret source files outside allowed scope
- arbitrary include/extends dependencies
- build context
- bind-mounted arbitrary files/directories
- Docker Volumes
- database/application data
- external resources

Restore confirmation states:

- selected managed files will be replaced
- current managed files are snapshotted first according to the recovery contract
- Volume/application data is not restored
- Cancel is not rollback

`restore_recovery_required` is a severe project-wide state that blocks unsafe mutation.

## 14. Compose project → Activity

Activity is project-filtered stored Audit history.

Suggested columns:

```text
TIME | KIND | ACTION | RESOURCE | ACTOR
```

Do not present Audit coverage as project-specific coverage. If host coverage is uncertain:

```text
Audit continuity for production-01 is uncertain.
Some project activity may be missing.
View host Audit →
```

## 15. Host → Containers

Table:

```text
COMPOSE PROJECT | SERVICE | CONTAINER | STATE | HEALTH | IMAGE | PORTS | PROTECTION | ACTIONS
```

Standalone Containers use `—` for Compose project / service.

Filters:

- State
- Compose: All / Compose / Standalone
- Search by Container, Image, project, service

Priority columns at narrow width:

1. Compose project
2. Service
3. Container
4. State

Health/Image/Ports may move to the details panel progressively.

Protected Dockpilot Agent Container remains visible, with destructive actions disabled and reasons shown.

Container actions use the same final `…` menu and state/protection rules as the
project Container list. They call Docker Container operations directly; they
must not be labeled as Compose Service operations or imply configuration is
being applied.

## 16. Container details

The shared right-side details panel used by Containers, Images, Networks, Volumes,
and operation details is resizable above the full-page mobile breakpoint.
Pointer users drag its left boundary; keyboard users focus the vertical
separator and use Left/Right Arrow. The browser stores one shared local width.
The allowed range preserves at least 420px of main content on pushed desktop
layouts and never exceeds 70% of an overlaid viewport. At 800px and below the
The details panel remains a fixed full-page surface and exposes no resize control.

Sections:

```text
General
State
Runtime
Compose project / service
Ports
Networks
Mounts
```

Potential fields:

- Container ID
- Image / Image ID
- Docker status
- Created / started / finished
- Exit code
- OOM killed
- Restart count
- Health state/detail
- Restart policy
- Stop signal/timeout
- Logging driver
- Command / Entrypoint
- Compose project/service
- One-off/Orphan status
- published Host IP/port -> target port/protocol
- network IPv4/IPv6/MAC/aliases
- mount type/source/destination/read-only

Mounts distinguish `volume`, `bind`, `tmpfs`, and other actual Docker mount types rather than calling everything a Volume.

Environment/raw inspect is sensitive and should require explicit reveal if added.

## 17. Host → Images

Table:

```text
REPOSITORY / TAGS | IMAGE ID | CREATED | SIZE | CONTAINERS
```

One Image ID is one row; multiple tags are references on that row.

Do not conflate:

- Untagged
- Unused
- Referenced by stopped/running Container

Details panel:

```text
General
Tags / digests
Usage
Image configuration
Platform
Metadata
Size details where available
```

Clarify that Size is cumulative image and parent-layer size, not unique disk use. If shared/unique size is available, display separately.

## 18. Host → Networks

Table:

```text
NETWORK | DRIVER | SCOPE | IPAM | CONTAINERS
```

Do not assume exactly one subnet.

Details panel:

```text
General
IPAM configs (IPv4/IPv6/multiple subnets)
Connected Containers
IP addresses / MAC address
Options
Labels
Compose project / service where labels prove it
```

Compose ownership comes from actual labels, not name-pattern inference.

## 19. Host → Volumes

Table:

```text
COMPOSE PROJECT | VOLUME | DRIVER | SCOPE | REFERENCES | SIZE(optional)
```

Usage wording:

```text
Referenced by 3 containers
2 running · 1 stopped
```

Size may be `Calculating…` or `Unavailable`; never convert unknown to `0 B`.

Details panel:

```text
General
Compose project / service
Referenced Containers + mount destinations
Driver options
Labels
Size when available
```

Mountpoint is shown with Driver and should not be interpreted as a normal local directory for every custom/remote driver.

## 20. Host → Container stats

Title/help:

```text
Container stats
Live · Container stats are collected only while this view is open.
Not retained by Dockpilot.
```

Views:

```text
[ Hierarchy ] [ Top containers ]
```

### Hierarchy

```text
Docker host
  Compose project
    Service
      Container
  Standalone
    Container
```

Columns:

```text
NAME | CPU % | MEMORY USAGE / LIMIT | NET I/O (RX / TX) | BLOCK I/O (READ / WRITE) | STATE
```

Host row means aggregate managed Docker Container usage against Engine-reported capacity, not host OS usage.

### Top containers

Flat comparison of the same current sample, sortable by CPU, memory, network, block I/O.

Stability rules:

- Values update every sample.
- Hierarchy row positions do not jump.
- Top list reorders less frequently than samples or on explicit sort refresh.
- No metric threshold colors.

Technical rules:

- CPU may exceed 100%.
- Memory should follow Docker-compatible semantics where contractually adopted.
- An unbounded memory-limit state is shown as
  `No container memory limit · {usage} used`, not as the internal term `Unbounded`.
- Network/Block `/s` rates require delta / actual observed elapsed time.
- First/reset sample displays unavailable/waiting rather than zero.
- `Processes / threads` is more accurate than implying Docker PIDS is process-only.
- Shared network namespace must not be blindly double-counted in aggregate; if safe de-duplication is unavailable, expose aggregate limitation.

## 21. Host → Audit

Subtitle:

> Stored audit history for this Docker host

Coverage warning distinguishes confirmed gap from continuity uncertainty.

Filters:

```text
From / Until
Resource
Kind
Actor
```

Table:

```text
TIME | KIND | ACTION | RESOURCE | ACTOR
```

Actor is technical provenance only:

- `ui:<source_ip>`
- `system`
- webhook identifier if actually present
- `unknown`

Never invent person/admin identity.

Event details may show:

- exact timestamp + timezone
- source/kind
- actor
- resource
- related Operation
- agent/incarnation/sequence technical evidence when useful

Candidate observed Docker events for contract review include OOM/restart/pause/unpause. They are evidence, not alert severity.

## 22. Host-scoped multi-Container Logs — P1 candidate

Do not add a Host `Logs` primary tab solely for this.

Potential route:

```text
Host → Containers → select several Containers → View logs
```

This opens the common log viewer scoped to the selected Containers.

## 23. Errors / empty / partial states

Never reuse one generic empty state for all cases.

Distinguish at minimum:

- no objects exist
- search has no matches
- Docker Engine unavailable
- capability unavailable
- query failed
- never scanned
- discovery incomplete/truncated
- stale last-known data
- partial cross-host result

Error copy should communicate:

1. what happened
2. what data/action is affected
3. whether currently displayed data is fresh
4. what the user can do next
5. technical reason, progressively disclosed

## 24. Resolved product decision

### Compose build policy

The v1 Compose build policy in §8 is approved. Dockpilot never builds, never
falls back to build, and never silently skips build-required Services. The UI
must use effective-model facts and exact unavailable reasons; it must not infer
build capability from filenames, Container names, or CLI error text.
