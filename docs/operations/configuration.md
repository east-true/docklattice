# Configuration reference

Dockpilot has two configuration surfaces, and they are deliberately different in
kind:

- **Command-line flags** — deployment wiring: addresses, paths, identity. An
  operator sets these.
- **Operational defaults** — timeouts, budgets, retention, intervals. These are
  compiled into the binary from the architecture record. There is no
  configuration file and no environment-variable override in v1.

That second point is intentional. Every default below was frozen by a decision
in [`architecture.md`](../architecture.md) section 19 and validated as a set;
`internal/config.V1Defaults` validates their cross-field relationships at
start-up, so a value cannot be changed in isolation without breaking an
invariant the validator enforces. Their validation status is recorded in
[release/defaults-validation.md](../release/defaults-validation.md).

## `dockpilot server`

```
dockpilot server [options]
```

| Flag | Default | Purpose |
|---|---|---|
| `--listen` | `127.0.0.1:8080` | HTTPS listen address for the UI, API, and Agent registration. |
| `--agent-listen` | `127.0.0.1:8443` | Listen address for the Agent transport. |
| `--allow-public-bind` | off | Required to bind either listener to a non-loopback address. |
| `--state-dir` | `/var/lib/dockpilot` | Durable Server state: identity, database, TLS material. |
| `--tls-cert` | `<state-dir>/tls/server.crt` | TLS certificate PEM. |
| `--tls-key` | `<state-dir>/tls/server.key` | TLS private key PEM. |

`--allow-public-bind` exists because **v1 has no browser authentication**.
Whoever can reach the UI port controls every connected Docker host. The
loopback default is not a security feature — it is a refusal to make exposure
the accident-prone path. See [`../../SECURITY.md`](../../SECURITY.md).

## `dockpilot server issue-token`

```
dockpilot server issue-token [options]
```

| Flag | Default | Purpose |
|---|---|---|
| `--state-dir` | `/var/lib/dockpilot` | Server state directory to issue against. |
| `--ttl` | `15m` | One-time token lifetime. |
| `--rejoin-agent-id` | none | Bind the token to an existing Agent identity instead of creating a new one. |

The command writes only the token to stdout. Use `--rejoin-agent-id` when an
Agent's credential expired while it was offline; a general token would enroll it
as a *new* host and orphan its history.

## `dockpilot agent`

```
dockpilot agent [options]
```

| Flag | Default | Purpose |
|---|---|---|
| `--state-dir` | `/var/lib/dockpilot` | Durable Agent state: Agent ID, credential, WAL, backups. Must be mode 0700 and owned by 65532. |
| `--server` | `127.0.0.1:8443` | Server Agent transport address. |
| `--registration-url` | `https://127.0.0.1:8080` | HTTPS registration base URL. |
| `--server-ca` | none | PEM CA or certificate used to authenticate the Server. |
| `--join-token-file` | none | Mode 0600 file containing the one-time Join Token. Read only when an enrollment is required; a registered Agent reconnects with its stored runtime credential and never opens this path, so the file may be removed after registration while the flag stays on the command line. |
| `--display-name` | none | Name shown for this host in the UI. |
| `--self-container-id` | detected | Explicit Agent container ID, used as a self-protection fallback. |
| `--self-container-name` | detected | Explicit Agent container name, used as a self-protection fallback. |
| `--project-root` | none | Discovery root. Repeatable. Must be an identical absolute-path bind mount. |

The self-protection flags matter more than they look: they are how the Agent
knows which container is *itself*, and therefore which container it must refuse
to stop, remove, or recreate. If detection fails and no fallback is supplied,
Dockpilot could be asked to kill its own Agent.

## Operational defaults

Values are as compiled in `internal/config.V1Defaults()`. Binary units are IEC.

### Operations

| Default | Value |
|---|---|
| `container.*` timeout | 1 min |
| `compose.up` timeout | 15 min |
| `compose.restart` timeout | 10 min |
| `compose.down` timeout | 5 min |
| `compose.pull` timeout | 45 min |
| file write timeout | 30 s |
| `backup.create` / `backup.restore` timeout | 5 min |
| `discovery.rescan` timeout | 10 min |
| cancel grace period (SIGTERM → SIGKILL) | 10 s |
| stalled-operation warning | 5 min |
| retained operation results | 500, or 24 h |
| operation output tail | 64 KiB |
| project lock wait before `PROJECT_BUSY` | 2 s |

Timeout does not have its own termination mechanism — it enters the same
cancellation path as an operator cancel, so a timed-out operation is reported
and audited like any other cancellation.

### Discovery

| Default | Value |
|---|---|
| scan interval | 5 min |
| max directories per scan | 200,000 |
| max scan duration | 1 min |
| directory visit rate | 1,000 /s |

Reaching either the directory or the duration bound returns a partial result
with `truncated=true` and the last scanned path, rather than an incomplete
result presented as a complete one.

### Connection and events

| Default | Value |
|---|---|
| heartbeat interval | 30 s |
| declared offline after | 90 s |
| stats sample interval | 2 s |
| browser sparkline samples | 120 |
| event coalescing window | 5 s |
| observed audit rate cap | 20 /s |

### Agent storage

| Default | Value |
|---|---|
| WAL max size | 256 MiB |
| WAL retention | 14 days |
| WAL fsync trigger | 1 s or 64 KiB |
| Agent state budget | 2 GiB |
| filesystem free floor | `max(1 GiB, 5%)` |
| emergency reserve | 64 MiB |
| automatic snapshots per project | 20 |
| editable file max size | 1 MiB |

WAL retention is whichever of 256 MiB or 14 days is reached first. See
[degraded-storage.md](degraded-storage.md) for what happens at the floor.

### Server storage

| Default | Value |
|---|---|
| operation retention | 90 days |
| audit retention | 365 days |
| audit store max size | 10 GiB |
| audit pressure warning | 80% |
| audit pressure aggressive | 95% |
| audit ACK stall warning | 5 min |

The Server keeps ingesting Agent audit at every pressure level, evicting the
oldest eligible canonical data instead of refusing new records.

### Credentials

| Default | Value |
|---|---|
| credential lifetime | 90 days |
| renewal begins at | 50% of lifetime remaining |

### Memory targets

| Default | Value |
|---|---|
| Agent RSS target | 256 MiB |
| Agent hard limit | 512 MiB |
| Server RSS target | 512 MiB |
| Server hard limit | 1 GiB |

These are budgets, not enforced cgroup limits. The measured peaks under the
production resource gate were 27–31 MiB for both roles; see
[release/resource-gate.md](../release/resource-gate.md).
