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
      ├─ Live Metrics
      └─ Audit
```

Object home remains under its Docker host. Global Search routes directly to that real context.

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

## 5. Host Summary

Role:

> What is this Docker Engine, what can Dockpilot currently do on it, and what requires attention?

Sections:

### Docker Engine

```text
Engine version
Containers        total / running / stopped
Images
CPU capacity
Memory capacity
Storage driver
```

CPU and memory are Engine-reported capacity, not host OS utilization.

### Engine details

Progressively disclose:

```text
Docker Engine version
Engine API version
Docker Compose version
Storage driver
Logging driver
Cgroup driver/version
Default runtime
OS type
Architecture
Kernel
Docker root dir
```

### Dockpilot management

```text
Agent connection
Docker Compose capability
Discovery capability / last completed scan
File access
Live Metrics capability
Operation recovery
```

### Compose exceptions

Show only exceptional projects, then `Open Compose`.

## 6. Host → Compose

Subtitle:

> Compose projects discovered on this Docker host

Actions:

- Refresh current view
- `Rescan projects` as a distinct discovery operation

Suggested table:

```text
PROJECT | SERVICES | CONTAINERS | CONFIGURATION | NEEDS ATTENTION
```

Project cell contains project name + working directory.

Service/runtime counts must not look authoritative if the source graph is incomplete.

No `New Project`.

## 7. Compose project → Summary

Header:

```text
backend
/srv/compose/backend
[ Pull ] [ Up ] [ Restart ] [ More ]
```

Tabs:

```text
Summary | Containers | Files | Logs | Backups | Activity
```

Sections:

### Runtime summary

- defined services
- existing Containers by Docker state
- services without Container
- profile-inactive services
- one-off/orphan count when relevant

### Effective Compose context

```text
Project name
Project directory
Compose files and merge order
Included applications
Extends references
Active profiles
Source graph complete/incomplete
```

### Configuration

```text
In sync / Changed / No baseline
Last verified
Known source references
```

### Management

```text
Compose operations
File access
Discovery record
Managed/unmanaged
```

Severe exceptions appear above all sections.

## 8. Compose operations semantics

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

| Effective Service model | Pull | Service Up | Project Up |
|---|---|---|---|
| `image` | Available | Available with `--no-build` | Included |
| `image` + `build` | Available for the declared Image only | Available with `--no-build` | Included |
| `build` only | Unavailable | Unavailable | Blocks the whole Project Up |
| `image` + `build` + `pull_policy: build` | Unavailable | Unavailable | Blocks the whole Project Up |

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
Service-level Up remains available only for an image-backed, non-build-required
Service.

Example unavailable reason:

```text
This Compose project contains 1 build-only Service: worker.
Dockpilot v1 does not build Images. Provide an Image for this Service before
running the whole project with Dockpilot.
```

## 9. Compose project → Containers

Primary table:

```text
SERVICE | CONTAINER | STATE | HEALTH | IMAGE | PORTS
```

Represent these explicitly:

- ordinary Container state (`Running`, `Exited`, `Paused`, `Restarting`, `Created`, `Removing`, `Dead`)
- `No container`
- `Profile inactive`
- `One-off`
- `Orphan`

`One-off` and `Orphan` should normally be secondary labels under the Container identity rather than extra table columns.

Service names are filters/shortcuts, not separate pages.

## 10. Compose project → Files

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

### Resolved configuration

Explicit reveal is required because resolved/interpolated output can contain sensitive values.

## 11. Compose project → Logs

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

Rules:

- Time range means querying logs still retained by Docker Engine, not Dockpilot historical storage.
- Follow and bounded range controls must not enter contradictory states.
- Scrolling away pauses auto-follow; `Jump to latest` resumes.
- `Clear view` clears browser display only.
- Preserve and show dropped lines/bytes from Dockpilot relay.
- On reconnect, do not promise gap-free resume unless a separate continuity contract exists.
- If Docker logging configuration cannot provide readable logs, show disabled/unavailable with reason.
- Multi-Container merge order is observational, not causal/audit ordering.

## 12. Compose project → Backups

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

## 13. Compose project → Activity

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

## 14. Host → Containers

Table:

```text
COMPOSE PROJECT | SERVICE | CONTAINER | STATE | HEALTH | IMAGE | PORTS
```

Standalone Containers use `—` for Compose context.

Filters:

- State
- Compose: All / Compose / Standalone
- Search by Container, Image, project, service

Priority columns at narrow width:

1. Compose project
2. Service
3. Container
4. State

Health/Image/Ports may move to Inspector progressively.

Protected Dockpilot Agent Container remains visible, with destructive actions disabled and reasons shown.

## 15. Container Inspector

Sections:

```text
General
State
Runtime
Compose context
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

## 16. Host → Images

Table:

```text
REPOSITORY / TAGS | IMAGE ID | CREATED | VIRTUAL SIZE | REFERENCES
```

One Image ID is one row; multiple tags are references on that row.

Do not conflate:

- Untagged
- Unused
- Referenced by stopped/running Container

Inspector:

```text
General
References / digests
Usage
Image configuration
Platform
Metadata
Size details where available
```

Clarify that virtual size is not identical to unique disk use. If shared/unique size is available, display separately.

## 17. Host → Networks

Table:

```text
NETWORK | DRIVER | SCOPE | IPAM | CONTAINERS
```

Do not assume exactly one subnet.

Inspector:

```text
General
IPAM configs (IPv4/IPv6/multiple subnets)
Connected Containers
Docker addresses / MAC
Options
Labels
Compose context where labels prove it
```

Compose ownership comes from actual labels, not name-pattern inference.

## 18. Host → Volumes

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

Inspector:

```text
General
Compose context
Referenced Containers + mount destinations
Driver options
Labels
Size when available
```

Mountpoint is shown with Driver and should not be interpreted as a normal local directory for every custom/remote driver.

## 19. Host → Live Metrics

Title/help:

```text
Live Metrics
Live · Metrics are collected only while this view is open.
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
NAME | CPU | MEMORY | NET RX/TX | BLOCK R/W | STATE
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
- Network/Block `/s` rates require delta / actual observed elapsed time.
- First/reset sample displays unavailable/waiting rather than zero.
- `Processes / threads` is more accurate than implying Docker PIDS is process-only.
- Shared network namespace must not be blindly double-counted in aggregate; if safe de-duplication is unavailable, expose aggregate limitation.

## 20. Host → Audit

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

Event Inspector may show:

- exact timestamp + timezone
- source/kind
- actor
- resource
- related Operation
- agent/incarnation/sequence technical evidence when useful

Candidate observed Docker events for contract review include OOM/restart/pause/unpause. They are evidence, not alert severity.

## 21. Host-scoped multi-Container Logs — P1 candidate

Do not add a Host `Logs` primary tab solely for this.

Potential route:

```text
Host → Containers → select several Containers → View logs
```

This opens the common log viewer scoped to the selected Containers.

## 22. Errors / empty / partial states

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

## 23. Resolved product decision

### Compose build policy

The v1 Compose build policy in §8 is approved. Dockpilot never builds, never
falls back to build, and never silently skips build-required Services. The UI
must use effective-model facts and exact unavailable reasons; it must not infer
build capability from filenames, Container names, or CLI error text.
