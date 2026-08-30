# Internal-network trial installation

This runbook installs one DockLattice Server and one or more remote Agents on a
trusted internal network when the target hosts cannot pull Images from an
external registry. It uses `docker save`, `scp`, and `docker load` to move an
already-present Agent Image from the Server host.

This is a trial-installation path, not a production exposure pattern. DockLattice
v1 has no browser authentication, user accounts, or RBAC. Every person who can
reach the published Server UI can control every connected Docker host. Restrict
the UI port to an explicit trusted source range with the host or network
firewall. For a durable production installation, use an authenticating reverse
proxy and the release verification procedure in the main [installation
guide](install.md).

## Deployment shape

The examples below use these values:

| Item                            | Example value                   |
| ------------------------------- | ------------------------------- |
| Server internal address         | `10.20.30.40`                   |
| Server UI and registration port | `18081`                         |
| Agent transport port            | `18443`                         |
| Server Container                | `docklattice-server-local`        |
| Server state Volume             | `docklattice-server-local-state`  |
| Server Image                    | `docklattice/server:validation-local` |
| Agent Image                     | `docklattice/agent:validation-local`  |
| Agent discovery root            | `/srv/stacks`                   |

Replace `10.20.30.40`, the SSH destination, and the discovery root with values
for the target environment. The `validation-local` Images are local validation
artifacts, not signed release Images. Server and Agent must use matching
revisions. Use the exact digest from a verified release when one is available.

The Server accepts inbound TCP on the UI/registration and Agent transport
ports. Agents make outbound connections to both Server ports; an Agent does not
need an inbound port.

## 1. Check the Server host

The Server host needs Docker and OpenSSL. Confirm that the matching Images are
already present and that the selected host ports are unused:

```sh
docker version
openssl version
docker image inspect docklattice/server:validation-local >/dev/null
docker image inspect docklattice/agent:validation-local >/dev/null
docker ps --format '{{.Names}} {{.Ports}}'
```

The local validation Images shown above are `linux/amd64`. Use matching
multi-architecture release Images instead when an Agent host is `arm64`.

## 2. Create the Server certificate and start the Server

Run this entire block in one shell. Change `server_ip` before running it. The
temporary private key is deleted after it has been copied into the named
Volume; the public certificate is retained at
`/tmp/docklattice-server-local-ca.crt` for Agent installation.

```sh
set -eu

server_ip=10.20.30.40
server_ui_port=18081
server_agent_port=18443
server_image=docklattice/server:validation-local
server_name=docklattice-server-local
server_volume=docklattice-server-local-state

tls_stage=$(mktemp -d /tmp/docklattice-server-tls.XXXXXX)
trap 'rm -rf "$tls_stage"' EXIT HUP INT TERM

openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 365 \
  -subj "/CN=$server_ip" \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1,IP:$server_ip" \
  -keyout "$tls_stage/server.key" \
  -out "$tls_stage/server.crt"
chmod 0600 "$tls_stage/server.key" "$tls_stage/server.crt"

docker volume create "$server_volume"

docker run --pull never --rm --user 0 --entrypoint /bin/sh \
  -v "$server_volume:/var/lib/docklattice" \
  -v "$tls_stage:/tls-source:ro" \
  "$server_image" -c \
  'mkdir -p /var/lib/docklattice/tls
   cp /tls-source/server.crt /var/lib/docklattice/tls/server.crt
   cp /tls-source/server.key /var/lib/docklattice/tls/server.key
   chown -R 65532:65532 /var/lib/docklattice
   chmod 0700 /var/lib/docklattice /var/lib/docklattice/tls
   chmod 0600 /var/lib/docklattice/tls/server.crt /var/lib/docklattice/tls/server.key'

docker run --pull never -d \
  --name "$server_name" \
  --restart unless-stopped \
  -p "0.0.0.0:$server_ui_port:8080" \
  -p "0.0.0.0:$server_agent_port:8443" \
  -v "$server_volume:/var/lib/docklattice:rw" \
  "$server_image"

docker cp \
  "$server_name:/var/lib/docklattice/tls/server.crt" \
  /tmp/docklattice-server-local-ca.crt

server_ready=0
attempt=0
while [ "$attempt" -lt 30 ]; do
  if curl --noproxy '*' --fail --silent --show-error \
    --cacert /tmp/docklattice-server-local-ca.crt \
    "https://$server_ip:$server_ui_port/api/v1/dashboard"; then
    server_ready=1
    break
  fi
  attempt=$((attempt + 1))
  sleep 1
done
test "$server_ready" -eq 1

rm -rf "$tls_stage"
trap - EXIT HUP INT TERM
```

A new Server returns a dashboard document with no hosts. Confirm the Container
and publication addresses:

```sh
docker ps --filter name=docklattice-server-local
docker logs --tail 100 docklattice-server-local
```

The log warning about an explicitly enabled public bind is expected for this
runbook. It is also a reminder that the deployment boundary must restrict
access.

## 3. Check each Agent host

Run these commands on the remote Agent host:

```sh
uname -m
docker version
test -S /var/run/docker.sock
test -r /sys/fs/cgroup/cgroup.controllers
```

The supported boundary is Linux `amd64` or `arm64`, cgroup v2, Docker Engine
API 1.40 or newer, and the standard local socket at
`/var/run/docker.sock`. Rootless Docker, Docker Desktop, `DOCKER_HOST`, remote
daemons, and socket proxies are not supported. See [supported
environments](supported-environments.md) for the complete boundary.

## 4. Transfer and load the Agent Image

On the Server host, set the SSH destination and export the already-present
Agent Image:

```sh
agent_ssh='operator@10.20.30.51'

docker save \
  --output /tmp/docklattice-agent-validation-local.tar \
  docklattice/agent:validation-local

sha256sum /tmp/docklattice-agent-validation-local.tar \
  > /tmp/docklattice-agent-validation-local.tar.sha256

scp \
  /tmp/docklattice-agent-validation-local.tar \
  /tmp/docklattice-agent-validation-local.tar.sha256 \
  /tmp/docklattice-server-local-ca.crt \
  "$agent_ssh:/tmp/"
```

On the Agent host, verify and load the Image:

```sh
sha256sum --check /tmp/docklattice-agent-validation-local.tar.sha256
docker load --input /tmp/docklattice-agent-validation-local.tar

docker image inspect docklattice/agent:validation-local \
  --format 'arch={{.Architecture}} version={{index .Config.Labels "org.opencontainers.image.version"}} revision={{index .Config.Labels "org.opencontainers.image.revision"}}'
```

Install the Server certificate for the Agent's unprivileged UID:

```sh
sudo install -d -m 0700 -o 65532 -g 65532 /etc/docklattice
sudo install -m 0400 -o 65532 -g 65532 \
  /tmp/docklattice-server-local-ca.crt \
  /etc/docklattice/server-ca.crt
```

Validate both Server ports from the Agent host. `sudo` is intentional: the
installed certificate is readable only by UID 65532 and root.

```sh
sudo curl --noproxy '*' --fail \
  --cacert /etc/docklattice/server-ca.crt \
  https://10.20.30.40:18081/api/v1/dashboard

sudo openssl s_client \
  -connect 10.20.30.40:18443 \
  -CAfile /etc/docklattice/server-ca.crt \
  </dev/null
```

If either connection fails, allow outbound traffic from the Agent and inbound
TCP `18081` and `18443` from the Agent's source range at the Server boundary.

## 5. Issue and transfer a one-time Join Token

Do this only after the Image, certificate, and network checks pass. A general
Join Token is single-use and expires after 15 minutes.

Run this entire block on the Server host. Keeping token creation, use, and
cleanup in one block prevents the output filename variable from being lost
between shell sessions.

```sh
set -eu

agent_ssh='operator@10.20.30.51'
join_token_file=$(mktemp /tmp/docklattice-join-token.XXXXXX)
trap 'rm -f "$join_token_file"' EXIT HUP INT TERM
chmod 0600 "$join_token_file"

docker run --pull never --rm \
  -v docklattice-server-local-state:/var/lib/docklattice:rw \
  docklattice/server:validation-local \
  server issue-token \
  --state-dir /var/lib/docklattice \
  --ttl 15m \
  > "$join_token_file"

test -s "$join_token_file"
scp "$join_token_file" "$agent_ssh:/tmp/docklattice-join-token"

rm -f "$join_token_file"
trap - EXIT HUP INT TERM
```

Never print the token or put its value directly in a command argument.

On the Agent host, install it for UID 65532 and remove the transferred copy:

```sh
sudo install -m 0600 -o 65532 -g 65532 \
  /tmp/docklattice-join-token \
  /etc/docklattice/join-token
rm -f /tmp/docklattice-join-token
```

## 6. Bootstrap the Agent

The example discovers Compose projects beneath `/srv/stacks` in supported
read-only mode. Replace that path everywhere in the block if the host uses a
different absolute discovery root. The host path and Container path must be
identical.

Run on the Agent host:

```sh
set -eu

agent_image=docklattice/agent:validation-local
project_root=/srv/stacks
docker_socket_gid=$(stat -c '%g' /var/run/docker.sock)
agent_display_name=$(hostname -s)

sudo mkdir -p "$project_root"

docker run --pull never -d \
  --name docklattice-agent \
  --user 65532:65532 \
  --group-add "$docker_socket_gid" \
  --label io.docklattice.role=agent \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v docklattice-agent-state:/var/lib/docklattice \
  -v /etc/docklattice/server-ca.crt:/var/lib/docklattice/server-ca.crt:ro \
  -v /etc/docklattice/join-token:/run/secrets/docklattice-join-token:ro \
  -v "$project_root:$project_root:ro" \
  "$agent_image" agent \
  --server 10.20.30.40:18443 \
  --registration-url https://10.20.30.40:18081 \
  --server-ca /var/lib/docklattice/server-ca.crt \
  --join-token-file /run/secrets/docklattice-join-token \
  --display-name "$agent_display_name" \
  --self-container-name docklattice-agent \
  --project-root "$project_root"

docker logs --tail 100 docklattice-agent
```

Open `https://10.20.30.40:18081` and wait until the new host reports `Active`.
Do not proceed merely because the Container is running; successful registration
and an Active host are the required boundary.

## 7. Remove the consumed token

After the UI reports the Agent as Active, replace the bootstrap Container with
its steady-state form. Preserve the `docklattice-agent-state` Volume: it contains
the stable Agent ID and runtime credential.

```sh
docker stop docklattice-agent
docker rm docklattice-agent
sudo rm -f /etc/docklattice/join-token
```

Recreate the Agent without the token mount or `--join-token-file`:

```sh
set -eu

agent_image=docklattice/agent:validation-local
project_root=/srv/stacks
docker_socket_gid=$(stat -c '%g' /var/run/docker.sock)
agent_display_name=$(hostname -s)

docker run --pull never -d \
  --name docklattice-agent \
  --restart unless-stopped \
  --user 65532:65532 \
  --group-add "$docker_socket_gid" \
  --label io.docklattice.role=agent \
  -v /var/run/docker.sock:/var/run/docker.sock:rw \
  -v docklattice-agent-state:/var/lib/docklattice \
  -v /etc/docklattice/server-ca.crt:/var/lib/docklattice/server-ca.crt:ro \
  -v "$project_root:$project_root:ro" \
  "$agent_image" agent \
  --server 10.20.30.40:18443 \
  --registration-url https://10.20.30.40:18081 \
  --server-ca /var/lib/docklattice/server-ca.crt \
  --display-name "$agent_display_name" \
  --self-container-name docklattice-agent \
  --project-root "$project_root"

docker restart docklattice-agent
docker logs --tail 100 docklattice-agent
```

Confirm that the same host identity returns to `Active`. Do not delete
`docklattice-agent-state`; losing it requires enrollment again. For an existing
Agent whose credential expired while it was offline, follow the identity
[recovery procedure](recovery.md) and issue a token bound with
`--rejoin-agent-id` instead of creating a duplicate host.

## Troubleshooting the trial path

### `curl: (77) error setting certificate file`

The installed CA file is intentionally mode `0400` and owned by UID/GID 65532,
so the login user cannot read it. Keep that permission and run the validation
with `sudo`:

```sh
sudo stat -c '%u:%g %a %n' /etc/docklattice/server-ca.crt
sudo curl --noproxy '*' --fail \
  --cacert /etc/docklattice/server-ca.crt \
  https://10.20.30.40:18081/api/v1/dashboard
```

The expected ownership and mode are `65532:65532 400`.

### `-bash: : No such file or directory` during token issue

The redirection target variable was empty, usually because token creation and
the `docker run` command were executed in different shells. Run the complete
token block in section 5, or use an explicit protected path:

```sh
umask 077
docker run --pull never --rm \
  -v docklattice-server-local-state:/var/lib/docklattice:rw \
  docklattice/server:validation-local \
  server issue-token --state-dir /var/lib/docklattice --ttl 15m \
  > /tmp/docklattice-join-token
test -s /tmp/docklattice-join-token
```

### Agent Container exits or remains offline

Inspect its bounded structured diagnostics:

```sh
docker ps -a --filter name=docklattice-agent
docker logs --tail 200 docklattice-agent
```

Typical causes are an expired token, an unreadable CA or token file, blocked
Server ports, a certificate without the Server IP in its SAN, the wrong Docker
socket GID, an unsupported Docker environment, or mismatched Server and Agent
Image revisions. Continue with the [Agent diagnostics](agent-diagnostics.md)
when the event records identify a specific failure.

## Cleanup

The transferred Image archive and public CA copy are not runtime dependencies
after the Image and CA have been installed on an Agent. Remove the transfer
artifacts from the Server host:

```sh
rm -f \
  /tmp/docklattice-agent-validation-local.tar \
  /tmp/docklattice-agent-validation-local.tar.sha256 \
  /tmp/docklattice-server-local-ca.crt
```

This command does not remove the loaded Image, Agent state, or installed CA.
On the Agent host, the copies under `/tmp` may be removed as well, but keep
`/etc/docklattice/server-ca.crt`. Do not remove `docklattice-agent-state` as part of
routine cleanup.
