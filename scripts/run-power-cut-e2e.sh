#!/bin/sh
set -eu

# Power-cut gate. The hardening matrix can kill a process; only a hypervisor
# can take the power away. This drives a disposable lab VM through:
#
#   setup in the guest   Server, Agent, fixture project, and work the API
#                        reported as durable
#   virsh destroy        power removed, no shutdown, no flush
#   virsh start          the guest boots into whatever actually reached disk
#   verify in the guest  every promise made before the cut is re-checked
#
# The physical host's Docker is never touched: everything the gate creates
# lives inside the guest, and the only host-side action is libvirt's.
#
#     ./scripts/run-power-cut-e2e.sh EVIDENCE_DIR dp-vm-clean \
#         sha256:<server> sha256:<agent> sha256:<fixture>

fail() {
    printf 'power-cut gate failed: %s\n' "$*" >&2
    [ -z "${status_file:-}" ] || printf 'status=FAIL\nreason=%s\n' "$*" >"$status_file"
    exit 1
}

[ "$#" -eq 5 ] || {
    printf 'usage: %s ABSOLUTE_EVIDENCE_DIR VM_NAME SERVER_IMAGE AGENT_IMAGE FIXTURE_IMAGE\n' "$0" >&2
    exit 2
}

evidence_dir=$1
vm=$2
server_image=$3
agent_image=$4
fixture_image=$5
status_file=

case "$evidence_dir" in /*) ;; *) fail "evidence directory must be absolute" ;; esac
[ ! -e "$evidence_dir" ] || fail "refusing to overwrite evidence directory: $evidence_dir"
case "$vm" in dp-vm-*) ;; *) fail "refusing to act on \"$vm\": lab VMs must be named dp-vm-*" ;; esac
for image in "$server_image" "$agent_image" "$fixture_image"; do
    case "$image" in sha256:*) ;; *) fail "image arguments must be exact sha256 image IDs: $image" ;; esac
done

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
provision=$repo_dir/scripts/vm-lab-provision.sh
guest_script=$repo_dir/scripts/power-cut-guest.sh
[ -x "$provision" ] || fail "vm-lab-provision.sh is not executable"
[ -f "$guest_script" ] || fail "power-cut-guest.sh is missing"

mkdir -p "$evidence_dir"
status_file=$evidence_dir/STATUS
printf 'started_at=%s\nvm=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$vm" >"$evidence_dir/environment.env"

guest_runtime=/home/lab/docklattice-power-cut
key_file=${VM_SSH_KEY:-$HOME/.ssh/docklattice-vm-lab}

guest() {
    "$provision" ssh "$vm" "$@"
}

address=$("$provision" ip "$vm") || fail "$vm has no address"
printf 'vm_address=%s\n' "$address" >>"$evidence_dir/environment.env"

scp -i "$key_file" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR "$guest_script" "lab@$address:/home/lab/power-cut-guest.sh" >/dev/null ||
    fail "could not copy the guest script into $vm"
guest 'chmod +x /home/lab/power-cut-guest.sh' >/dev/null

# A previous run's containers would make the restart assertions meaningless.
# The Server and Agent state directories belong to the image's unprivileged
# user, so removing them is the one thing here that needs the guest's sudo.
guest "/home/lab/power-cut-guest.sh teardown $guest_runtime >/dev/null 2>&1 || true; sudo -n rm -rf $guest_runtime" >/dev/null ||
    fail "could not clear a previous run's guest runtime at $guest_runtime"

printf 'phase=setup\n' >>"$evidence_dir/environment.env"
guest "/home/lab/power-cut-guest.sh setup $guest_runtime $server_image $agent_image $fixture_image" \
    >"$evidence_dir/setup.log" 2>&1 || {
    cp "$evidence_dir/setup.log" "$evidence_dir/setup.failed.log" 2>/dev/null || true
    fail "guest setup failed: $(tail -1 "$evidence_dir/setup.log" 2>/dev/null)"
}

# The cut. libvirt calls it destroy; it removes power and nothing else.
#
# The delay is jittered so the cut does not always land at the same point of
# the writer's loop. A gate that always cuts between iterations would only ever
# test the easy case.
jitter=$(( $(od -An -N1 -tu1 </dev/urandom | tr -d ' ') % 9 + 2 ))
printf 'cut_delay_seconds=%s\n' "$jitter" >>"$evidence_dir/environment.env"
sleep "$jitter"
cut_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
"$provision" poweroff "$vm" >"$evidence_dir/poweroff.log" 2>&1 || fail "could not cut power to $vm"
printf 'power_cut_at=%s\n' "$cut_at" >>"$evidence_dir/environment.env"

"$provision" start "$vm" >"$evidence_dir/start.log" 2>&1 || fail "$vm did not start again"
deadline=$(( $(date +%s) + 600 ))
booted=0
while [ "$(date +%s)" -lt "$deadline" ]; do
    if guest 'systemctl is-system-running >/dev/null 2>&1 || true; docker info >/dev/null 2>&1' >/dev/null 2>&1; then
        booted=1
        break
    fi
    sleep 10
done
[ "$booted" -eq 1 ] || fail "$vm never came back with a working Docker after the power cut"
printf 'booted_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$evidence_dir/environment.env"

printf 'phase=verify\n' >>"$evidence_dir/environment.env"
guest "/home/lab/power-cut-guest.sh verify $guest_runtime" >"$evidence_dir/verify.log" 2>&1 || {
    guest "cat $guest_runtime/evidence/power-cut.assertions.env 2>/dev/null" \
        >"$evidence_dir/assertions.env" 2>/dev/null || true
    fail "guest verification failed: $(tail -1 "$evidence_dir/verify.log" 2>/dev/null)"
}

guest "cat $guest_runtime/evidence/power-cut.assertions.env" >"$evidence_dir/assertions.env" ||
    fail "could not collect the guest assertions"
guest "cat $guest_runtime/writer.journal" >"$evidence_dir/writer.journal" 2>/dev/null || true
guest "cd $guest_runtime/evidence && tar cf - ." >"$evidence_dir/guest-evidence.tar" 2>/dev/null ||
    printf 'guest evidence archive unavailable\n' >&2

printf 'finished_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$evidence_dir/environment.env"
printf 'status=PASS\n' >"$status_file"
printf 'power-cut gate: PASS (%s)\n' "$vm"
