# Dockpilot Web UI — Acceptance Checklist

## App shell / IA

- [ ] Sidebar contains Dockpilot wordmark, Search, Home, and registered Docker hosts only.
- [ ] No user/avatar/settings controls are added.
- [ ] Host-selected main navigation is exactly: Summary / Compose / Containers / Images / Networks / Volumes / Live Metrics / Audit.
- [ ] Compose project navigation is exactly: Summary / Containers / Files / Logs / Backups / Activity.
- [ ] Service is not introduced as a separate route/page depth.

## Brand / visual

- [ ] Primary interaction color is `#1D6FC2` with the approved supporting palette.
- [ ] Light-first design; no gradient/glass/neon aesthetics.
- [ ] Normal states are not over-badged or painted semantic green.
- [ ] Tables/lists dominate repeated operational objects; giant KPI cards are absent.

## Home

- [ ] Home prioritizes Search, compact filters, Needs attention, and Docker hosts.
- [ ] No Host OS CPU/memory metrics appear.
- [ ] Attention contains deterministic Dockpilot-known exceptions only.
- [ ] A partial/unavailable host cannot silently disappear and look like fewer resources.

## Operations

- [ ] Active long operations remain visible after navigation.
- [ ] Operation phase semantics are preserved.
- [ ] Cancel is never called rollback.
- [ ] Output is labeled `Output tail` when bounded.
- [ ] Unknown operation state remains unknown.

## Compose semantics

- [ ] `No container`, `Profile inactive`, `One-off`, `Orphan`, and actual Docker Container states are distinguishable.
- [ ] Pull completion does not claim running Containers were updated.
- [ ] Configuration save does not suggest Restart as the apply mechanism.
- [ ] Down confirmation matches actual invocation semantics.
- [ ] Every Up invocation includes `--no-build`; Dockpilot exposes no Build action or build fallback.
- [ ] Pull explicitly targets effective Services with a declared Image; mixed `image` + `build` Services remain pullable without `--ignore-buildable`.
- [ ] Build-only and `pull_policy: build` Services have explicit unavailable reasons for Pull and Service Up.
- [ ] Project Up is unavailable when its effective target set contains any build-required Service; the UI never silently skips one.

## Files / sensitive values

- [ ] Merge order / include / extends / interpolation env / service env / secret/config source are not flattened into one misleading category.
- [ ] `.env` is not mislabeled as a Docker Secret.
- [ ] Resolved config requires explicit reveal because it may expose expanded sensitive values.
- [ ] Hash/concurrency conflicts cannot silently overwrite external edits.
- [ ] Save does not imply Compose changes were applied.

## Logs

- [ ] UI states Docker Engine retention, not Dockpilot retention.
- [ ] Tail/Since/Until/Follow semantics cannot form contradictory states.
- [ ] Scrolling away pauses follow; user can jump to latest.
- [ ] `Clear view` only clears the browser view.
- [ ] Dropped relay lines/bytes are visible.
- [ ] Reconnection does not promise gap-free resume without a contract.
- [ ] Logging-driver inability is shown as unavailable with reason.

## Inventory / Inspector

- [ ] Container table shows Compose project and service context where proven.
- [ ] Standalone Containers remain visible.
- [ ] Protected Dockpilot Agent Container remains visible with blocked destructive actions and reasons.
- [ ] Container Inspector distinguishes published ports from image exposed ports.
- [ ] Mount types are explicit; bind mounts are not called Volumes.
- [ ] Network Inspector supports multiple IPAM configs/subnets.
- [ ] Volume size is not assumed cheap/available and unknown is never shown as zero.
- [ ] Image untagged and unused states are not conflated.

## Live Metrics

- [ ] Entering the view starts viewer-scoped collection; leaving stops it when no viewers remain.
- [ ] Host row is not labeled as Host OS utilization.
- [ ] CPU >100% is allowed.
- [ ] Missing/unavailable metrics are not zero.
- [ ] Rate values are derived from real sample deltas, not cumulative counters mislabeled `/s`.
- [ ] Hierarchy row order remains stable during live updates.
- [ ] Top Containers does not reorder on every sample.
- [ ] No charts/history/threshold alerts are introduced.

## Audit / Activity

- [ ] Audit is visibly stored/durable and distinct from Logs.
- [ ] Project Activity is a filtered Audit view, not separate coverage semantics.
- [ ] Confirmed Audit gap and continuity uncertainty are distinct.
- [ ] Actor is technical provenance, not invented human identity.
- [ ] Time-range navigation exists when API support is added; UI does not fake full-text/time-range support before then.

## Accessibility

- [ ] Native semantic tables/links/buttons are used where possible.
- [ ] First-column object names are explicit links rather than invisible row-click-only controls.
- [ ] Visible focus for all interactive controls.
- [ ] Modal dialogs trap focus and restore it on close.
- [ ] Non-modal Inspector does not trap focus.
- [ ] Status never relies only on color.
- [ ] `prefers-reduced-motion` is respected.
- [ ] Frequent metrics updates do not spam assistive live regions.

## Responsive visual acceptance

Manually test at minimum:

- [ ] 1440px: Sidebar + Main + Inspector
- [ ] 1280px: dense tables remain usable
- [ ] 1024px: priority columns remain readable with Inspector policy applied
- [ ] 768px: collapsible Sidebar; Host tabs remain single-line scroll/overflow
- [ ] 375px: detail becomes single-column/full page; no mandatory two-dimensional UI is clipped without an accessible path

Test representative states:

- [ ] all normal
- [ ] Agent disconnected
- [ ] Docker unavailable
- [ ] Compose unavailable
- [ ] discovery incomplete/truncated
- [ ] zero projects vs failed discovery
- [ ] read-only project
- [ ] collision
- [ ] restore recovery required
- [ ] configuration changed / no baseline
- [ ] orphan / one-off / profile inactive / no container
- [ ] logs unavailable
- [ ] live metrics disconnected/stale/dropped frames
- [ ] Audit gap / continuity uncertain
- [ ] active cancellable operation / non-cancellable operation / unknown result
- [ ] many hosts
- [ ] many projects
- [ ] long names/paths/digests

## Automated implementation evidence — 2026-08-23

The unchecked boxes above remain the human acceptance record, especially for
real-Agent and destructive confirmation states. They are not silently marked
complete by a fixture test. The implementation now also has a repeatable
Playwright gate:

- `npm run test:ui` serves the actual embedded UI assets with the production
  Content Security Policy and intercepts API reads with bounded deterministic
  fixtures;
- Chromium runs at 1440, 1280, 1024, 768, and 375 pixel viewports;
- the suite covers the host-only sidebar, Home attention/partial availability,
  the no-build Compose policy and blocked Project Up, responsive navigation,
  and the route-aware non-modal/full-width Container Inspector;
- each viewport attaches full-page screenshots to `playwright-report`, which
  can be viewed with `npm run test:ui:report` or exercised interactively with
  `npm run test:ui:open`;
- the current fixture run completes with 32 passed and 58 intentional skips:
  3 viewport-inapplicable navigation cases and 55 opt-in live-VM cases across
  the five normal fixture projects.

The Go integration and race suites, `go vet`, JavaScript syntax validation,
release-scope checks, and Docker Compose CLI option/model smoke check also pass.

## Live VM destructive implementation evidence — 2026-08-24

The opt-in `tests/ui/vm-acceptance.spec.mjs` suite runs against production
Server and Agent images on an Ubuntu 24.04 VM with Docker Engine 29.1.3 (API
1.52) and the Agent-bundled Docker Compose 5.3.1. It requires explicit
`DOCKPILOT_VM_ACCEPTANCE=1`, HTTPS base URL, SSH host, and SSH key variables, so
the destructive cases cannot run accidentally in the normal fixture suite.

One serial desktop run completed with 11 passed in 1.9 minutes. It exercised:

- live Host, Compose project, responsive navigation, Inspectors, Logs, Files,
  secret reveal, optimistic-write conflicts, backups, and restore-without-Up;
- the no-build policy through real Pull, Up, Restart, Stop, and Start
  operations, including the admitted `image + build` Service;
- malformed and duplicate JSON fields, traversal and absolute file paths,
  unmanaged file reads, a request over the body limit, command-shaped kind and
  target values, operation-ID spec mismatch, and read-only/collision mutation
  attempts;
- an eight-request same-project mutation storm and explicit cancellation;
- rapid Metrics route churn and modal focus containment;
- Agent stop, missing Compose plugin, inaccessible Docker socket, Server
  restart during an active operation, and recovery of each dependency;
- real Compose Down followed by Engine and UI reconciliation checks: current
  model Service Containers are removed, one-off/orphan Containers and their
  still-used Networks remain, the named Volume retains the same mountpoint,
  and Up restores web/worker/nolog without building.

Every injected failure has a `finally` recovery path. After the completed run,
the Docker socket was `0660` with the expected Docker group, Server and Agent
were running, web was healthy, and worker/nolog were running. Screenshots are
written to the configured VM evidence directory.

## Five-viewport live visual evidence — 2026-08-24

`npm run test:ui:vm:visual` is a read-only Playwright visual gate for an already
running live environment. Set `PLAYWRIGHT_TEST_BASE_URL` and optionally
`DOCKPILOT_VM_EVIDENCE_DIRECTORY`; the command waits for the Agent and Docker
capability, captures the five required viewports, rejects browser errors, and
rejects document-level horizontal overflow.

The completed live-VM run passed 5/5:

- 1440px: Sidebar, main Container table, and non-modal Inspector;
- 1280px: dense Container table;
- 1024px: Container table with the Inspector policy applied;
- 768px: collapsed Sidebar control and single-line scrollable Host tabs;
- 375px: full-page, internally scrollable, one-column Container Inspector.

Direct screenshot review found the initial 375px Inspector still using two
definition columns. The responsive CSS was corrected to one column below
480px, and both fixture and live visual gates now assert that layout. The CSS
asset is also included in the repository Prettier gate so it remains readable
rather than returning to compressed one-line rules.

These captures support, but do not replace, the unchecked human checklist at
the top of this file. A person must still use the running UI to judge focus
feel, density, color, and interaction semantics before changing the milestone
from `v1 UI implementation complete — integration pending` to
`v1 UI implemented and validated`.

**Verified implementation commit:**
`706f1d0578b1fb2511d17746391c7abfcec634c8`
