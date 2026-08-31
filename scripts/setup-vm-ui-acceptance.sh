#!/bin/sh
set -eu

# Prepare the disposable live-VM environment used by vm-acceptance.spec.mjs.
# This script must run inside a dp-vm-* guest. It only replaces resources with
# the docklattice-acceptance-* prefix plus the dedicated Server and Agent.

usage() {
    printf 'usage: %s SERVER_IMAGE AGENT_IMAGE\n' "$0" >&2
}

fail() {
    printf 'VM UI acceptance setup: %s\n' "$*" >&2
    exit 1
}

[ "$#" -eq 2 ] || {
    usage
    exit 2
}

server_image=$1
agent_image=$2
fixture_image=alpine:3.22
acceptance_root=/opt/docklattice-acceptance
network=docklattice-acceptance-net
server=docklattice-server
agent=docklattice-agent
certificate_days=${DOCKLATTICE_ACCEPTANCE_CERT_DAYS:-30}

case "$certificate_days" in
    ''|*[!0-9]*) fail "DOCKLATTICE_ACCEPTANCE_CERT_DAYS must be an integer" ;;
esac
[ "$certificate_days" -ge 1 ] && [ "$certificate_days" -le 365 ] ||
    fail "DOCKLATTICE_ACCEPTANCE_CERT_DAYS must be between 1 and 365"

case "$(hostname)" in
    dp-vm-*) ;;
    *) fail "refusing to run outside a dp-vm-* guest" ;;
esac

for command_name in docker openssl curl jq sudo; do
    command -v "$command_name" >/dev/null 2>&1 ||
        fail "$command_name is required"
done

docker image inspect "$server_image" >/dev/null 2>&1 ||
    fail "Server Image is unavailable: $server_image"
docker image inspect "$agent_image" >/dev/null 2>&1 ||
    fail "Agent Image is unavailable: $agent_image"

if ! docker image inspect "$fixture_image" >/dev/null 2>&1; then
    docker image inspect alpine:latest >/dev/null 2>&1 ||
        fail "alpine:3.22 or alpine:latest is required"
    docker image tag alpine:latest "$fixture_image"
fi

docker image tag "$server_image" docklattice-ui-vm:server
docker image tag "$agent_image" docklattice-ui-vm:agent

remove_project_containers() {
    project=$1
    docker ps --all --quiet \
        --filter "label=com.docker.compose.project=$project" |
        while IFS= read -r container_id; do
            [ -n "$container_id" ] || continue
            docker rm --force "$container_id" >/dev/null
        done
}

for project in \
    docklattice-acceptance-normal \
    docklattice-acceptance-build-policy \
    docklattice-acceptance-read-only \
    docklattice-acceptance-collision; do
    remove_project_containers "$project"
done

for container_name in "$agent" "$server" docklattice-acceptance-one-off; do
    if docker container inspect "$container_name" >/dev/null 2>&1; then
        docker rm --force "$container_name" >/dev/null
    fi
done

for network_name in \
    docklattice-acceptance-normal_acceptance-net \
    docklattice-acceptance-normal_default \
    "$network"; do
    if docker network inspect "$network_name" >/dev/null 2>&1; then
        docker network rm "$network_name" >/dev/null
    fi
done

for volume_name in \
    docklattice-acceptance-normal_acceptance-data \
    docklattice-agent-state \
    docklattice-server-state; do
    if docker volume inspect "$volume_name" >/dev/null 2>&1; then
        docker volume rm "$volume_name" >/dev/null
    fi
done

sudo rm -rf "$acceptance_root"
sudo install -d -m 0755 \
    "$acceptance_root/bootstrap" \
    "$acceptance_root/fixtures/stacks/normal" \
    "$acceptance_root/fixtures/stacks/build-policy" \
    "$acceptance_root/fixtures/stacks/collision-a" \
    "$acceptance_root/fixtures/stacks-ro/read-only" \
    "$acceptance_root/fixtures/stacks-ro/collision-b"

sudo tee "$acceptance_root/fixtures/stacks/normal/.env" >/dev/null <<'EOF'
ACCEPTANCE_IMAGE_TAG=3.22
EOF

sudo tee "$acceptance_root/fixtures/stacks/normal/service.env" >/dev/null <<'EOF'
FIXTURE_SECRET=fixture-secret-value
EOF

sudo tee "$acceptance_root/fixtures/stacks/normal/settings.conf" >/dev/null <<'EOF'
mode=acceptance
EOF

sudo tee "$acceptance_root/fixtures/stacks/normal/compose.yaml" >/dev/null <<'EOF'
name: docklattice-acceptance-normal

services:
  web:
    image: alpine:${ACCEPTANCE_IMAGE_TAG}
    pull_policy: never
    command:
      - /bin/sh
      - -c
      - |
        counter=1
        trap 'exit 0' TERM INT
        while :; do
          echo "acceptance web log $${counter}"
          counter=$$((counter + 1))
          sleep 1
        done
    env_file:
      - service.env
    volumes:
      - acceptance-data:/var/lib/acceptance
      - ./settings.conf:/etc/acceptance/settings.conf:ro
    networks:
      acceptance-net:
        aliases:
          - acceptance-web

  worker:
    image: alpine:${ACCEPTANCE_IMAGE_TAG}
    pull_policy: never
    command:
      - /bin/sh
      - -c
      - trap 'exit 0' TERM INT; while :; do sleep 60; done
    networks:
      - acceptance-net

  nolog:
    image: alpine:${ACCEPTANCE_IMAGE_TAG}
    pull_policy: never
    command:
      - /bin/sh
      - -c
      - trap 'exit 0' TERM INT; while :; do sleep 60; done
    logging:
      driver: none
    networks:
      - acceptance-net

  profiled:
    image: alpine:${ACCEPTANCE_IMAGE_TAG}
    pull_policy: never
    profiles:
      - manual
    command:
      - /bin/sh
      - -c
      - trap 'exit 0' TERM INT; while :; do sleep 60; done
    networks:
      - acceptance-net

volumes:
  acceptance-data:

networks:
  acceptance-net:
    enable_ipv6: true
    ipam:
      config:
        - subnet: 10.51.0.0/24
        - subnet: fd00:51::/64
EOF

sudo tee "$acceptance_root/fixtures/stacks/normal/compose-orphan.yaml" >/dev/null <<'EOF'
name: docklattice-acceptance-normal

services:
  orphan:
    image: alpine:3.22
    pull_policy: never
    command:
      - /bin/sh
      - -c
      - trap 'exit 0' TERM INT; while :; do sleep 60; done
EOF

sudo tee "$acceptance_root/fixtures/stacks/build-policy/Dockerfile" >/dev/null <<'EOF'
FROM alpine:3.22
EOF

sudo tee "$acceptance_root/fixtures/stacks/build-policy/compose.yaml" >/dev/null <<'EOF'
name: docklattice-acceptance-build-policy

services:
  image-only:
    image: alpine:3.22
    pull_policy: never
    command: ["/bin/sh", "-c", "while :; do sleep 60; done"]

  image-and-build:
    image: alpine:3.22
    pull_policy: never
    build:
      context: .
    command: ["/bin/sh", "-c", "while :; do sleep 60; done"]

  build-only:
    build:
      context: .
    command: ["/bin/sh", "-c", "while :; do sleep 60; done"]

  build-policy-required:
    image: docklattice-acceptance-must-build:latest
    pull_policy: build
    build:
      context: .
    command: ["/bin/sh", "-c", "while :; do sleep 60; done"]
EOF

sudo tee "$acceptance_root/fixtures/stacks/collision-a/compose.yaml" >/dev/null <<'EOF'
name: docklattice-acceptance-collision

services:
  first:
    image: alpine:3.22
    pull_policy: never
EOF

sudo tee "$acceptance_root/fixtures/stacks-ro/read-only/compose.yaml" >/dev/null <<'EOF'
name: docklattice-acceptance-read-only

services:
  readonly:
    image: alpine:3.22
    pull_policy: never
EOF

sudo tee "$acceptance_root/fixtures/stacks-ro/collision-b/compose.yaml" >/dev/null <<'EOF'
name: docklattice-acceptance-collision

services:
  second:
    image: alpine:3.22
    pull_policy: never
EOF

sudo chown -R 65532:65532 "$acceptance_root/fixtures"
sudo find "$acceptance_root/fixtures" -type d -exec chmod 0755 {} +
sudo find "$acceptance_root/fixtures" -type f -exec chmod 0644 {} +

subject_alt_name="DNS:server,DNS:$(hostname),IP:127.0.0.1"
for address in $(hostname -I 2>/dev/null); do
    case "$address" in
        *:*) ;;
        *.*) subject_alt_name="$subject_alt_name,IP:$address" ;;
    esac
done

sudo openssl req -x509 -newkey rsa:2048 -sha256 -nodes \
    -days "$certificate_days" \
    -subj "/CN=$(hostname)" \
    -addext "subjectAltName=$subject_alt_name" \
    -keyout "$acceptance_root/bootstrap/server.key" \
    -out "$acceptance_root/bootstrap/server-ca.crt" \
    >/dev/null 2>&1
sudo chmod 0644 "$acceptance_root/bootstrap/server-ca.crt"
sudo chmod 0600 "$acceptance_root/bootstrap/server.key"
certificate_not_after=$(sudo openssl x509 \
    -in "$acceptance_root/bootstrap/server-ca.crt" \
    -noout \
    -enddate)
printf 'Acceptance certificate %s\n' "$certificate_not_after"

docker network create \
    --subnet 198.18.238.0/24 \
    "$network" >/dev/null
docker volume create docklattice-server-state >/dev/null
docker volume create docklattice-agent-state >/dev/null

docker run --rm \
    --user 0:0 \
    --entrypoint /bin/sh \
    --volume docklattice-server-state:/state \
    --volume "$acceptance_root/bootstrap:/bootstrap:ro" \
    docklattice-ui-vm:server \
    -c 'mkdir -p /state/tls; cp /bootstrap/server.key /state/tls/server.key; cp /bootstrap/server-ca.crt /state/tls/server.crt; chown -R 65532:65532 /state; chmod 0700 /state /state/tls; chmod 0600 /state/tls/server.key /state/tls/server.crt'

docker run --detach \
    --name "$server" \
    --hostname "$server" \
    --network "$network" \
    --network-alias server \
    --restart unless-stopped \
    --publish 8080:8080 \
    --volume docklattice-server-state:/var/lib/docklattice \
    docklattice-ui-vm:server \
    server \
    --listen 0.0.0.0:8080 \
    --agent-listen 0.0.0.0:8443 \
    --allow-public-bind >/dev/null

deadline=$(( $(date +%s) + 60 ))
while ! curl --fail --silent --show-error --insecure \
    https://127.0.0.1:8080/api/v1/dashboard >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] ||
        fail "Server did not become ready"
    sleep 1
done

docker run --rm \
    --user 65532:65532 \
    --volume docklattice-server-state:/var/lib/docklattice \
    docklattice-ui-vm:server \
    server issue-token \
    --state-dir /var/lib/docklattice \
    --ttl 30m |
    sudo tee "$acceptance_root/bootstrap/join-token" >/dev/null
sudo chown 65532:65532 "$acceptance_root/bootstrap/join-token"
sudo chmod 0600 "$acceptance_root/bootstrap/join-token"

socket_gid=$(stat -c '%g' /var/run/docker.sock)
docker run --detach \
    --name "$agent" \
    --hostname "$agent" \
    --network "$network" \
    --restart unless-stopped \
    --user 65532:65532 \
    --group-add "$socket_gid" \
    --label io.docklattice.role=agent \
    --volume docklattice-agent-state:/var/lib/docklattice:z \
    --volume "$acceptance_root/bootstrap/server-ca.crt:/var/lib/docklattice/server-ca.crt:ro" \
    --volume "$acceptance_root/bootstrap/join-token:/var/lib/docklattice/join-token:ro" \
    --volume "$acceptance_root/fixtures/stacks:$acceptance_root/fixtures/stacks:rw" \
    --volume "$acceptance_root/fixtures/stacks-ro:$acceptance_root/fixtures/stacks-ro:ro" \
    --volume /var/run/docker.sock:/var/run/docker.sock:rw \
    docklattice-ui-vm:agent \
    agent \
    --server server:8443 \
    --registration-url https://server:8080 \
    --server-ca /var/lib/docklattice/server-ca.crt \
    --join-token-file /var/lib/docklattice/join-token \
    --display-name docklattice-vm-acceptance \
    --self-container-name "$agent" \
    --project-root "$acceptance_root/fixtures/stacks" \
    --project-root "$acceptance_root/fixtures/stacks-ro" >/dev/null

normal_directory="$acceptance_root/fixtures/stacks/normal"
docker run --rm \
    --user 65532:65532 \
    --group-add "$socket_gid" \
    --volume /var/run/docker.sock:/var/run/docker.sock:rw \
    --volume "$normal_directory:$normal_directory:rw" \
    --workdir "$normal_directory" \
    --entrypoint docker \
    docklattice-ui-vm:agent \
    compose \
    --file compose.yaml \
    up --detach --no-build

deadline=$(( $(date +%s) + 90 ))
while :; do
    dashboard=$(curl --fail --silent --show-error --insecure \
        https://127.0.0.1:8080/api/v1/dashboard 2>/dev/null || true)
    if printf '%s' "$dashboard" | jq -e '
        any(.hosts[]?;
            .display_name == "docklattice-vm-acceptance" and
            .state == "ACTIVE" and
            .capabilities.docker.enabled == true and
            .capabilities.compose.enabled == true
        ) and
        any(.projects[]?;
            .name == "docklattice-acceptance-normal" and
            .managed == true
        ) and
        any(.projects[]?;
            .name == "docklattice-acceptance-build-policy" and
            .managed == true
        )
    ' >/dev/null 2>&1; then
        break
    fi
    [ "$(date +%s)" -lt "$deadline" ] ||
        fail "Agent or acceptance projects did not become ready"
    sleep 2
done

printf 'VM UI acceptance environment is ready.\n'
printf 'base_url=https://%s:8080\n' "$(hostname -I | awk '{ print $1 }')"
printf 'docker_socket_gid=%s\n' "$socket_gid"
