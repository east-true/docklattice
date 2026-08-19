# Container installation notes

These notes define the filesystem and privilege boundary for the v1 images.
They intentionally use placeholders for the final signed image references;
replace them only with release-gate artifacts.

## Server

Mount durable state at `/var/lib/dockpilot` and place the TLS certificate and
private key at `/var/lib/dockpilot/tls/server.crt` and `server.key`. The image
runs as UID/GID 65532, so a bind-mounted state directory and its files must be
owned by that identity and must not be accessible to other users. For example,
prepare `/srv/dockpilot/server-state` as root, copy the certificate and key into
its `tls` directory, set both ownerships to `65532:65532`, the directories to
mode 0700, and the files to mode 0600.

The default container command listens on ports 8080 (HTTPS UI/registration) and
8443 (Agent transport) and explicitly enables a public bind. Publish only the
ports required by the internal deployment boundary; do not expose them directly
to the Internet.

```sh
docker run -d --name dockpilot-server --restart unless-stopped \
  -p 8080:8080 -p 8443:8443 \
  -v /srv/dockpilot/server-state:/var/lib/dockpilot:rw \
  <signed-server-image-reference>
```

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
dockpilot server issue-token \
  --state-dir /srv/dockpilot/server-state --ttl 15m \
  > /etc/dockpilot/join-token
chown 65532:65532 /etc/dockpilot/join-token
chmod 0600 /etc/dockpilot/join-token
```

For an expired existing Agent credential, issue a purpose-bound token with
`--rejoin-agent-id <stable-agent-uuid>` instead of a general token.

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

The Join Token file must be a regular owner-readable file with mode 0600 and
must be readable as UID 65532 inside the container. Remove it after successful
registration. Each discovery root must be an identical absolute-path bind
mount: `/srv/stacks:/srv/stacks`, never a remapped path. Use `:ro` for the
first-class read-only mode; use `:rw` only when file editing and backup restore
are intended, and grant UID 65532 the corresponding host filesystem access.

Run the Agent separately from managed Compose projects. Do not use rootless
Docker's nonstandard socket, `DOCKER_HOST`, or a socket proxy for v1.

## Host-driven Agent upgrade

Self-protection prevents Dockpilot from replacing its own container. Upgrade
from the host: verify the signed image digest, stop and remove only the Agent
container, then recreate it with the same state volume, socket group, identity
paths, discovery-root mounts, and labels. Never delete the Agent state volume
during an image upgrade.

The release-gate version of this procedure, including exact image-ID and
identity-reconnect assertions, is documented in
[`../release/clean-host-install-e2e.md`](../release/clean-host-install-e2e.md). Its checked-in status
is `PASS`, backed by the recorded execution documented there.
