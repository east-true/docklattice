# DockLattice Web UI — Acceptance Checklist

## App shell / IA

- [ ] Sidebar contains DockLattice wordmark, Search, Home, and registered Docker hosts only.
- [ ] No user/avatar/settings controls are added.
- [ ] Host-selected main navigation is exactly: Summary / Compose / Containers / Images / Networks / Volumes / Container stats / Audit.
- [ ] Compose project navigation is exactly: Summary / Services / Containers / Files / Logs / Backups / Activity.
- [ ] Services is a project-level collection tab; an individual Service is not
      introduced as a separate route/page depth.

## Brand / visual

- [ ] Primary interaction color is `#1D6FC2` with the approved supporting palette.
- [ ] Light-first design; no gradient/glass/neon aesthetics.
- [ ] Normal states are not over-badged or painted semantic green.
- [ ] Tables/lists dominate repeated operational objects; giant KPI cards are absent.

## Home

- [ ] Home prioritizes Search, compact filters, Needs attention, and Docker hosts.
- [ ] No Host OS CPU/memory metrics appear.
- [ ] Attention contains deterministic DockLattice-known exceptions only.
- [ ] A partial/unavailable host cannot silently disappear and look like fewer resources.

## Operations

- [ ] Active long operations remain visible after navigation.
- [ ] Operation phase semantics are preserved.
- [ ] Cancel is never called rollback.
- [ ] Output is labeled `Output tail` when bounded.
- [ ] Unknown operation state remains unknown.
- [ ] A newly accepted mutation shows an icon-labeled blue `Started` Toast,
      never success; the same Toast becomes green, amber, or red only after
      the authoritative operation reaches the corresponding terminal state.
- [ ] Operation Toasts provide `View operation`, an explicit dismiss button,
      status/alert semantics, and do not cover an open desktop details panel.

## Host Summary

- [ ] Host, Docker Engine, and Compose projects use separate outer panels in
      that order; Agent/DockLattice, Engine, and project facts are not mixed.
- [ ] Docker Engine overview and advanced facts share the Engine panel but are
      separated by spacing and a subsection heading; overview fields are not
      repeated and no redundant divider appears.
- [ ] Host metadata appears before current capability states.
- [ ] Docker Engine presents running-Container CPU and memory usage as
      `used / total`, with logical CPUs and IEC memory units; the panel states
      that Host processes outside Docker are excluded and shows observation
      freshness.

## Compose semantics

- [ ] Compose Project Summary uses separate `Project` and `Containers` outer
      panels in that order; the full effective Service model is not duplicated
      in Summary.
- [ ] Effective Compose metadata appears before the separately spaced
      `DockLattice management` subsection; observed Compose Container counts
      remain in the Containers panel.
- [ ] Compose Summary ends with an exception-only `Services needing attention`
      list for missing Containers, abnormal runtime/Health, and no-build policy
      blockers. It does not repeat healthy Services or treat inactive profiles
      as failures.
- [ ] The Services tab owns the effective Service classification table and
      a final row-menu control without duplicating project-wide action buttons;
      Service `Down` is not exposed.
- [ ] The Service table uses one semantic fact per column: Service, Status,
      Containers, Health, Image, Build, Pull policy, Profiles, Depends on,
      and Ports. Its final utility column has no visible heading.
- [ ] The final utility column contains only a quiet `…` trigger; it opens a
      state- and policy-aware menu directly below the row for per-Service
      `Pull / Up | Start / Stop / Restart` operations.
- [ ] Each Service value remains one line high. Long values are ellipsized with
      the complete value available as a tooltip, not wrapped into taller two-
      or three-line cells.
- [ ] Host → Compose retains the selected Docker host as the page title;
      `Compose projects` is the list section, not a replacement Host identity.
- [ ] The Compose Project list keeps the current Container count separate from
      `Last observed`; stale or missing Projects show an unavailable count rather
      than placing a timestamp in the `Containers` column.
- [ ] `No container`, `Excluded by profile`, `One-off`, `Orphan`, and actual Docker Container states are distinguishable.
- [ ] Pull completion does not claim running Containers were updated.
- [ ] Configuration save does not suggest Restart as the apply mechanism.
- [ ] Header actions are ordered `Pull / Up / Down | Start / Stop / Restart`;
      the two groups distinguish applying or removing the Compose project from
      existing-Container control.
- [ ] Project-level Pull, Up, and Down each require confirmation. Pull explains
      that it does not start or build; Up explains create/recreate/start and
      unconditional `--no-build`; Down matches actual removal semantics.
- [ ] Every Up invocation includes `--no-build`; DockLattice exposes no Build action or build fallback.
- [ ] Pull explicitly targets effective Services with a declared Image; mixed `image` + `build` Services remain pullable without `--ignore-buildable`.
- [ ] Build-only and `pull_policy: build` Services have explicit unavailable reasons for Pull and Service Up.
- [ ] Project Up is unavailable when its effective target set contains any build-required Service; the UI never silently skips one.

## Files / sensitive values

- [ ] Merge order / include / extends / interpolation env / service env / secret/config source are not flattened into one misleading category.
- [ ] Source category headings and actual source items have visibly different
      hierarchy, indentation, and item treatment at desktop and mobile widths.
- [ ] `.env` is not mislabeled as a Docker Secret.
- [ ] `docker compose config` output requires explicit reveal because it may expose resolved sensitive values.
- [ ] Hash/concurrency conflicts cannot silently overwrite external edits.
- [ ] Save does not imply Compose changes were applied.

## Logs

- [ ] UI states Docker Engine retention, not DockLattice retention.
- [ ] Scope/time filters use a compact grid; Agent ID is not repeated as a
      visible filter, and browser-only Find remains adjacent to loaded output.
- [ ] Tail/Since/Until/Follow semantics cannot form contradictory states.
- [ ] Scrolling away pauses follow; user can jump to latest.
- [ ] `Clear view` only clears the browser view.
- [ ] Dropped relay lines/bytes are visible.
- [ ] Reconnection does not promise gap-free resume without a contract.
- [ ] Logging-driver inability is shown as unavailable with reason.

## Inventory / details panel

- [ ] Container table shows Compose project and service context where proven.
- [ ] Standalone Containers remain visible.
- [ ] Protected DockLattice Agent Container remains visible with blocked destructive actions and reasons.
- [ ] Each real Container row has an unlabeled final utility column with the
      same quiet `…` menu used by Services for state-aware Start / Stop /
      Restart / Remove; placeholder `No container` rows do not.
- [ ] Container Restart and Remove require confirmation. Remove is non-forced,
      requires a stopped Container, retains attached Volumes, and never implies
      a Compose configuration change.
- [ ] Container details distinguish published ports from the image configuration's exposed ports.
- [ ] Mount types are explicit; bind mounts are not called Volumes.
- [ ] Network details support multiple IPAM configurations and subnets.
- [ ] Volume size is not assumed cheap/available and unknown is never shown as zero.
- [ ] Image untagged and unused states are not conflated.

## Container stats

- [ ] Entering the view starts viewer-scoped collection; leaving stops it when no viewers remain.
- [ ] Host row is not labeled as Host OS utilization.
- [ ] CPU >100% is allowed.
- [ ] Missing/unavailable metrics are not zero.
- [ ] A Container without a memory limit is labeled
      `No container memory limit · {usage} used`; the UI does not expose the internal
      `Unbounded` term or imply infinite physical memory.
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
- [ ] List column separators support pointer drag and focused Left/Right Arrow
      resizing; adjacent columns share the fixed available table width and the
      browser-local preference introduces no horizontal table or page overflow.
- [ ] First-column object names are explicit links rather than invisible row-click-only controls.
- [ ] Visible focus for all interactive controls.
- [ ] Modal dialogs trap focus and restore it on close.
- [ ] The non-modal details panel does not trap focus.
- [ ] Above 800px, the details panel's left boundary supports pointer drag and
      focused Left/Right Arrow resizing, persists one browser-local width
      across object types, preserves at least 420px of pushed desktop main
      content, and introduces no horizontal page overflow.
- [ ] At 800px and below, the details panel remains full-page and does not expose
      a resize separator.
- [ ] Status never relies only on color.
- [ ] `prefers-reduced-motion` is respected.
- [ ] Frequent metrics updates do not spam assistive live regions.

## Responsive visual acceptance

Manually test at minimum:

- [ ] 1440px: Sidebar + Main + details panel
- [ ] 1280px: dense tables remain usable
- [ ] 1024px: priority columns remain readable with the details-panel policy applied
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
- [ ] Orphan / One-off / Excluded by profile / No container
- [ ] logs unavailable
- [ ] Container stats disconnected/stale/dropped frames
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
  the Host Summary Engine disclosure and management hierarchy, the no-build
  Compose policy and blocked Project Up, responsive navigation, and the
  route-aware non-modal/full-width Container details;
- each viewport attaches full-page screenshots to `playwright-report`, which
  can be viewed with `npm run test:ui:report` or exercised interactively with
  `npm run test:ui:open`;
- the current fixture run completes with 112 passed and 63 intentional skips:
  3 viewport-inapplicable navigation cases and 60 opt-in live-VM cases across
  the five configured viewports.

The Go integration and race suites, `go vet`, JavaScript syntax validation,
release-scope checks, and Docker Compose CLI option/model smoke check also pass.

## Live VM destructive implementation evidence — 2026-08-30

The opt-in `tests/ui/vm-acceptance.spec.mjs` suite runs against production
Server and Agent images on an Ubuntu 24.04 VM with Docker Engine 29.1.3 (API
1.52) and the Agent-bundled Docker Compose 5.5.0. It requires explicit
`DOCKLATTICE_VM_ACCEPTANCE=1`, HTTPS base URL, SSH host, and SSH key variables, so
the destructive cases cannot run accidentally in the normal fixture suite.

Prepare only a disposable `dp-vm-*` guest. Load the exact Server and Agent
images under test, copy `scripts/setup-vm-ui-acceptance.sh` into the guest, and
run it there with those two Image references. The setup script refuses any
other hostname and only replaces the dedicated `docklattice-acceptance-*`,
`docklattice-server`, and `docklattice-agent` resources. The Docker socket GID
is host-specific and must be passed to Playwright:

```sh
./scripts/setup-vm-ui-acceptance.sh "$server_image" "$agent_image"

DOCKLATTICE_VM_ACCEPTANCE=1 \
PLAYWRIGHT_TEST_BASE_URL="https://$vm_ip:8080" \
DOCKLATTICE_VM_EVIDENCE_DIRECTORY="$evidence_directory" \
DOCKLATTICE_VM_SSH_HOST="$vm_ip" \
DOCKLATTICE_VM_SSH_USER=lab \
DOCKLATTICE_VM_SSH_KEY="$vm_ssh_key" \
DOCKLATTICE_VM_DOCKER_SOCKET_GID="$docker_socket_gid" \
npm run test:ui -- tests/ui/vm-acceptance.spec.mjs \
  --project=desktop-1440 --workers=1
```

The setup certificate is valid for 30 days by default and includes the guest
hostname and discovered IPv4 addresses in its subject alternative names. Set
`DOCKLATTICE_ACCEPTANCE_CERT_DAYS` to an integer from 1 through 365 when a
different disposable-lab lifetime is required.

On a persistent libvirt test host, enable VM autostart and keep lifecycle
evidence outside the terminal session:

```sh
virsh autostart "$vm_name"
(crontab -l 2>/dev/null; printf '%s\n' \
  "@reboot $repository/scripts/monitor-vm-lifecycle.sh") | \
  awk '!seen[$0]++' | crontab -
setsid -f "$repository/scripts/monitor-vm-lifecycle.sh"
```

The monitor records the host boot ID, every domain's initial state, and
subsequent libvirt events in
`~/.local/state/docklattice/vm-lifecycle.log`. A log that ends abruptly and
resumes with a different boot ID identifies a host restart even when libvirt
could not persist a domain shutdown reason.

The current serial desktop run completed with 12 passed in 1.9 minutes. It
exercised:

- live Host, Compose project, responsive navigation, details panels, Logs, Files,
  secret reveal, optimistic-write conflicts, backups, and restore-without-Up;
- the no-build policy through real Pull, Up, Restart, Stop, and Start
  operations, including the admitted `image + build` Service;
- malformed and duplicate JSON fields, traversal and absolute file paths,
  unmanaged file reads, a request over the body limit, command-shaped kind and
  target values, operation-ID spec mismatch, and read-only/collision mutation
  attempts;
- an eight-request same-project mutation storm and explicit cancellation;
- rapid Metrics route churn and modal focus containment;
- Agent stop, a missing Compose plugin with the Agent and Docker capabilities
  retained but Compose disabled, an inaccessible Docker socket that fails the
  Agent closed, Server restart during an active operation, and recovery of each
  dependency;
- real Compose Down followed by Engine and UI reconciliation checks: current
  model Service Containers are removed, one-off/orphan Containers and their
  still-used Networks remain, the named Volume retains the same mountpoint,
  and Up restores web/worker/nolog without building.

Every injected failure has a `finally` recovery path. After the completed run,
the Docker socket was `0660` with the expected Docker group, Server and Agent
were running, web was healthy, and worker/nolog were running. Screenshots are
written to the configured VM evidence directory.

After the owner identified redundant Host Summary sections, the focused live
VM case was rerun against the rebuilt production Server image. It verifies
separate Host, Docker Engine, and Compose projects panels; Engine overview and
technical sections without repeated version or storage driver; and Host
metadata positioned before current capability states.

## Five-viewport live visual evidence — 2026-08-24

`npm run test:ui:vm:visual` is a read-only Playwright visual gate for an already
running live environment. Set `PLAYWRIGHT_TEST_BASE_URL` and optionally
`DOCKLATTICE_VM_EVIDENCE_DIRECTORY`; the command waits for the Agent and Docker
capability, captures the five required viewports, rejects browser errors, and
rejects document-level horizontal overflow.

The completed live-VM run passed 5/5:

- 1440px: Sidebar, main Container table, and non-modal details panel;
- 1280px: dense Container table;
- 1024px: Container table with the details-panel policy applied;
- 768px: collapsed Sidebar control, single-line scrollable Host tabs, separate
  Host/Engine/Compose panels, a single-column Engine technical section, and
  metadata-before-capabilities Host hierarchy;
- 375px: full-page, internally scrollable, one-column Container details.

Direct screenshot review found the initial 375px details panel still using two
definition columns. The responsive CSS was corrected to one column below
480px, and both fixture and live visual gates now assert that layout. The CSS
asset is also included in the repository Prettier gate so it remains readable
rather than returning to compressed one-line rules.

Owner inspection then found that Host Summary displayed the Docker Engine
overview and Engine details as two always-visible panels with repeated facts,
and placed management capability states before session/discovery metadata. The
final hierarchy uses separate `Host`, `Docker Engine`, and `Compose projects`
panels. Host metadata appears above a spaced `Capabilities` section, while
Engine technical facts remain visible as a spaced subsection in the Engine
panel without a redundant divider or repeated overview facts. Five-viewport
fixture coverage, the focused production VM case, and the repeated 5/5 live
visual gate verify the corrected hierarchy.

The final Compose-focused VM case also verifies that Host → Compose retains
the selected Host title, `Containers` and `Last observed` remain separate, and
pointer/keyboard column resizing redistributes adjacent widths within the fixed
table width. Reloaded browser-local proportions persist without introducing
horizontal table or page overflow.

These captures support, but do not replace, the human checklist at the top of
this file. Repeat the visual checks against the revision being promoted; focus
feel, density, color, and interaction semantics cannot be inferred from an old
capture or an automated assertion alone.

**Historical acceptance baseline:**
`706f1d0578b1fb2511d17746391c7abfcec634c8`. Current promotion evidence belongs
in the pull request and release checks for the revision under review.
