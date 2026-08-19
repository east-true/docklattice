# Dockpilot documentation

Start here. Each document below has one audience and one job.

## New to Dockpilot

| Document | What it answers |
|---|---|
| [Concepts](concepts.md) | What a Server, an Agent, a Project, an Operation, and the Audit archive are, and how they relate. **Read this first.** |
| [Architecture decision record](architecture.md) | Why every one of those things is the way it is. The authority for all behaviour. Written in Korean. |

The architecture record is authoritative: if any other document — including this
one — disagrees with it, the architecture record is right and the other document
is a bug.

## Operating Dockpilot

| Document | What it answers |
|---|---|
| [Supported environments](operations/supported-environments.md) | Whether your host is inside the tested boundary, and what is explicitly refused. |
| [Installation](operations/install.md) | How to prepare state directories, issue a Join Token, and run both containers with the correct privilege boundary. |
| [Configuration](operations/configuration.md) | Every command-line flag, and every operational default with the value it is compiled with. |
| [HTTP API](operations/api.md) | The Server's read and write endpoints, their error shape, and their secret-handling rules. |
| [Identity recovery](operations/recovery.md) | What to do after losing Server identity state, the Server database, or an Agent state directory. |
| [Degraded storage](operations/degraded-storage.md) | What `DEGRADED_STORAGE` means, what Dockpilot evicts on its own, and how to leave the state. |

## Verifying and contributing

| Document | What it answers |
|---|---|
| [Release evidence](release/README.md) | Which gates ran, against which images, and what each one actually proves. |
| [Implementation plan](implementation-plan.md) | The phase-by-phase v1 plan and its completion rule. |
| [Contributing](../CONTRIBUTING.md) | How to build, test, and propose a change that the architecture record permits. |
| [Security policy](../SECURITY.md) | The v1 threat model and how to report a vulnerability. |

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
