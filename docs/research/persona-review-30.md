# Dockpilot — 30 Persona Review Summary

**Purpose:** Stress-test the final UI design against technical, Docker-operations, and design/human-factors perspectives.  
**Nature:** Synthetic evidence-based personas; not claims of 30 literal user interviews.

## Technical personas (10)

1. Compose Application Owner — intermediate
2. CLI-first Compose Power User — advanced
3. Multi-host Docker Operator — advanced
4. Log-first Incident Investigator — advanced
5. Senior SRE / On-call Responder — senior
6. Senior Platform Engineer — senior
7. Senior Linux / Docker Infrastructure Engineer — long-tenure senior
8. Docker Network / Storage Troubleshooter — advanced
9. Security / Audit / Forensic Operator — senior
10. Distributed / Edge Docker Operator — advanced

## Docker operator personas (10)

Experience distribution:

- New: 2
- Junior: 2
- Intermediate: 2
- Senior: 2
- Long-tenure: 2

Personas:

1. New Application Operator
2. Read-only Support Operator
3. Compose Deployment Operator
4. Night On-call Developer
5. Compose Estate Operator
6. Multi-host Operations Generalist
7. Production Release Operator
8. Audit / Compliance Operator
9. Linux / Docker Administrator
10. Remote Site / Edge Estate Operator

## Designer personas (10)

Content design:

1. Technical Content Designer
2. Error & Recovery Content Designer
3. Technical Terminology / Localization Designer

Industrial / human factors:

4. Industrial HMI / Situational Awareness Designer
5. Human Factors / Ergonomics Designer
6. Control-room / Maintenance Console Designer

Web / interaction:

7. Data-dense Enterprise Web Designer
8. Accessibility-focused Web Designer
9. Responsive Interaction Designer
10. Design-system / Interaction Consistency Designer

## Consensus findings incorporated into the final contract

- Host-first information architecture survives all persona groups.
- Global Search is required because users may know a project/container but not its host.
- Global Operation Center is required for long-running operations.
- Docker technical truth should be progressively disclosed rather than simplified away.
- New/junior users need short explanatory copy for precise Docker terms rather than replacement terminology.
- Offline, stale, unavailable, empty, and partial results must remain distinct.
- Tables must remain spatially stable during live updates.
- Normal state should be visually quiet; semantic color should emphasize real abnormal states.
- Inspectors should preserve list context and be route-aware/non-modal on desktop.
- Modal dialogs are for short decisions, not object exploration.
- Explicit object links are safer and more accessible than invisible whole-row click behavior.
- Audit needs forensic navigation; Logs need operational navigation; they must remain semantically separate.
- Sensitive Docker/Compose values require explicit reveal.
- Docker inspection richness does not imply every Docker mutation belongs in Dockpilot.

## Design outcome

No persona group required a fundamental IA replacement. The remaining work is primarily implementation-contract alignment, final visual execution, and resolution of the Compose build policy.
