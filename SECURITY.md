# Security policy

## Reporting a vulnerability

Report privately through **GitHub Security Advisories** on this repository
(*Security → Report a vulnerability*). Do not open a public issue for anything
that lets someone reach a Docker host they should not reach.

Please include the version or commit, your host and Docker Engine versions,
and the smallest reproduction you have. If a report is confirmed, the fix and
its advisory will describe the actual boundary that failed rather than a
generic severity label.

## Supported versions

| Version | Supported |
|---|---|
| v1 (`main`) | Yes |
| Anything under `prototype/`, `cmd/transport-prototype`, `internal/candidate`, `internal/contract` | No — disposable Appendix A prototype, not linked into the release binary |

## Threat model

Dockpilot is designed for an **internal network**. Knowing what it does and does
not defend against is more useful than a list of features.

### What v1 does not protect

**There is no browser authentication.** No accounts, no login, no sessions, no
roles. This is a v1 scope decision recorded in architecture section 6.6, not a
missing feature. Anyone who can reach the Server's UI port has full control of
every connected Docker host.

Because of that, the Server binds to `127.0.0.1` by default and refuses a
non-loopback bind without an explicit `--allow-public-bind`. That default is not
a security control — it exists so that exposing the port is a decision someone
makes, rather than something that happens by accident. Put Dockpilot behind an
authenticating reverse proxy, or reach it over a tunnel or VPN.

For the Server image, the loopback boundary is the host-side Docker publish
address, not the process address inside the container. Use
`-p 127.0.0.1:8080:8080`, never an unqualified `-p 8080:8080`. If remote Agents
must register, expose only `/api/v1/agent/` through a TLS reverse proxy and
protect every browser/API path with authentication.

**mTLS, a private CA, and certificate rotation are DO NOT BUILD for v1**
(architecture section 18). Server transport uses a single server certificate the
operator provides.

**An Agent controls its host's Docker daemon.** Access to `/var/run/docker.sock`
is equivalent to root on that host. Dockpilot reduces the blast radius — the
Agent process runs as UID/GID 65532, holds the socket only through a
supplementary group, and contains no shell-command execution wrapper — but it
cannot make socket access safe. Treat every Agent host as being as trusted as
the Server.

### What v1 does protect

These are enforced boundaries with gate evidence behind them, not intentions.
Each is exercised by the [abuse matrix](docs/release/abuse-matrix-e2e.md).

| Boundary | How |
|---|---|
| **Agent enrollment** | One-time, short-lived Join Token, plus a signed 90-day credential. A replayed token is refused with `HTTP 401`. |
| **Server authenticity** | The Agent pins the Server CA. An Agent holding a foreign CA is refused at the TLS handshake, before it ever presents its token. |
| **File access** | `(project_uid, relative_path)` only; absolute paths refused at the API boundary; a name whitelist of Compose and `.env` files; confined to the project's canonical working directory, with TOCTOU-aware path validation. |
| **Secrets** | `.env` values are masked unless `reveal=true` is passed explicitly, are never persisted in Server storage, and never leave the Agent during backup. |
| **Path identity** | A discovery root whose container path differs from its host path fails the Path Identity Self-Check and is demoted to read-only, rather than producing bind mounts that point somewhere unintended. |
| **Self-protection** | The Agent refuses container and Compose operations targeting itself (`DENY_PROTECTED_CONTAINER`, `DENY_PROTECTED_PROJECT`). |
| **Operation identity** | Operation IDs are idempotency keys. Re-using an ID for different work is refused, so an ID cannot be rebound. |
| **Backup integrity** | Restore verifies every file against the manifest's SHA-256 and fails the whole restore on any mismatch, leaving the live project untouched. |
| **Request handling** | Unknown and repeated query parameters, bodies where none are allowed, and oversized payloads are refused rather than ignored. |
| **Error disclosure** | A `500` response carries no detail. The cause is logged Server-side with a bounded, newline-scrubbed record. |

### Reporting scope

In scope: anything that crosses one of the boundaries above, escapes a discovery
root, exposes a secret without `reveal=true`, forges or replays enrollment, or
lets one Agent affect another.

Out of scope: the absence of browser authentication (documented above), anything
reachable only because the UI port was deliberately exposed to an untrusted
network, and anything in the disposable prototype tree.
