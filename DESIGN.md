# Dockpilot Design System

**Status:** Final UI/UX design direction for implementation review  
**Scope:** First-party Dockpilot Web UI  
**Theme:** Light-first, dense operational workbench

## 1. Product design principles

Dockpilot is a Docker operations console for multiple Docker hosts on an internal network. It is not a generic SaaS dashboard, host-OS monitoring suite, alerting product, or Docker CLI replacement.

The UI should answer, in order:

1. Where should I look?
2. Which Docker/Compose object is involved?
3. What is its current state and evidence?
4. What Dockpilot-supported action is safe and available next?

Use Docker terminology first. Use Dockpilot terminology only for Dockpilot concepts.

Preferred UI nouns:

- Docker host
- Docker Engine
- Docker Compose
- Compose project
- Compose service
- Container
- Image
- Network
- Volume
- Agent
- Operation
- Audit
- Backup
- Discovery

Avoid introducing `Fleet`, `Workload`, `Stack`, `Runtime`, or `Environment` as Dockpilot UI labels where Docker has a more precise term.

## 2. Brand

Dockpilot's product identity is the **Dockpilot wordmark**. Do not introduce an official decorative logo mark in the application shell.

### Primary palette

```text
Primary            #1D6FC2
Primary hover      #175A9E
Primary soft       #E8F1FB
Canvas              #F6FAFC
Surface             #FFFFFF
Text primary        #0F172A
Text secondary      #64748B
Border              #E2E8F0
```

Semantic colors are reserved for actual states. Normal objects should not all become green/blue badges.

- primary blue: selection, navigation, primary actions, links, focus
- warning: deterministic attention states only
- danger: failures, blocked destructive conditions, confirmed gaps
- neutral: ordinary healthy/available/current states

Never communicate status by color alone.

## 3. Typography and density

Use system sans-serif for UI and system monospace for IDs, digests, SHA values, logs, and file contents.

- Default body: 14px
- Labels/meta: 12px
- Page heading: 20px
- 4px base spacing, 8px primary rhythm
- Small radii, low/no shadow, hairline borders
- Tabular numerals for metrics, counts, durations, timestamps

Do not turn the UI into a terminal theme.

## 4. App shell

Desktop structure:

```text
Sidebar | Main content | optional right details panel
```

Sidebar contains only:

```text
Dockpilot
Search…
Home

Docker hosts
  production-01
  10.20.1.27
  production-02
  10.20.1.28
  ...
```

Rules:

- Show every registered Docker host; scroll when necessary.
- Sidebar status represents current Agent session availability only, not aggregate host health.
- Show current session source IP only while a current Agent session exists.
- Do not show user avatar/account/settings in v1.
- Do not add top-level Containers, Images, Networks, Volumes, Logs, Metrics, Compose, or Audit destinations.
- Global search is a shortcut into the object's real host context, not a new global object home.

## 5. Host navigation

Selecting a Docker host opens a host header and flat internal navigation:

```text
Summary | Compose | Containers | Images | Networks | Volumes | Container stats | Audit
```

On narrow widths, horizontally scroll this navigation. Do not wrap it into multiple lines.

## 6. Compose project navigation

A Compose project is a full route with breadcrumb preserving host context.

```text
production-01 / Compose / backend

Summary | Services | Containers | Files | Logs | Backups | Activity
```

The Services collection is a project tab because it owns a complete effective
model and per-Service operations. An individual Service is still context, not a
new page depth: do not introduce a `/services/:name` detail route.

Do not mirror the Host's Engine-wide Images, Networks, or Volumes inventories
under a Compose project. Compose depth exists for the effective model, source
files, project-scoped logs/history, backups, and Compose operations; Engine
objects remain in Host scope.

## 7. Tables

Operational tables are the primary repeated-object primitive.

- Prefer native semantic HTML tables.
- The first cell contains an explicit object link.
- Row hover assists scanning but should not be the only affordance.
- Quick actions use a final `…` overflow control where justified.
- Sticky header is allowed.
- Numeric columns are tabular and normally right-aligned.
- Avoid card grids for large inventories.
- Avoid making every row a button.

## 8. Details panel

Use a route-aware, non-modal right details panel for Container, Image, Network, and Volume detail.

Requirements:

- Selected object is reflected in URL/query state.
- Browser Back closes/changes selection naturally.
- Refresh restores the selected object when possible.
- On narrow layouts, the details panel becomes a full-width detail route.
- The details panel is not a focus-trapping modal.
- Dangerous actions are visually separated.
- Disabled actions show a reason rather than silently disappearing when practical.

Use modal dialogs only for short decisions such as destructive confirmation, restore confirmation, or sensitive-value reveal.

## 9. Data-time semantics

These labels are part of the product contract:

```text
Inventory/Home snapshot     Checked 14:52
Discovery                   Last completed scan 14:47
Container stats             Live · sample 1s ago
Docker logs                 Docker Engine logs · Not retained by Dockpilot Server
Audit / Activity            Stored audit history
Last-known stale value      Last checked 14:32 · current state unavailable
```

Never render missing/unavailable data as zero.

## 10. Status vocabulary

Use these terms consistently:

- `Available`: the capability can currently be used
- `Unavailable`: technically cannot currently be used
- `Blocked`: mutation prevented by a safety rule/state
- `Read-only`: read is available, write is not
- `No container`: active Compose service exists but no corresponding Container exists
- `Excluded by profile`: Compose Service is ignored because none of its profiles are active
- `One-off`: Container created as a one-off Compose run
- `Orphan`: Container remains but no service exists in the current effective Compose model
- `No baseline`: no authoritative Dockpilot baseline exists for comparison
- `Last known`: a past successful observation, not current state

Do not collapse Audit gap and Audit continuity uncertainty into one generic health state.

## 11. Motion

Motion is minimal and functional.

- 120–200ms disclosure/details-panel transition
- no count-up animations
- no live-number tweening
- no blinking Live indicator
- no page-load choreography
- respect `prefers-reduced-motion`

Live tables update values without constantly moving rows.

## 12. Accessibility

- Visible keyboard focus is mandatory.
- Use semantic headings, tables, links, buttons, forms, and dialogs.
- Do not rely on color alone.
- Dialogs trap focus correctly; closing returns focus to the invoking control.
- Non-modal details panel does not trap focus.
- Screen-reader live regions must not announce every metrics sample.
- Technical reasons should be programmatically associated with disabled/error state.

## 13. Responsive rules

- > =1280px: Sidebar + Main + optional details panel
- 1024–1279px: narrow Sidebar, priority columns, details panel may overlay or narrow Main
- 768–1023px: collapsible Sidebar, key columns retained, optional horizontal table scroll
- <768px: navigation drawer, single-column detail; details panel becomes full-page

Dockpilot remains desktop-first.

## 14. Product boundaries

Do not add:

- host OS CPU/RAM/network monitoring
- retained metrics or historical metrics charts
- metric alert thresholds
- registry update notifications unless separately designed
- fake person identity/RBAC UI
- `New Project`
- arbitrary Docker mutation merely because Docker supports it
- decorative dashboard charts
- generic Settings without a real product capability

Docker-provided **inspection data** should be exposed when operationally useful. Docker-supported **mutations** must be separately adopted as Dockpilot operations before appearing in UI.
