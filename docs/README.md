# DockLattice documentation

Use this page as the stable entry point. Permanent contracts and operator
procedures live here; completed gap reports, review prompts, and handoff notes
are intentionally excluded from the repository.

The [architecture decision record](architecture.md) is authoritative. If
another document disagrees with it, treat that disagreement as a documentation
bug.

## Choose a path

| You want to… | Start here | Then read |
|---|---|---|
| Understand DockLattice | [Concepts](concepts.md) | [Architecture](architecture.md) |
| Install or operate it | [Operations index](operations/README.md) | [Supported environments](operations/supported-environments.md), [Installation](operations/install.md), [Internal-network trial installation](operations/internal-network-install.md) |
| Use or integrate the API | [HTTP API](operations/api.md) | [Interface freeze](interface-freeze.md) |
| Maintain the browser UI | [Product and UI contracts](design/README.md) | [Web UI acceptance](design/web-ui-acceptance.md) |
| Verify a release claim | [Release evidence](release/README.md) | The individual gate record linked from its table |
| Contribute code | [Contributing](../CONTRIBUTING.md) | [Security policy](../SECURITY.md) |

## Document map

### Core contracts

| Document | Role |
|---|---|
| [Concepts](concepts.md) | Short English mental model for Server, Agent, Project, Operation, and Audit. |
| [Architecture](architecture.md) | Authoritative decisions, invariants, scope, and defaults. Written in Korean. |
| [Interface freeze](interface-freeze.md) | Stable identifiers, states, errors, capabilities, audit coverage, credentials, and compatibility rules. |

### Maintainer and operator collections

| Collection | Contents |
|---|---|
| [Operations](operations/README.md) | Installation, configuration, API, supported environments, Agent diagnostics, and recovery procedures. |
| [Product and UI contracts](design/README.md) | Current UI behavior, responsive acceptance, and live metrics design. |
| [Release evidence](release/README.md) | Reproducible gates, environment records, known limits, and the historical v1 plan. |

## Reading the architecture record

`architecture.md` is a decision record, not a manual — it is organized by
decision, not by task. The sections most often needed:

| Section | Subject |
|---|---|
| 2 | Architecture invariants — the decisions that are closed |
| 3 | Deployment model, path identity, self-protection |
| 5 | Server-Agent transport contract and traffic classes |
| 6 | Identity, credentials, and the three-layer archive identity |
| 7 | Discovery and stable project identity |
| 8 | Operation model, project lock, idempotency |
| 9 | Cancellation model |
| 10 | Safe file access |
| 11 | Audit model, WAL, coverage, and ACK rules |
| 13 | Configuration backup and restore |
| 14 | Disk pressure and resource management |
| 18 | Feature classification: CORE, OPTIONAL, FUTURE, DO NOT BUILD |
| 19 | Operational defaults |
