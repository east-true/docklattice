# Contributing to Dockpilot

## The one rule that is different here

[`docs/architecture.md`](docs/architecture.md) is the authority, and every
behaviour in Dockpilot is classified in its section 18 as **CORE**, **OPTIONAL**,
**FUTURE**, or **DO NOT BUILD**.

A change that adds a FUTURE or DO NOT BUILD behaviour will not be accepted, and
`scripts/verify-release-scope.sh` will fail it before a human reads it. That
script audits the transitive dependency graph of `./cmd/dockpilot` rather than a
directory list, so the classification cannot be evaded by moving a file.

If you believe a classification is wrong, propose the architecture change first,
as its own discussion. Changing the record is a normal thing to do; changing
behaviour that contradicts the record is not.

The architecture record is written in Korean.
[`docs/concepts.md`](docs/concepts.md) is an English summary of the model, but it
is a summary — where the two differ, the record wins.

## Getting set up

Go, at the version declared in [`go.mod`](go.mod). Nothing else is required to
build or to run the unit tests.

```sh
go build ./cmd/dockpilot
go test ./...
go test -race ./...
```

The protocol definitions under `proto/` are regenerated with
[`buf`](https://buf.build) and `protoc-gen-go`:

```sh
buf generate
```

Commit the regenerated files with the `.proto` change that caused them.

## Before you open a pull request

Run everything that does not need Docker:

```sh
go test ./...
go test -race ./...
for check in scripts/verify-*.sh; do "$check"; done
npm run check:docs
```

The `verify-*.sh` scripts are static contract checks: they confirm the release
scope, the distribution rules the Dockerfile must obey, and the fail-closed
safety boundary of each end-to-end harness. They are fast and they need no
daemon. A pull request that fails one of them is not ready.
`npm run check:docs` verifies repository-local Markdown and image targets
without depending on the availability of external websites.

The Docker-backed matrices under [`docs/release/`](docs/release/README.md) need a
local Linux Docker Engine on cgroup v2. You are not expected to run them for an
ordinary change; a maintainer runs the affected gate before a release.

## What good change looks like here

**Match the surrounding code.** Naming, comment density, and error style are
consistent throughout the repository on purpose. New code should be
indistinguishable from the code next to it.

**Fail closed.** When Dockpilot cannot prove something is safe, it refuses and
says why with a specific reason code — not a generic error, and never a silent
degradation. A new code path that has a "probably fine" branch is not finished.

**Refuse with a reason, not a shrug.** `PROJECT_BUSY`, `PATH_IDENTITY_MISMATCH`,
`DENY_PROTECTED_CONTAINER`, `SAFE_FILE_CONFLICT` — an operator reading a refusal
should learn which invariant stopped them.

**Do not invent information.** If Docker cannot say who did something, the actor
is `unknown`. If a scan was truncated, it is reported as truncated. A confident
answer that was guessed is worse than an honest gap.

**Test the behaviour, not its shape.** A test that passes because it never
exercised the path it names is worse than no test. Two examples from this
repository's own history: a token-replay test that never consumed a token
because the Agent already held a credential, and a harness that waited for a
terminal status while omitting `rejected` from the terminal set. Both passed.
Both measured nothing.

## Reporting bugs

Open an issue with the version or commit, your host kernel and Docker Engine
versions, whether the Agent root is `:ro` or `:rw`, and the exact refusal or
error text. If the issue involves a discovery root, include the bind mount as
written — the identical-path requirement is the single most common cause of
surprising behaviour.

Security issues go through [SECURITY.md](SECURITY.md) instead, privately.

## License

By contributing, you agree that your contributions are licensed under the
Apache License 2.0, as in [LICENSE](LICENSE).
