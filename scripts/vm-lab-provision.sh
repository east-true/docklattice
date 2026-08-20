#!/bin/sh
set -eu

# Provisions the disposable VMs the container-based lab cannot stand in for.
#
# The multi-agent lab already gives every Agent its own Docker daemon, its own
# storage, and its own network namespace, which is enough for partitions,
# daemon loss, and per-host pressure. Three things it cannot give, because
# there is only one kernel and one init:
#
#   a real `systemctl restart docker`   the daemon under its actual service
#                                       manager, with the unit's own ordering,
#                                       socket activation, and cgroup
#   a genuinely clean host              a machine where Dockpilot has never
#                                       run and no other Compose project
#                                       exists, which is what the clean-host
#                                       gate is defined for
#   an abrupt power cut                 the guest losing power mid-write, not
#                                       a process being killed
#
# Each VM is Ubuntu 24.04 from the cloud image, brought up by cloud-init with
# Docker installed from Ubuntu's own repository. They are disposable: destroy
# and recreate rather than repair.
#
#     ./scripts/vm-lab-provision.sh create dp-vm-clean
#     ./scripts/vm-lab-provision.sh ip dp-vm-clean
#     ./scripts/vm-lab-provision.sh ssh dp-vm-clean 'docker version'
#     ./scripts/vm-lab-provision.sh poweroff dp-vm-clean   # abrupt, no shutdown
#     ./scripts/vm-lab-provision.sh destroy dp-vm-clean
#
# It never touches the physical host's Docker, and every VM it makes carries
# the dp-vm- prefix; nothing without that prefix is ever acted on.

usage() {
    printf 'usage: %s create|start|poweroff|destroy|ip|ssh|list NAME [ARGS...]\n' "$0" >&2
    printf 'NAME must begin with dp-vm-\n' >&2
}

fail() {
    printf 'vm lab: %s\n' "$*" >&2
    exit 1
}

pool=default
base_image=noble-server-cloudimg-amd64.img
image_dir=/var/lib/libvirt/images
vm_memory=${VM_MEMORY_MIB:-2048}
vm_vcpus=${VM_VCPUS:-2}
vm_disk_gib=${VM_DISK_GIB:-20}
key_file=${VM_SSH_KEY:-$HOME/.ssh/dockpilot-vm-lab}

command -v virsh >/dev/null 2>&1 || fail "virsh is not installed"
command -v virt-install >/dev/null 2>&1 || fail "virt-install is not installed"
command -v cloud-localds >/dev/null 2>&1 || fail "cloud-localds is not installed"

# virsh needs the libvirt group. A shell that predates the group membership
# still works through sg, so this is transparent either way.
vsh() {
    if virsh --connect qemu:///system version >/dev/null 2>&1; then
        virsh --connect qemu:///system "$@"
    else
        sg libvirt -c "virsh --connect qemu:///system $*"
    fi
}

require_name() {
    case "$1" in
        dp-vm-*) ;;
        *) fail "refusing to act on \"$1\": lab VMs must be named dp-vm-*" ;;
    esac
}

ensure_key() {
    [ -f "$key_file" ] && return 0
    mkdir -p "$(dirname "$key_file")"
    ssh-keygen -t ed25519 -N '' -C dockpilot-vm-lab -f "$key_file" >/dev/null
    printf 'created lab ssh key: %s\n' "$key_file"
}

cmd_create() {
    name=$1
    require_name "$name"
    ensure_key
    vsh dominfo "$name" >/dev/null 2>&1 && fail "$name already exists"
    [ -f "$image_dir/$base_image" ] ||
        fail "base image missing: $image_dir/$base_image (run prepare-vm-lab-image.sh)"

    work=$(mktemp -d)
    # The seed carries the whole guest configuration: there is no manual step
    # between creating a VM and having a Docker host with a key on it.
    cat >"$work/user-data" <<CLOUDINIT
#cloud-config
hostname: $name
users:
  - name: lab
    groups: [sudo, docker]
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - $(cat "$key_file.pub")
package_update: true
packages:
  - docker.io
  - docker-compose-v2
  - curl
  - jq
runcmd:
  - [ systemctl, enable, --now, docker ]
  - [ sh, -c, "usermod -aG docker lab" ]
  - [ sh, -c, "docker version >/var/log/dockpilot-vm-ready 2>&1 || true" ]
CLOUDINIT
    printf 'instance-id: %s\nlocal-hostname: %s\n' "$name" "$name" >"$work/meta-data"
    cloud-localds "$work/$name-seed.iso" "$work/user-data" "$work/meta-data"

    # The disk is a copy-on-write overlay on the shared base image, so a VM
    # costs its own writes and nothing more.
    vsh vol-create-as "$pool" "$name.qcow2" "${vm_disk_gib}G" \
        --format qcow2 --backing-vol "$base_image" --backing-vol-format qcow2 >/dev/null
    vsh vol-create-as "$pool" "$name-seed.iso" 1M --format raw >/dev/null
    vsh vol-upload --pool "$pool" "$name-seed.iso" "$work/$name-seed.iso"
    rm -rf "$work"

    virt_install() {
        virt-install --connect qemu:///system --name "$name" \
            --memory "$vm_memory" --vcpus "$vm_vcpus" \
            --disk "vol=$pool/$name.qcow2,device=disk,bus=virtio" \
            --disk "vol=$pool/$name-seed.iso,device=cdrom" \
            --os-variant ubuntu24.04 --network network=default,model=virtio \
            --graphics none --noautoconsole --import
    }
    if virsh --connect qemu:///system version >/dev/null 2>&1; then
        virt_install
    else
        sg libvirt -c "$(command -v virt-install) --connect qemu:///system --name $name \
            --memory $vm_memory --vcpus $vm_vcpus \
            --disk vol=$pool/$name.qcow2,device=disk,bus=virtio \
            --disk vol=$pool/$name-seed.iso,device=cdrom \
            --os-variant ubuntu24.04 --network network=default,model=virtio \
            --graphics none --noautoconsole --import"
    fi
    printf '%s created; cloud-init installs Docker on first boot\n' "$name"
}

cmd_ip() {
    name=$1
    require_name "$name"
    deadline=$(( $(date +%s) + 180 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        address=$(vsh domifaddr "$name" 2>/dev/null | awk '/ipv4/ { sub("/.*", "", $4); print $4; exit }')
        if [ -n "$address" ]; then
            printf '%s\n' "$address"
            return 0
        fi
        sleep 3
    done
    fail "$name has no address yet"
}

cmd_ssh() {
    name=$1
    shift
    require_name "$name"
    address=$(cmd_ip "$name")
    ssh -i "$key_file" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR -o ConnectTimeout=10 "lab@$address" "$@"
}

cmd_wait_ready() {
    name=$1
    require_name "$name"
    deadline=$(( $(date +%s) + 900 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if cmd_ssh "$name" 'docker version >/dev/null 2>&1' >/dev/null 2>&1; then
            printf '%s is ready\n' "$name"
            return 0
        fi
        sleep 10
    done
    fail "$name never finished cloud-init"
}

cmd_poweroff() {
    name=$1
    require_name "$name"
    # destroy is libvirt's term for cutting power, not for deleting anything.
    vsh destroy "$name"
    printf '%s powered off abruptly\n' "$name"
}

cmd_start() {
    name=$1
    require_name "$name"
    vsh start "$name"
}

cmd_destroy() {
    name=$1
    require_name "$name"
    vsh destroy "$name" >/dev/null 2>&1 || true
    vsh undefine "$name" --nvram >/dev/null 2>&1 || vsh undefine "$name" >/dev/null 2>&1 || true
    vsh vol-delete --pool "$pool" "$name.qcow2" >/dev/null 2>&1 || true
    vsh vol-delete --pool "$pool" "$name-seed.iso" >/dev/null 2>&1 || true
    printf '%s destroyed\n' "$name"
}

cmd_list() {
    vsh list --all | awk 'NR<=2 || /dp-vm-/'
}

[ "$#" -ge 1 ] || { usage; exit 2; }
action=$1
shift
case "$action" in
    create) [ "$#" -ge 1 ] || { usage; exit 2; }; cmd_create "$1" ;;
    start) [ "$#" -ge 1 ] || { usage; exit 2; }; cmd_start "$1" ;;
    poweroff) [ "$#" -ge 1 ] || { usage; exit 2; }; cmd_poweroff "$1" ;;
    destroy) [ "$#" -ge 1 ] || { usage; exit 2; }; cmd_destroy "$1" ;;
    ip) [ "$#" -ge 1 ] || { usage; exit 2; }; cmd_ip "$1" ;;
    ready) [ "$#" -ge 1 ] || { usage; exit 2; }; cmd_wait_ready "$1" ;;
    ssh) [ "$#" -ge 1 ] || { usage; exit 2; }; cmd_ssh "$@" ;;
    list) cmd_list ;;
    *) usage; exit 2 ;;
esac
