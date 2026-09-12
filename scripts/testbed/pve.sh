#!/usr/bin/env bash
# MonoPanel testbed on a Proxmox VE host: one VM per supported distribution,
# built from the official cloud images with cloud-init. Every VM gets a `clean`
# snapshot right after its first boot, so a test run can always start from a
# fresh OS: `reset` rolls back to it in seconds.
#
# This script runs ON the Proxmox host as root. The local wrapper
# (scripts/testbed/testbed.sh) pipes it over ssh with the settings from
# .dev/testbed.env, so nothing site-specific lives in the repository.
#
#   pve.sh up [name...]      create the VMs that are missing and boot them
#   pve.sh snapshot [name...] take the clean snapshot (the wrapper calls it once
#                            cloud-init is through, which only ssh can tell)
#   pve.sh reset [name...]   roll back to the clean snapshot and boot
#   pve.sh down [name...]    destroy the VMs (and their images stay cached)
#   pve.sh status            vmid, name, ip, state, snapshot, uptime
#   pve.sh list              "name vmid ip" per VM, for the local ssh config
#   pve.sh names             the distribution names
#
# Settings (environment):
#   TB_PREFIX     first three octets of the LAN, e.g. 192.0.2      (required)
#   TB_SSH_KEY    public key that becomes root's authorized key    (required)
#   TB_GATEWAY    default $TB_PREFIX.1;  TB_DNS defaults to the gateway
#   TB_IP_BASE    VM number n gets $TB_PREFIX.$((TB_IP_BASE + n)); default 200
#   TB_VMID_BASE  VM number n gets vmid TB_VMID_BASE + n; default 900
#   TB_BRIDGE     vmbr0;  TB_STORAGE local-lvm;  TB_CORES 2;  TB_MEMORY 3072 (MiB);  TB_DISK 32G
#   TB_IMAGE_URL_<name>  image URL override for one distribution (a nearby mirror)
set -euo pipefail

: "${TB_PREFIX:?first three octets of the network, e.g. 192.0.2}"
: "${TB_SSH_KEY:?public ssh key for root}"
: "${TB_CIDR:=24}"
: "${TB_GATEWAY:=$TB_PREFIX.1}"
: "${TB_DNS:=$TB_GATEWAY}"
: "${TB_IP_BASE:=200}"
: "${TB_VMID_BASE:=900}"
: "${TB_BRIDGE:=vmbr0}"
: "${TB_STORAGE:=local-lvm}"
: "${TB_CORES:=2}"
: "${TB_MEMORY:=3072}"
: "${TB_DISK:=32G}"
: "${TB_DOMAIN:=testbed}"
: "${TB_IMAGES:=/var/lib/vz/import}"
: "${TB_SNIPPETS:=/var/lib/vz/snippets}"

# The platform matrix from docs/02-platform-matrix.md. Number, name, image.
# The number fixes the vmid and the IP address, so keep it stable.
DISTROS=(
	"1 debian12   https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"
	"2 debian13   https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2"
	"3 ubuntu2204 https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img"
	"4 ubuntu2404 https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
	"5 ubuntu2604 https://cloud-images.ubuntu.com/resolute/current/resolute-server-cloudimg-amd64.img"
	"6 alma9      https://repo.almalinux.org/almalinux/9/cloud/x86_64/images/AlmaLinux-9-GenericCloud-latest.x86_64.qcow2"
	"7 alma10     https://repo.almalinux.org/almalinux/10/cloud/x86_64/images/AlmaLinux-10-GenericCloud-latest.x86_64.qcow2"
	"8 rocky9     https://dl.rockylinux.org/pub/rocky/9/images/x86_64/Rocky-9-GenericCloud-Base.latest.x86_64.qcow2"
	"9 rocky10    https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2"
	# Oracle names its images by update and build; check yum.oracle.com/oracle-linux-templates.html for newer ones.
	"10 ol9       https://yum.oracle.com/templates/OracleLinux/OL9/u8/x86_64/OL9U8_x86_64-kvm-b293.qcow2"
	"11 ol10      https://yum.oracle.com/templates/OracleLinux/OL10/u1/x86_64/OL10U1_x86_64-kvm-b291.qcow2"
)

# Sources for the foreign-panel migration tests (docs/07 §6): a machine per
# panel we import from. They are not part of the matrix, so `names` skips
# them; address them by name (testbed.sh up fastpanel bitrixvm).
# A fourth word holds options: bios=seabios for images without an EFI
# partition (the CentOS 7 cloud image).
SOURCES=(
	"12 fastpanel https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"
	"13 bitrixvm  https://repo.almalinux.org/almalinux/9/cloud/x86_64/images/AlmaLinux-9-GenericCloud-latest.x86_64.qcow2"
	# EOL source: CentOS 7 with bitrix-env 7, the way old Bitrix servers still run.
	"14 bitrixvm7 https://cloud.centos.org/centos/7/images/CentOS-7-x86_64-GenericCloud-2211.qcow2 bios=seabios"
)

log() { printf '\033[36m[%s]\033[0m %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die() { echo "pve.sh: $*" >&2; exit 1; }

# row <name> -> "n name url"
row() {
	local r
	for r in "${DISTROS[@]}" "${SOURCES[@]}"; do
		[ "$(awk '{print $2}' <<<"$r")" = "$1" ] && { echo "$r"; return; }
	done
	die "unknown distribution: $1 (see: pve.sh names)"
}
num() { row "$1" | awk '{print $1}'; }
url() { row "$1" | awk '{print $3}'; }
opt() { row "$1" | awk '{print $4}' | tr ' ' '\n' | sed -n "s/^$2=//p"; }
vmid() { echo $((TB_VMID_BASE + $(num "$1"))); }
ip() { echo "$TB_PREFIX.$((TB_IP_BASE + $(num "$1")))"; }
host() { echo "mp-$1"; }
names() { local r; for r in "${DISTROS[@]}"; do awk '{print $2}' <<<"$r"; done; }
# all_names adds the migration sources: for DNS and status, not for the matrix.
all_names() { local r; for r in "${DISTROS[@]}" "${SOURCES[@]}"; do awk '{print $2}' <<<"$r"; done; }
exists() { qm status "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------- images ----

# fetch_image downloads an image once. A per-distribution URL override
# (TB_IMAGE_URL_<name>, e.g. a nearby mirror) beats the table. The size is
# checked against Content-Length so a cut-off download is never imported.
fetch_image() {
	local name=$1 u file want got override
	override="TB_IMAGE_URL_$name"
	u=${!override:-$(url "$name")}
	file="$TB_IMAGES/mp-$name-$(basename "$u")"
	if [ ! -s "$file" ]; then
		log "$name: downloading $(basename "$u")"
		mkdir -p "$TB_IMAGES"
		curl -fsSL --retry 3 --retry-delay 5 -o "$file.part" "$u" || { rm -f "$file.part"; die "$name: download failed"; }
		want=$(curl -fsSIL "$u" | awk 'tolower($1)=="content-length:"{v=$2} END{gsub(/\r/,"",v); print v}')
		got=$(stat -c %s "$file.part")
		if [ -n "$want" ] && [ "$want" != "$got" ]; then
			rm -f "$file.part"
			die "$name: download is $got bytes, expected $want"
		fi
		mv "$file.part" "$file"
	fi
	echo "$file"
}

# Proxmox reads cloud-init snippets only from a storage that allows them.
ensure_snippets() {
	local content
	content=$(awk '/^dir: local$/{f=1} f&&/content/{print $2; exit}' /etc/pve/storage.cfg)
	case ",$content," in
	*,snippets,*) ;;
	*) log "enabling snippets on storage local"; pvesm set local --content "$content,snippets" ;;
	esac
	mkdir -p "$TB_SNIPPETS"
}

# The cloud-init drive sits on virtio-scsi (scsi1), not ide2: the Debian cloud
# kernel has no AHCI driver, so on q35 an ide2 drive is invisible to it.
#
# The user-data: root with our key, the guest agent, nothing else. The network
# comes from Proxmox (--ipconfig0), which is kept when only `user` is custom.
# The agent is (re)started last: some images (Oracle Linux) ship it running
# already, so a ping alone would not mean "runcmd is through". On EL the agent
# ships with guest-exec blocked (/etc/sysconfig/qemu-ga); the block is lifted
# by runcmd and the restart makes it take, but SELinux still refuses the exec
# itself — so on EL "Permission denied" from guest-exec is the ready signal,
# while "has been disabled" means the old agent is still running.
write_snippet() {
	local name=$1 h
	h=$(host "$name")
	cat >"$TB_SNIPPETS/$h.yaml" <<YAML
#cloud-config
hostname: $h
fqdn: $h.$TB_DOMAIN
manage_etc_hosts: true
disable_root: false
ssh_pwauth: false
users:
  - name: root
    ssh_authorized_keys:
      - $TB_SSH_KEY
bootcmd:
  # CentOS 7 is past its end of life: its mirrors are gone, the packages live on vault.centos.org.
  - '[ ! -f /etc/centos-release ] || ! grep -q " 7\." /etc/centos-release || sed -i "s|^mirrorlist=|#mirrorlist=|; s|^#baseurl=http://mirror.centos.org|baseurl=http://vault.centos.org|" /etc/yum.repos.d/CentOS-*.repo'
  # The CentOS 7 image leaves a dead libvirt nameserver first in resolv.conf; every lookup then waits 5 s and ssh logins take 40 s.
  - '[ ! -f /etc/centos-release ] || sed -i "/^nameserver 192.168.122.1/d" /etc/resolv.conf'
package_update: true
packages:
  - qemu-guest-agent
write_files:
  - path: /etc/ssh/sshd_config.d/10-testbed.conf
    content: |
      PermitRootLogin prohibit-password
      PasswordAuthentication no
runcmd:
  - mkdir -p /root/.ssh && chmod 700 /root/.ssh
  - grep -qF "$TB_SSH_KEY" /root/.ssh/authorized_keys 2>/dev/null || echo "$TB_SSH_KEY" >> /root/.ssh/authorized_keys
  - chmod 600 /root/.ssh/authorized_keys
  - systemctl restart sshd || systemctl restart ssh
  - '[ ! -f /etc/sysconfig/qemu-ga ] || sed -i -E "s/^FILTER_RPC_ARGS=.*/FILTER_RPC_ARGS=\"\"/" /etc/sysconfig/qemu-ga'
  - systemctl enable qemu-guest-agent
  - systemctl restart qemu-guest-agent
YAML
}

# ------------------------------------------------------------------- vms ----

create() {
	local name=$1 id h image
	id=$(vmid "$name"); h=$(host "$name")
	if exists "$id"; then
		log "$name: vm $id exists, skipping creation"
		return
	fi
	image=$(fetch_image "$name")
	write_snippet "$name"
	local bios
	bios=$(opt "$name" bios); bios=${bios:-ovmf}
	log "$name: creating vm $id ($h, $(ip "$name"), $bios)"
	qm create "$id" --name "$h" --ostype l26 --machine q35 --bios "$bios" \
		--cpu host --cores "$TB_CORES" --memory "$TB_MEMORY" \
		--scsihw virtio-scsi-single --net0 "virtio,bridge=$TB_BRIDGE" \
		--serial0 socket --vga serial0 --agent enabled=1,fstrim_cloned_disks=1 \
		--onboot 1 --tags monopanel-testbed >/dev/null
	[ "$bios" = seabios ] || qm set "$id" --efidisk0 "$TB_STORAGE:1,efitype=4m,pre-enrolled-keys=0" >/dev/null
	if ! qm set "$id" --scsi0 "$TB_STORAGE:0,import-from=$image,discard=on,ssd=1,iothread=1" >/dev/null; then
		# A half-made VM would be skipped by the next run; remove it.
		qm destroy "$id" --purge >/dev/null 2>&1 || true
		die "$name: importing $image failed"
	fi
	qm disk resize "$id" scsi0 "$TB_DISK" >/dev/null
	qm set "$id" --boot order=scsi0 --scsi1 "$TB_STORAGE:cloudinit" \
		--cicustom "user=local:snippets/$h.yaml" \
		--ipconfig0 "ip=$(ip "$name")/$TB_CIDR,gw=$TB_GATEWAY" \
		--nameserver "$TB_DNS" --searchdomain "$TB_DOMAIN" >/dev/null
}

# wait_ready blocks until the guest agent answers. That is all the host can
# see: guest-exec is blocked by the agent's filter on EL and by SELinux even
# when unblocked, and Oracle's images ship the agent running before cloud-init
# is through. Whether cloud-init finished is checked over ssh by the wrapper.
wait_ready() {
	local name=$1 id
	id=$(vmid "$name")
	for _ in $(seq 1 120); do
		qm agent "$id" ping >/dev/null 2>&1 && { log "$name: guest agent answers"; return; }
		sleep 5
	done
	die "$name: guest agent did not come up in 10 minutes"
}

snapshot_clean() {
	local name=$1 id
	id=$(vmid "$name")
	if qm listsnapshot "$id" | grep -q '^ *`->  *clean '; then
		return
	fi
	log "$name: shutting down for the clean snapshot"
	qm shutdown "$id" --timeout 180 >/dev/null
	qm snapshot "$id" clean --description "fresh cloud image, first boot done, guest agent installed" >/dev/null
	qm start "$id" >/dev/null
}

up() {
	local n
	ensure_snippets
	for n in "$@"; do create "$n"; done
	for n in "$@"; do
		[ "$(qm status "$(vmid "$n")" | awk '{print $2}')" = running ] || { log "$n: starting"; qm start "$(vmid "$n")" >/dev/null; }
	done
	for n in "$@"; do wait_ready "$n"; done
}

snapshot() {
	local n
	for n in "$@"; do snapshot_clean "$n"; done
	for n in "$@"; do wait_ready "$n"; done
	log "clean: $*"
}

reset() {
	local n id
	for n in "$@"; do
		id=$(vmid "$n")
		exists "$id" || die "$n: vm $id does not exist"
		log "$n: rolling back to clean"
		[ "$(qm status "$id" | awk '{print $2}')" = stopped ] || qm stop "$id" >/dev/null
		qm rollback "$id" clean >/dev/null
		qm start "$id" >/dev/null
	done
	for n in "$@"; do wait_ready "$n"; done
}

down() {
	local n id
	for n in "$@"; do
		id=$(vmid "$n")
		exists "$id" || continue
		log "$n: destroying vm $id"
		[ "$(qm status "$id" | awk '{print $2}')" = stopped ] || qm stop "$id" >/dev/null
		qm destroy "$id" --purge >/dev/null
		rm -f "$TB_SNIPPETS/$(host "$n").yaml"
	done
}

status() {
	local n id st snap
	printf '%-5s %-14s %-16s %-8s %s\n' VMID NAME IP STATE SNAPSHOT
	for n in $(all_names); do
		id=$(vmid "$n")
		if exists "$id"; then
			st=$(qm status "$id" | awk '{print $2}')
			snap=$(qm listsnapshot "$id" | grep -q '^ *`->  *clean ' && echo clean || echo -)
		else
			st=absent; snap=-
		fi
		printf '%-5s %-14s %-16s %-8s %s\n' "$id" "$(host "$n")" "$(ip "$n")" "$st" "$snap"
	done
}

list() { local n; for n in $(all_names); do echo "$n $(vmid "$n") $(ip "$n")"; done; }

# ------------------------------------------------------------------ main ----

cmd=${1:-status}; shift || true
case "$cmd" in
up | snapshot | reset | down)
	# shellcheck disable=SC2046 # the names are single words
	[ $# -gt 0 ] || set -- $(names)
	for n in "$@"; do row "$n" >/dev/null; done
	"$cmd" "$@"
	;;
status | list | names) "$cmd" ;;
*) die "usage: pve.sh up|snapshot|reset|down [name...] | status | list | names" ;;
esac
