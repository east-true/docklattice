# Container installation notes

These notes define the filesystem and privilege boundary for the v1 Images.
Release automation publishes two multi-architecture Images:

- `ghcr.io/east-true/dockpilot-server`;
- `ghcr.io/east-true/dockpilot-agent`.

Both support `linux/amd64` and `linux/arm64`. Installation always uses the
immutable digest from the selected GitHub Release, never a mutable tag copied
by hand.

## Select and verify a release

Install Docker, `gh`, `jq`, and Cosign on the operator workstation. Set the
release tag explicitly, then download and checksum every attached evidence
asset. `v0.1.0-rc.1` is the planned first Image-bearing release; this command
becomes usable after that release has been published.

```sh
release_tag=v0.1.0-rc.1
release_dir=$(mktemp -d)

gh release download "$release_tag" \
  --repo east-true/dockpilot \
  --dir "$release_dir"

(
  cd "$release_dir"
  sha256sum --check SHA256SUMS
)

server_image=$(jq -er '.images.server.reference' \
  "$release_dir/release-images.json")
agent_image=$(jq -er '.images.agent.reference' \
  "$release_dir/release-images.json")
```

Verify both independent supply-chain statements. GitHub attestation proves the
tag workflow and source repository that built the digest. Cosign verifies the
keyless signature issued to that exact tag workflow identity.

```sh
gh attestation verify "oci://$server_image" \
  --repo east-true/dockpilot
gh attestation verify "oci://$agent_image" \
  --repo east-true/dockpilot

certificate_identity="https://github.com/east-true/dockpilot/.github/workflows/release.yml@refs/tags/$release_tag"
certificate_issuer="https://token.actions.githubusercontent.com"

cosign verify \
  --certificate-identity "$certificate_identity" \
  --certificate-oidc-issuer "$certificate_issuer" \
  "$server_image"

cosign verify \
  --certificate-identity "$certificate_identity" \
  --certificate-oidc-issuer "$certificate_issuer" \
  "$agent_image"

docker pull "$server_image"
docker pull "$agent_image"
```

Keep `release-images.json`, `SHA256SUMS`, the four architecture-specific
CycloneDX SBOMs, the four complete Trivy JSON reports, and the Go dependency
license archive with the deployment record. The exact `trivyignore.yaml` used
by the gate is attached and checksummed alongside them. The release workflow
fails before publication when a fixable HIGH or CRITICAL vulnerability is
present; unfixed findings remain visible in the attached reports. A small
number of path-scoped, time-limited exceptions for the newest upstream Docker
CLI and Compose binaries are documented in the distribution record and remain
visible in those same reports.

## Server

Mount durable state at `/var/lib/dockpilot` and place the TLS certificate and
private key at `/var/lib/dockpilot/tls/server.crt` and `server.key`. The image
runs as UID/GID 65532, so a bind-mounted state directory and its files must be
owned by that identity and must not be accessible to other users. For example,
prepare `/srv/dockpilot/server-state` as root, copy the certificate and key into
its `tls` directory, set both ownerships to `65532:65532`, the directories to
mode 0700, and the files to mode 0600.

The default container command listens on all *container* interfaces on ports
8080 (HTTPS UI/registration) and 8443 (Agent transport). Docker publishes an
unqualified `-p` mapping on every host interface, so constrain the host side
explicitly. Port 8080 has no browser authentication and must remain loopback;
8443 belongs only on the private/VPN interface used by Agents.

```sh
server_private_ip=10.0.0.10

docker run -d --name dockpilot-server --restart unless-stopped \
  -p 127.0.0.1:8080:8080 -p "$server_private_ip:8443:8443" \
  -v /srv/dockpilot/server-state:/var/lib/dockpilot:rw \
  "$server_image"
```

Remote registration also uses port 8080. Put a TLS reverse proxy on the Server
host and expose only `/api/v1/agent/` without browser authentication; require
authentication for every other path and proxy them to `127.0.0.1:8080`. Point
the Agent's `--registration-url` at that proxy. Do not publish the whole Server
port with `-p 8080:8080` merely to make registration reachable.

## Agent

The Agent is non-root, but access to `/var/run/docker.sock` grants control of
the host Docker daemon. Add the socket's numeric host group as a supplementary
group instead of running the container as root:

The Agent state root is a security boundary. A bind-mounted host directory must
be owned by UID/GID 65532 and have mode 0700 before the container starts. A
named volume is initialized to those ownership and mode values by the image.

Issue a short-lived, one-time Join Token from the Server state before starting
a new Agent. The command writes only the token to stdout, so create the secret
with a restrictive umask and make it readable by the Agent container identity:

```sh
umask 077
join_token_file=$(mktemp)
trap 'rm -f "$join_token_file"' EXIT HUP INT TERM

docker run --rm \
  -v /srv/dockpilot/server-state:/var/lib/dockpilot:rw \
  "$server_image" server issue-token \
  --state-dir /var/lib/dockpilot \
  --ttl 15m \
  > "$join_token_file"

sudo install -d -m 0700 -o 65532 -g 65532 /etc/dockpilot
sudo install -m 0600 -o 65532 -g 65532 \
  "$join_token_file" \
  /etc/dockpilot/join-token
rm "$join_token_file"
trap - EXIT HUP INT TERM
```

For an expired existing Agent credential, issue a purpose-bound token with
`--rejoin-agent-id <stable-agent-uuid>` instead of a general token.

```sh
docker_socket_gid=$(stat -c '%g' /var/run/docker.sock)
docker run -d --name dockpilot-agent \
  --user 65532:65532 --group-add "$docker_socket_gid" \
  --label io.dockpilot.role=agent \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v dockpilot-agent-state:/var/lib/dockpilot \
  -v /etc/dockpilot/server-ca.crt:/var/lib/dockpilot/server-ca.crt:ro \
  -v /etc/dockpilot/join-token:/run/secrets/dockpilot-join-token:ro \
  -v /srv/stacks:/srv/stacks:ro \
  "$agent_image" agent \
  --server dockpilot.internal:8443 \
  --registration-url https://dockpilot.internal \
  --server-ca /var/lib/dockpilot/server-ca.crt \
  --join-token-file /run/secrets/dockpilot-join-token \
  --project-root /srv/stacks
```

The Join Token file must be a regular owner-readable file with mode 0600 and
must be readable as UID 65532 inside the container. Wait until the UI reports
the Agent as Active. Then replace the bootstrap container with its steady-state
form before deleting the token. The state volume retains the issued Agent
credential; the deleted host file is no longer a bind-mount dependency.

```sh
docker stop dockpilot-agent
docker rm dockpilot-agent
sudo rm /etc/dockpilot/join-token

docker run -d --name dockpilot-agent --restart unless-stopped \
  --user 65532:65532 --group-add "$docker_socket_gid" \
  --label io.dockpilot.role=agent \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v dockpilot-agent-state:/var/lib/dockpilot \
  -v /etc/dockpilot/server-ca.crt:/var/lib/dockpilot/server-ca.crt:ro \
  -v /srv/stacks:/srv/stacks:ro \
  "$agent_image" agent \
  --server dockpilot.internal:8443 \
  --registration-url https://dockpilot.internal \
  --server-ca /var/lib/dockpilot/server-ca.crt \
  --project-root /srv/stacks
```

Confirm that the same Agent ID returns Active after this recreation. Do not
leave the consumed token file, its bind mount, or `--join-token-file` in the
steady-state container configuration. Dockpilot never deletes the host secret
itself; removing it remains an operator action.

Losing the state volume is the case that does need a token again. Issue a new
one, purpose-bound with `--rejoin-agent-id` if the Agent had registered before,
rather than expecting the consumed one to still work. Each discovery root must
be an identical absolute-path bind mount: `/srv/stacks:/srv/stacks`, never a
remapped path. Use `:ro` for the first-class read-only mode; use `:rw` only when
file editing and backup restore are intended, and grant UID 65532 the
corresponding host filesystem access.

Run the Agent separately from managed Compose projects. Do not use rootless
Docker's nonstandard socket, `DOCKER_HOST`, or a socket proxy for v1.

## Host-driven Agent upgrade

Self-protection prevents Dockpilot from replacing its own container. Upgrade
from the host. First run the release selection and verification procedure above
for the new release and record the currently running digest:

```sh
previous_agent_image=$(docker inspect dockpilot-agent \
  --format '{{.Config.Image}}')

docker stop dockpilot-agent
docker rm dockpilot-agent
```

Recreate it with `$agent_image` and the same complete steady-state `docker run`
contract shown above: state volume, Docker socket group, CA path, discovery
roots, labels, Server addresses, and arguments. Do not restore the bootstrap
token bind or `--join-token-file`. Do not delete or replace
`dockpilot-agent-state`. Confirm that the existing Agent ID returns Active
before removing the old digest from the host.

To roll back an Agent, verify that the target release notes permit downgrade,
then repeat the host-driven recreation with `$previous_agent_image`. If a
release changes Agent state incompatibly, restore the offline state snapshot
required by those release notes instead of attempting an in-place downgrade.

## Server upgrade and rollback

Record the old digest and take an offline backup of
`/srv/dockpilot/server-state` before changing the Server. The backup must
include the database, Identity State, and TLS material together.

```sh
previous_server_image=$(docker inspect dockpilot-server \
  --format '{{.Config.Image}}')

docker stop dockpilot-server
sudo tar -C /srv/dockpilot \
  -czf "/srv/dockpilot/server-state-before-$release_tag.tar.gz" \
  server-state
docker rm dockpilot-server
```

Recreate the Server with `$server_image`, the same state path, and the same
published interfaces. Confirm the original Server identity and Agents before
deleting the offline backup. For rollback, stop and remove the new Server,
restore the matching offline state archive, and recreate it with
`$previous_server_image`. Never pair an older database with a newer Identity
State, or the reverse; the recovery matrix deliberately fails closed on that
split-brain shape.

The release-gate version of this procedure, including exact image-ID and
identity-reconnect assertions, is documented in
[`../release/clean-host-install-e2e.md`](../release/clean-host-install-e2e.md). Its checked-in status
is `PASS`, backed by the recorded execution documented there.
