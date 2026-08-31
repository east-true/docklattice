#!/bin/sh
set -eu

# Keep libvirt lifecycle evidence across terminal sessions and host reboots.
# A new monitor-start record includes the host boot ID and every domain's
# current state, so an abrupt end followed by a new boot ID is diagnosable even
# when libvirt could not emit a graceful shutdown event.

state_root=${XDG_STATE_HOME:-"$HOME/.local/state"}
log_directory=${DOCKLATTICE_VM_LOG_DIRECTORY:-"$state_root/docklattice"}
log_file=${DOCKLATTICE_VM_LIFECYCLE_LOG:-"$log_directory/vm-lifecycle.log"}

for command_name in flock virsh; do
    command -v "$command_name" >/dev/null 2>&1 || {
        printf 'VM lifecycle monitor: %s is required\n' "$command_name" >&2
        exit 1
    }
done

mkdir -p "$log_directory"
chmod 0700 "$log_directory"
lock_file="$log_directory/vm-lifecycle.lock"
exec 9>"$lock_file"
chmod 0600 "$lock_file"
flock -n 9 || exit 0
touch "$log_file"
chmod 0600 "$log_file"

attempt=0
until virsh uri >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 100 ]; then
        printf '%s event=monitor-failed reason=libvirt-unavailable\n' \
            "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$log_file"
        exit 1
    fi
    sleep 3
done

timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
boot_id=$(sed -n '1p' /proc/sys/kernel/random/boot_id 2>/dev/null || true)
[ -n "$boot_id" ] || boot_id=unavailable

printf '%s boot_id=%s event=monitor-start\n' "$timestamp" "$boot_id" >>"$log_file"

virsh list --all --name |
    while IFS= read -r domain; do
        [ -n "$domain" ] || continue
        state=$(virsh domstate "$domain" --reason 2>&1 | tr '\n' ' ')
        printf '%s boot_id=%s event=initial-state domain=%s state=%s\n' \
            "$timestamp" \
            "$boot_id" \
            "$domain" \
            "$state"
    done >>"$log_file"

exec virsh event \
    --all \
    --loop \
    --timestamp >>"$log_file" 2>&1
