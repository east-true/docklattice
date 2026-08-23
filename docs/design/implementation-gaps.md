# Dockpilot UI — Implementation Gap Register

**Purpose:** Separate desired UI from current implementation/API capability.  
**Rule:** A gap does not authorize a backend change by itself.

## High-priority gaps to verify against repository

1. Global active/recent Operation listing suitable for the Operation Center.
2. Structured/bounded Operation output suitable for `Output tail`.
3. Authoritative current Agent session source IP exposed to Web UI without persistence.
4. Engine details (`docker info`) required by Host Summary advanced details.
5. Host Container inspect details: Compose labels, health, ports, mounts, networks, restart policy, logging driver, diagnostics.
6. Host Image inspect details and image-to-container reference mapping.
7. Network inspect details including multi-IPAM and connected Containers.
8. Volume inspect/reference mapping and optional disk-usage query.
9. Effective Compose model/source graph information including merge order, profiles, include/extends/env references.
10. Structured Compose runtime model sufficient to distinguish profile-inactive/no-container/one-off/orphan.
11. Audit From/Until query support.
12. Host-scoped multi-Container log viewer if the P1 candidate is adopted.
13. Global live Container search partial-result semantics if adopted.
14. Engine API / Compose version capability exposure.
15. Observed Docker event whitelist review for OOM/restart/pause/unpause.

## Explicitly not implementation gaps

These are product boundaries, not missing features:

- Host OS monitoring
- retained metrics
- metric thresholds/alerting
- arbitrary Docker mutation surface
- authentication/user identity in v1
- generic Settings
- New Project

## Open product decision

### Compose build policy

Must be resolved before final mutation implementation:

- whether `Up` may perform build-capable behavior
- treatment of build-only services
- treatment of mixed image/build projects
- Pull availability wording and flags
