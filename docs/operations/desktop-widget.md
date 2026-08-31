# Desktop widget

The DockLattice desktop widget is an additional, compact interface to a remote
DockLattice Server. It does not replace the embedded browser UI.

The widget is currently a development preview. It is built and tested from
source, but no signed Windows, macOS, or Linux installer has been released.
Its compact interface uses a light theme by default so status and action groups
remain legible in a small always-available window.

## When to use each interface

| Interface | Use it for |
|---|---|
| Desktop widget | Quick project status and the frequent Compose actions Pull, Up, Down, Start, and Stop |
| Browser UI | Fleet navigation, object Inspectors, Files, Logs, Metrics, Audit, backups, diagnostics, and every detailed workflow |

Both interfaces read the same Server API and submit the same managed
Operations. The Agent remains the execution authority, so closing either view
does not cancel an accepted Operation.

The widget never connects to a Docker socket, starts a shell, or runs Docker
CLI commands locally. Its Rust bridge permits only the dashboard, project
runtime, Operation-status, and Operation-start endpoints. Operation-start is
limited to these five kinds:

- `compose.pull`
- `compose.up`
- `compose.down`
- `compose.start`
- `compose.stop`

There is no Restart, arbitrary command, filesystem, log, metrics, Audit, or
Docker object API in the widget. Those workflows remain available in the
browser UI where applicable.

## Connection model

On first run, enter the remote Server's HTTPS origin. A URL may contain only
the scheme, host, and optional port. Embedded credentials, paths, queries,
fragments, redirects, and plain HTTP are rejected.

If the Server certificate is not rooted in the operating system trust store,
paste its CA certificate or PEM bundle. The widget does not provide a
"trust invalid certificate" switch. The Server origin and CA PEM are local
device preferences; the CA is public trust material, not a private key.

DockLattice v1 still has no built-in user authentication or RBAC. Do not make
the Server generally reachable merely to use the widget. Follow the
[installation security boundary](install.md#server) and
[security policy](../../SECURITY.md).

## Interaction contract

- Pull, Up, and Down require explicit confirmation.
- Pull never falls back to an Image build.
- Up always follows DockLattice's `--no-build` policy.
- A project whose effective model requires a build has Up disabled with the
  Server-provided reason.
- Service rows expose Start and Stop only when an existing non-one-off
  Container has been observed.
- Refresh replaces data in place and defaults to 15 seconds. Manual, 30-second,
  and 60-second intervals are also available.
- Closing the window hides it to the system tray on Windows, macOS, and Linux.
  The tray menu opens the widget or quits the process explicitly. A Linux
  desktop must expose status icons or AppIndicator-compatible tray items to
  reopen the hidden window from the panel.
- A small badge on the application tray icon reports connection state without
  opening the window: gray is not configured, amber is connecting, green is
  connected, and red is a connection error. The tray menu repeats the state in
  text for accessibility and for Linux desktops where tray tooltips are
  unavailable.

## Build and test from source

The widget uses Tauri 2 with a static HTML/CSS/JavaScript frontend and a small
Rust HTTPS bridge. The existing Go Server and Agent are unchanged.

```sh
cd desktop
npm ci
npm test
npm run test:ui
npm run tauri -- build --debug --no-bundle
```

Tauri's operating-system build prerequisites are still required. CI compiles
the widget on Windows, macOS, and Linux; the compact UI interaction suite runs
on Linux Chromium as a deterministic frontend test.
