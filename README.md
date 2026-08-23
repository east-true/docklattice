# Dockpilot

**One web interface for every Docker host on your internal network.**

Each host runs an outbound-only Container Agent. A central Server owns
registration, operation history, and the canonical Audit archive. No inbound
port on any host, no agent-side firewall rules, no SSH loop.

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8.svg)](go.mod)
[![Platforms](https://img.shields.io/badge/platforms-linux%2Famd64%20%7C%20linux%2Farm64-lightgrey.svg)](docs/operations/supported-environments.md)
[![Status](https://img.shields.io/badge/v1-complete-brightgreen.svg)](docs/release/README.md)

```
                                    ┌──────────────────┐
   browser ──── HTTPS ────────────▶ │  Dockpilot       │
                                    │  Server          │
                                    │                  │
                                    │  registration    │
                                    │  operations      │
                                    │  audit archive   │
                                    └────────▲─────────┘
                                             │  one reverse gRPC connection,
                    ┌────────────────────────┼────────────────────────┐
                    │                        │                        │
            ┌───────┴───────┐        ┌───────┴───────┐        ┌───────┴───────┐
            │ Agent         │        │ Agent         │        │ Agent         │
            │ docker.sock   │        │ docker.sock   │        │ docker.sock   │
            │ /srv/stacks   │        │ /srv/stacks   │        │ /srv/stacks   │
            └───────────────┘        └───────────────┘        └───────────────┘
              host A                   host B                   host C
                                    (behind NAT)             (dynamic IP)
```

The Agent dials out. That single decision is why hosts behind NAT, behind a
firewall, or on a changing IP need no special handling at all.

## Read this before you deploy

> [!WARNING]
> **Dockpilot v1 has no browser authentication.** There are no user accounts, no
> login, and no sessions — this is a deliberate v1 scope decision, not an
> oversight. Anyone who can reach the Server's UI port has full control of every
> connected Docker host.
>
> The Server therefore binds to `127.0.0.1` by default and requires an explicit
> `--allow-public-bind` to do anything else. Put it behind your own
> authenticating proxy, or reach it through a tunnel. See
> [SECURITY.md](SECURITY.md).
> The Server container listens on all container interfaces so Docker port
> publishing can reach it. That does not make `-p 8080:8080` safe: always bind
> the host side to `127.0.0.1` unless an authenticating proxy is the only
> network path to the port.

## What it does

| | |
|---|---|
| **Fleet view** | Every host, its containers, images, networks, and volumes, from Docker Engine directly — never from a stale Dockpilot cache. |
| **Compose operations** | `up`, `down`, `pull`, `start`, `stop`, `restart` per project, with live stdout and a preserved output tail when things fail. |
| **Container operations** | Start, stop, restart, remove — with the Agent refusing to act on itself. |
| **Safe file editing** | Compose files and `.env`, whitelisted by name, atomic, validated, snapshotted before every write, and guarded against concurrent edits by hash. |
| **Configuration backup and restore** | Per-project archives with a per-file SHA-256 manifest, stored on the Agent so `.env` secrets never cross the network. |
| **Live logs and stats** | Streamed as Server-Sent Events, rate-capped, with dropped bytes reported rather than hidden. |
| **Durable audit** | Everything done through Dockpilot, plus changes made outside it, in one archive with explicit gap accounting. |

And the things it deliberately refuses to do, because they are the reason
tools like this become unreliable:

- It does not replicate Docker's runtime state into its own database.
- It does not store your Compose file contents server-side.
- It does not keep log or metric history — both are live relays.
- It does not reimplement anything Docker or Compose already does.
- It does not offer a lock force-release, because that API's only use is
  corrupting a half-finished `compose up`.
- It does not call cancellation "rollback". Cancelling stops further work and
  says so when effects were partial.

## Quick start

You need a Linux host with a local Docker Engine on cgroup v2. Check
[supported environments](docs/operations/supported-environments.md) first —
Docker Desktop, rootless Docker's nonstandard socket, and remote daemons are
explicitly out of scope.

**1. Prepare Server state and start the Server.**

```sh
sudo install -d -o 65532 -g 65532 -m 0700 /srv/dockpilot/server-state/tls
# place your TLS certificate and key as server.crt / server.key, 0600, owned by 65532

docker run -d --name dockpilot-server --restart unless-stopped \
  -p 127.0.0.1:8080:8080 -p <private-server-ip>:8443:8443 \
  -v /srv/dockpilot/server-state:/var/lib/dockpilot:rw \
  <signed-server-image-reference>
```

Port 8080 serves both the browser UI and Agent registration. For remote Agents,
put a TLS reverse proxy on the same host: expose `/api/v1/agent/` for
registration, require browser authentication for every other path, and proxy
to `127.0.0.1:8080`. Publish 8443 only on a private/VPN interface. Never replace
the loopback mapping above with `-p 8080:8080`.

**2. Issue a one-time Join Token.**

```sh
umask 077
dockpilot server issue-token \
  --state-dir /srv/dockpilot/server-state --ttl 15m \
  > /etc/dockpilot/join-token
sudo chown 65532:65532 /etc/dockpilot/join-token
```

**3. Start an Agent on each Docker host.**

```sh
docker_socket_gid=$(stat -c '%g' /var/run/docker.sock)
docker run -d --name dockpilot-agent --restart unless-stopped \
  --user 65532:65532 --group-add "$docker_socket_gid" \
  --label io.dockpilot.role=agent \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v dockpilot-agent-state:/var/lib/dockpilot \
  -v /etc/dockpilot/server-ca.crt:/var/lib/dockpilot/server-ca.crt:ro \
  -v /etc/dockpilot/join-token:/run/secrets/dockpilot-join-token:ro \
  -v /srv/stacks:/srv/stacks:ro \
  <signed-agent-image-reference> agent \
  --server dockpilot.internal:8443 \
  --registration-url https://dockpilot.internal:8080 \
  --server-ca /var/lib/dockpilot/server-ca.crt \
  --join-token-file /run/secrets/dockpilot-join-token \
  --project-root /srv/stacks
```

Remove the token file once the Agent has registered; it is one-time by design.

> **`-v /srv/stacks:/srv/stacks` is not a typo.** Every discovery root must be an
> **identical** absolute-path bind mount. The Agent hands paths to the Docker
> daemon, which resolves them on the *host* — if the container's view of a path
> differs, every bind mount Dockpilot creates silently points somewhere else. The
> Agent checks this itself at start-up and demotes any root that fails to
> read-only rather than trusting a path it cannot prove.
>
> Use `:ro` for read-only management; that is a first-class mode, not a degraded
> one. Use `:rw` only when you intend to edit files and restore backups.

Full procedure, including the privilege boundary and host-driven Agent upgrades:
[docs/operations/install.md](docs/operations/install.md).

## Documentation

**[Start at the documentation index →](docs/README.md)**

| | |
|---|---|
| [Concepts](docs/concepts.md) | The mental model: Server, Agent, Project, Operation, Audit. Read this first. |
| [Architecture decision record](docs/architecture.md) | Why every one of those is the way it is. The authority for all behaviour. Korean. |
| [Supported environments](docs/operations/supported-environments.md) | Where v1 runs, and what is explicitly refused. |
| [Installation](docs/operations/install.md) | Container preparation, privilege boundary, Agent upgrade. |
| [Configuration](docs/operations/configuration.md) | Every flag and every operational default. |
| [HTTP API](docs/operations/api.md) | Endpoints, error shape, secret handling. |
| [Identity recovery](docs/operations/recovery.md) | Recovering from Server identity, database, or Agent state loss. |
| [Degraded storage](docs/operations/degraded-storage.md) | The `DEGRADED_STORAGE` operator procedure. |
| [Release evidence](docs/release/README.md) | Which gates ran, on what, and what each proves. |

## Status

**v1 is complete.** Every phase of
[the implementation plan](docs/implementation-plan.md) has passed, at revision
`f1d4087`.

Unit, race, and static checks pass. Six gates ran against the release images:
the production cgroup resource matrix over three trials, clean-host container
installation, a real-container recovery matrix covering all three Server-side
loss outcomes, an adversarial failure-injection matrix, an abuse matrix of
inputs the API must refuse, and a reproducible two-architecture release build
whose independent runs produced byte-identical archives.

What is *not* done is recorded just as plainly: only the memory row of
[defaults validation](docs/release/defaults-validation.md) is promoted to
`validated`, and the one-hour and overnight soaks required before signing a
release candidate have not been run. Details and the two known non-blockers are
in [release evidence](docs/release/README.md).

## Development

Requires the Go version declared in [`go.mod`](go.mod).

```sh
go build ./cmd/dockpilot
go test ./...
go test -race ./...
```

The browser workbench has a Playwright acceptance suite at the five required
viewports (1440, 1280, 1024, 768, and 375 pixels). Install the pinned test
dependency and Chromium once, then run the suite or open the interactive UI:

```sh
npm ci
npx playwright install chromium
npm run test:ui
npm run test:ui:open
```

`npm run test:ui:headed` runs the same scenarios in a visible browser and
`npm run test:ui:report` opens the HTML report with the captured Home, Compose
build-policy, and Inspector screenshots. Set `PLAYWRIGHT_TEST_BASE_URL` to test
an already running Dockpilot UI instead of the local asset server. The setup
follows Playwright's official [web server](https://playwright.dev/docs/test-webserver)
and [CI](https://playwright.dev/docs/ci) guidance.

Run `npm run format` after editing the browser code. CI runs
`npm run check:format` so the workbench, Playwright configuration, and UI tests
remain consistently formatted and reviewable.

Contract and safety-boundary checks, none of which need Docker:

```sh
for check in scripts/verify-*.sh; do "$check"; done
```

See [CONTRIBUTING.md](CONTRIBUTING.md) before proposing a change — the
architecture record classifies every behaviour as CORE, OPTIONAL, FUTURE, or
DO NOT BUILD, and a pull request that adds a DO NOT BUILD behaviour will be
rejected by `scripts/verify-release-scope.sh` before a human sees it.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

The embedded production frontend is first-party: `internal/webui/assets` is
three hand-written files with no third-party code and no external references.
Playwright is a development-only acceptance-test dependency and is not linked
into the Dockpilot binary or copied into either release image.

Third-party license and NOTICE texts for the Go binary are generated at release
time from the pinned `go.sum` versions rather than vendored:

```sh
./scripts/generate-license-inventory.sh
```

It covers every module actually linked into `./cmd/dockpilot`, writes a
checksummed `INVENTORY.tsv` under `dist/licenses/`, and fails closed if any
module has no recoverable license text. The Agent image's separately bundled
programs are covered by `distribution/IMAGE-LICENSES.md` and the `/licenses`
tree inside the image.

---

Dockpilot is not affiliated with or endorsed by Docker, Inc. Docker and the
Docker logo are trademarks or registered trademarks of Docker, Inc.
