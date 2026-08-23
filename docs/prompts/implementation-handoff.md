# Dockpilot Web UI implementation handoff prompt

Work in repository `east-true/dockpilot`.

The design has been reworked after Docker official-document validation and a 30-persona technical/operator/design review. Do not implement from the old Web UI draft documents without first reconciling them with the files below.

Read in this order:

1. repository product/architecture/interface contracts
2. `DESIGN.md`
3. `docs/design/web-ui.md`
4. `docs/design/web-ui-acceptance.md`
5. `docs/design/implementation-gaps.md`
6. existing Live Metrics, Audit, Operation, Discovery, File, Backup contracts
7. current `internal/webui/**` and related Server/Agent code/tests

Rules:

- Existing authoritative architecture/interface contracts still win if they conflict with the UI design.
- Do not silently change API/proto/storage semantics to satisfy a mockup.
- Record desired-UI vs implementation gaps explicitly.
- Compose mutation behavior must follow the approved v1 no-build policy in the
  gap report; do not infer any additional build behavior.
- UI terminology must follow Docker first: Docker host, Docker Engine, Docker Compose, Compose project, Service, Container, Image, Network, Volume.
- Sidebar is Host-first: Dockpilot wordmark, Search, Home, registered Docker hosts.
- Host navigation is Summary / Compose / Containers / Images / Networks / Volumes / Live Metrics / Audit.
- Compose project tabs are Summary / Containers / Files / Logs / Backups / Activity.
- Service is not a page depth.
- Preserve live-vs-stored semantics exactly.
- Docker-provided inspection information may be exposed when useful; Docker-supported mutation is not automatically a Dockpilot feature.
- No Host OS metrics, retained metrics, alert thresholds, fake history, fake users, New Project, generic Settings, giant KPI cards, gradients, charts, or badge spam.
- Long Operations require persistent global feedback with real phase/output evidence.
- Use explicit links in table object cells; do not rely on invisible whole-row click behavior.
- Inspector is route-aware non-modal desktop UI and full-page on narrow screens.
- All unavailable/blocked/partial/stale states must preserve their exact meaning and reason.

Before implementation, produce a gap report mapping every requested UI field/interaction to:

- already supported
- can be derived safely
- requires minimal backend/API extension
- unsupported / product decision required

Do not code until that report is reviewed.

## Current integration status — 2026-08-24

**Milestone:** v1 UI implementation complete — integration pending.

The required gap report was reviewed. Approved slices A and B and the v1
Compose no-build policy are implemented; slice C remains absent. The
implementation has passed the full Go/release gates, five-viewport fixture
Playwright, 11-case destructive live-VM Playwright, and a five-viewport live
visual evidence pass.

The remaining boundary is owner completion of
`docs/design/web-ui-acceptance.md`, followed by commit/push/PR review and merge.
Automated and AI-assisted screenshot review must not mark the human checklist
complete.
