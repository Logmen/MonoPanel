#!/usr/bin/env bash
# Local side of the MonoPanel testbed: drives scripts/testbed/pve.sh on the
# Proxmox host over ssh, keeps ssh aliases for the VMs, deploys the panel to
# them and runs the end-to-end tests. Settings come from .dev/testbed.env
# (git-ignored):
#
#   PVE_HOST=root@pve.example.com     # ssh destination of the Proxmox host
#   TB_PREFIX=192.0.2                 # LAN of the VMs, first three octets
#   TB_GATEWAY=192.0.2.1              # optional, default .1
#   TB_IP_BASE=200                    # VM n gets TB_PREFIX.(TB_IP_BASE+n)
#   TB_SSH_KEY_FILE=~/.ssh/id_ed25519 # private key; its .pub goes to root@vm
#   TB_ZONE=example.com               # optional: Cloudflare zone for VM names
#   TB_DNS_LABEL=tb                   # VM n is <name>.tb.example.com (+ wildcard)
#   TB_CF_TOKEN=…                     # Cloudflare API token, Zone:DNS:Edit on the zone
#
#   testbed.sh up|reset|down [name...]   VMs on the Proxmox host
#   testbed.sh status | names            what exists
#   testbed.sh ssh-config                write ~/.ssh/config.d/monopanel-testbed
#   testbed.sh deploy <name> [--binary]  package (default) or binary + bootstrap
#   testbed.sh e2e <name>                make e2e against the VM
#   testbed.sh checks <name>             the hardening checks on the VM (scripts/testbed/checks.sh)
#   testbed.sh matrix [name...]          reset + deploy + e2e + checks for each VM, summary
#   testbed.sh upgrade [name...]         reset, the previous release, its objects, a pure package
#                                        upgrade to this build, checks; TB_FROM=v0.8.19 picks the release
#   testbed.sh migrate <src> <dst>       move an account between two VMs and verify
#   testbed.sh dns [up|down|list]        A records for the VMs in the Cloudflare zone
#   testbed.sh cms <name> [cms...]       mp cms install wordpress|joomla|opencart|bitrix into preset sites and check
#   testbed.sh sources <args...>         the BitrixVM and FASTPANEL source machines (scripts/testbed/sources.sh)
set -euo pipefail
export PATH="$HOME/go/bin:$HOME/.local/go/bin:$HOME/.local/node/bin:$PATH"

root=$(cd "$(dirname "$0")/../.." && pwd)
env_file="$root/.dev/testbed.env"
[ -f "$env_file" ] || { echo "testbed.sh: create $env_file first (see the header of this script)" >&2; exit 2; }
# shellcheck disable=SC1090
. "$env_file"
: "${PVE_HOST:?PVE_HOST in $env_file}"
: "${TB_PREFIX:?TB_PREFIX in $env_file}"
: "${TB_SSH_KEY_FILE:=$HOME/.ssh/id_ed25519}"
TB_SSH_KEY_FILE=${TB_SSH_KEY_FILE/#\~/$HOME}
[ -f "$TB_SSH_KEY_FILE.pub" ] || { echo "testbed.sh: $TB_SSH_KEY_FILE.pub not found" >&2; exit 2; }

ssh_cfg="$HOME/.ssh/config.d/monopanel-testbed"
logs="$root/.dev/testbed/logs"
mkdir -p "$logs"
self="$root/scripts/testbed/testbed.sh"

log() { printf '\033[36m[%s]\033[0m %s\n' "$(date +%H:%M:%S)" "$*" >&2; }

# vm_family <ssh alias>: deb or rpm
vm_family() { ssh "$1" '. /etc/os-release; case " $ID $ID_LIKE " in *debian*|*ubuntu*) echo deb;; *) echo rpm;; esac'; }

# vm_addr <name>: the panel's address, its name in the zone when there is one
vm_addr() {
	if [ -n "${TB_ZONE:-}" ]; then echo "$1.${TB_DNS_LABEL:-tb}.$TB_ZONE"; else ssh "mp-$1" 'hostname -I | cut -d" " -f1'; fi
}

# extras <family>: what a deploy installs besides nginx, PHP and the
# database: TB_EXTRA when it is set (empty for nothing), else Apache on
# Debian and Ubuntu — sites run in Apache mode there, and checks.sh tests
# the accounts' Apaches.
extras() {
	if [ -n "${TB_EXTRA+set}" ]; then
		printf '%s' "$TB_EXTRA"
	elif [ "$1" = deb ]; then
		printf apache
	fi
}

# dist_package <family>: this build's newest package in dist/
dist_package() {
	case "$1" in
	deb) (cd "$root/dist" && ls -t monopanel_*_amd64.deb | head -1) ;;
	rpm) (cd "$root/dist" && ls -t monopanel-*.x86_64.rpm | head -1) ;;
	esac
}

# remote <args...>: run pve.sh on the Proxmox host with our settings.
remote() {
	local vars="" v
	for v in TB_PREFIX TB_GATEWAY TB_DNS TB_IP_BASE TB_VMID_BASE TB_BRIDGE TB_STORAGE TB_CORES TB_MEMORY TB_DISK TB_CIDR $(compgen -v TB_IMAGE_URL_ || true); do
		[ -n "${!v:-}" ] && vars+="$v=$(printf %q "${!v}") "
	done
	vars+="TB_SSH_KEY=$(printf %q "$(cut -d' ' -f1,2 "$TB_SSH_KEY_FILE.pub")")"
	# shellcheck disable=SC2029
	ssh "$PVE_HOST" "$vars bash -s -- $(printf '%q ' "$@")" <"$root/scripts/testbed/pve.sh"
}

# wait_cloud_init blocks until a VM accepts root over ssh and cloud-init
# reports done: the key arrives early in cloud-init, the final stage may still
# be installing packages. "done with recoverable errors" (exit 2) is accepted.
wait_cloud_init() {
	local name=$1 code
	for _ in $(seq 1 120); do
		if ssh -o BatchMode=yes -o ConnectTimeout=5 "mp-$name" true >/dev/null 2>&1; then
			ssh -o BatchMode=yes "mp-$name" 'cloud-init status --wait >/dev/null 2>&1; echo $?' 2>/dev/null | grep -qE '^[02]$' && { log "mp-$name: cloud-init done"; return; }
			code=$(ssh -o BatchMode=yes "mp-$name" 'cloud-init status 2>&1 | head -1')
			echo "mp-$name: cloud-init: $code" >&2
			return 1
		fi
		sleep 5
	done
	echo "mp-$name: no ssh in 10 minutes" >&2
	return 1
}

# wait_ssh blocks until a VM accepts root over ssh: after a rollback the
# guest agent may answer before sshd is up (Oracle's images start it early).
wait_ssh() {
	local name=$1
	for _ in $(seq 1 60); do
		ssh -o BatchMode=yes -o ConnectTimeout=5 "mp-$name" true >/dev/null 2>&1 && return
		sleep 5
	done
	echo "mp-$name: no ssh in 5 minutes" >&2
	return 1
}

# ssh_config writes one Host stanza per VM. Rebuilt VMs get new host keys, so
# they live in their own known_hosts file that is simply removed on `down`.
ssh_config() {
	mkdir -p "$(dirname "$ssh_cfg")"
	{
		echo "# MonoPanel testbed, generated by scripts/testbed/testbed.sh ssh-config"
		remote list | while read -r name _ ip; do
			cat <<STANZA
Host mp-$name
    HostName $ip
    User root
    IdentityFile $TB_SSH_KEY_FILE
    IdentitiesOnly yes
    StrictHostKeyChecking accept-new
    UserKnownHostsFile ~/.ssh/known_hosts.monopanel-testbed
    LogLevel ERROR
STANZA
		done
	} >"$ssh_cfg"
	if ! grep -qs "^Include ~/.ssh/config.d/monopanel-testbed" "$HOME/.ssh/config"; then
		# Include has to come before the first Host block to apply globally.
		{ echo "Include ~/.ssh/config.d/monopanel-testbed"; cat "$HOME/.ssh/config" 2>/dev/null; } >"$HOME/.ssh/config.new"
		mv "$HOME/.ssh/config.new" "$HOME/.ssh/config"
	fi
	log "wrote $ssh_cfg"
}

# deploy <name> [--binary]: build, copy, run bootstrap.sh on the VM.
deploy() {
	local name=$1 how=${2:-} vm ip family pkg extra
	vm="mp-$name"
	ip=$(ssh "$vm" 'hostname -I | cut -d" " -f1')
	addr=$ip
	[ -z "${TB_ZONE:-}" ] || addr="$name.${TB_DNS_LABEL:-tb}.$TB_ZONE"
	family=$(vm_family "$vm")
	extra=$(extras "$family")
	log "$vm ($ip, $family): $([ -n "${TB_PREBUILT:-}" ] && echo "deploying" || echo "building")${extra:+, with $extra}"
	if [ "$how" = "--binary" ]; then
		[ -n "${TB_PREBUILT:-}" ] || (cd "$root" && make -s build)
		scp -q "$root/dist/monopanel" "$vm:/tmp/monopanel"
		pkg=/tmp/monopanel
	else
		[ -n "${TB_PREBUILT:-}" ] || (cd "$root" && make -s "$family" >/dev/null)
		pkg=$(dist_package "$family")
		scp -q "$root/dist/$pkg" "$vm:/tmp/$pkg"
		pkg="/tmp/$pkg"
	fi
	scp -q "$root/scripts/testbed/bootstrap.sh" "$vm:/tmp/bootstrap.sh"
	# Let's Encrypt allows five certificates per name a week and every reset
	# would ask for a new one: a certificate issued once is kept here and
	# put back on the rebuilt VM while it is good for another month.
	local certdir="$root/.dev/testbed/certs/$name" panelcert=""
	if [ -s "$certdir/panel.crt" ] && [ -s "$certdir/panel.key" ] && openssl x509 -in "$certdir/panel.crt" -noout -checkend 2592000 >/dev/null 2>&1; then
		scp -q "$certdir/panel.crt" "$vm:/tmp/panel-cert.crt" && scp -q "$certdir/panel.key" "$vm:/tmp/panel-cert.key" && panelcert=/tmp/panel-cert
	fi
	# The Cloudflare token travels on stdin, not on a command line. The
	# extras are quoted: "apache fail2ban" is one value.
	# shellcheck disable=SC2029
	ssh "$vm" "TB_CF_TOKEN=\$(head -1) TB_PHP=$(printf %q "${TB_PHP:-}") TB_DB=$(printf %q "${TB_DB:-}") TB_EXTRA=$(printf %q "$extra") TB_PANEL_CERT=$panelcert bash /tmp/bootstrap.sh $pkg $addr" <<<"${TB_CF_TOKEN:-}"
	# Keep a freshly issued certificate for the next reset. An issued one
	# lives with the panel's ACME certificates under its name, not in tls/
	# (that is the self-signed one it starts with).
	local issued="/var/lib/monopanel/certs/$addr"
	# shellcheck disable=SC2029
	if [ -z "$panelcert" ] && ssh "$vm" "openssl x509 -in $issued/fullchain.pem -noout -issuer 2>/dev/null" | grep -qi "let's encrypt"; then
		mkdir -p "$certdir" && chmod 700 "$certdir"
		scp -q "$vm:$issued/fullchain.pem" "$certdir/panel.crt" && scp -q "$vm:$issued/privkey.pem" "$certdir/panel.key" && chmod 600 "$certdir"/panel.* && log "$vm: panel certificate kept in $certdir"
	fi
}

e2e() {
	local name=$1
	# A run of TestOwnPasswordNeedsCurrent that was killed before its cleanup
	# leaves its throwaway administrator e2eadm<hhmmss> behind, and with two
	# administrators the token make e2e asks root for (mp token create, no
	# --user) is refused. Only that name goes, on a testbed machine.
	# shellcheck disable=SC2016 # expanded on the machine
	ssh -n "mp-$name" 'for u in $(mp user list --json 2>/dev/null | grep -oE "\"login\": *\"e2eadm[0-9]{6}\"" | grep -oE "e2eadm[0-9]{6}"); do
		mp user rm "$u" --purge --yes </dev/null >/dev/null 2>&1 && echo "removed $u, the administrator an interrupted e2e run left" >&2
	done' || true
	(cd "$root" && MONOPANEL_INSECURE=1 make -s e2e HOST="mp-$name")
}

# checks <name>: scripts/testbed/checks.sh on the VM, with the warnings the
# doctor may give (doctor-expected.sh); TB_CARRIED=1 also checks what
# upgrade-seed.sh made. Its PASS/FAIL lines and TOTAL go to stdout.
checks() {
	local name=$1 vm="mp-$1"
	log "$vm: checks${TB_CARRIED:+ (with what the previous release made)}"
	ssh "$vm" 'mkdir -p /root/tb-checks && chmod 700 /root/tb-checks'
	scp -q "$root/scripts/testbed/checks.sh" "$root/scripts/testbed/doctor-expected.sh" "$vm:/root/tb-checks/"
	# shellcheck disable=SC2029
	ssh -n "$vm" "TB_CARRIED=${TB_CARRIED:+1} bash /root/tb-checks/checks.sh $name"
}

# step <file> <label> <testbed.sh args...>: one step of a VM's cycle as a
# testbed.sh process of its own — errexit holds inside it, whatever the
# caller does with the status — recorded as "<label> ok|FAIL" in file.
step() {
	local file=$1 label=$2
	shift 2
	if bash "$self" "$@"; then
		echo "$label ok" >>"$file"
		return 0
	fi
	echo "$label FAIL" >>"$file"
	return 1
}

# step_of <file> <label>: what step wrote for it, "-" when it did not run
step_of() { awk -v s="$2" '$1 == s {r = $2} END {print (r == "" ? "-" : r)}' "$1" 2>/dev/null || echo -; }

# matrix: the whole cycle per VM, in parallel, with one log per VM and a
# table: reset, deploy, e2e, checks — e2e and checks both run when the
# deploy did. TB_NO_RESET=1 skips the rollback (for VMs that are fresh
# anyway), TB_NO_CHECKS=1 the checks.
matrix() {
	local n i pids=() names=("$@") results=() f
	log "building the packages once for ${names[*]}"
	(cd "$root" && make -s deb >/dev/null && make -s rpm >/dev/null)
	export TB_PREBUILT=1
	for n in "${names[@]}"; do
		f="$logs/.matrix-$n"
		: >"$f"
		(
			exec >"$logs/$n.log" 2>&1
			[ -n "${TB_NO_RESET:-}" ] || step "$f" reset reset "$n" || exit 1
			step "$f" deploy deploy "$n" || exit 1
			rc=0
			step "$f" e2e e2e "$n" || rc=1
			[ -n "${TB_NO_CHECKS:-}" ] || step "$f" checks checks "$n" || rc=1
			exit "$rc"
		) &
		pids+=($!)
		# eleven resets at once open eleven ssh sessions to the Proxmox host in
		# the same second; sshd's MaxStartups drops the last one (ol10 every time)
		sleep 2
	done
	for i in "${!names[@]}"; do
		if wait "${pids[$i]}"; then results+=("${names[$i]} ok"); else results+=("${names[$i]} FAIL"); fi
	done
	echo
	printf '%-12s %-7s %-7s %-7s %s\n' VM DEPLOY E2E CHECKS RESULT
	for i in "${!names[@]}"; do
		n=${names[$i]}
		f="$logs/.matrix-$n"
		printf '%-12s %-7s %-7s %-7s %s\n' "$n" "$(step_of "$f" deploy)" "$(step_of "$f" e2e)" "$(step_of "$f" checks)" "${results[$i]#* }"
		rm -f "$f"
	done
	echo "logs: $logs/"
	printf '%s\n' "${results[@]}" | grep -q FAIL && return 1 || return 0
}

# ------------------------------------------------- the previous release ----
# The upgrade starts from a published release: its package as GitHub serves
# it, checked against the release's SHA256SUMS and their signature, and its
# own bootstrap.sh from the tag.

# previous_release: the tag of the release before this build — the last tag
# for a development build (0.8.19-21-g…, 0.8.19-dirty), the one before it for
# a clean build of a tag. TB_FROM overrides.
previous_release() {
	local d
	if [ -n "${TB_FROM:-}" ]; then
		echo "$TB_FROM"
		return
	fi
	d=$(git -C "$root" describe --tags --dirty --match 'v[0-9]*' 2>/dev/null) || {
		echo "testbed.sh: no release tag in this tree: set TB_FROM=v<version>" >&2
		return 1
	}
	if [[ $d =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
		git -C "$root" describe --tags --abbrev=0 --match 'v[0-9]*' "$d^"
	else
		git -C "$root" describe --tags --abbrev=0 --match 'v[0-9]*'
	fi
}

# release_dir <tag>: where fetch_release keeps a release
release_dir() { echo "$root/.dev/testbed/releases/$1"; }

# release_asset <tag> <family>: the package's name, as the release workflow writes it
release_asset() {
	case "$2" in
	deb) echo "monopanel_${1#v}_amd64.deb" ;;
	rpm) echo "monopanel-${1#v}.x86_64.rpm" ;;
	esac
}

# verify_sums_signature <SHA256SUMS> <SHA256SUMS.sig> <base64 ed25519 key>:
# 0 good, 1 bad, 2 when this openssl cannot tell (ed25519 needs -rawin, 3.0)
verify_sums_signature() {
	local d rc help
	help=$(openssl pkeyutl -help 2>&1 || true)
	grep -q -- -rawin <<<"$help" || return 2
	d=$(mktemp -d)
	# the key as SubjectPublicKeyInfo: the DER prefix of an ed25519 key, then its 32 bytes
	{
		printf '\x30\x2a\x30\x05\x06\x03\x2b\x65\x70\x03\x21\x00'
		base64 -d <<<"$3"
	} >"$d/pub.der"
	base64 -d "$2" >"$d/sig" 2>/dev/null
	if openssl pkey -pubin -inform DER -in "$d/pub.der" -out "$d/pub.pem" 2>/dev/null &&
		openssl pkeyutl -verify -pubin -inkey "$d/pub.pem" -rawin -in "$1" -sigfile "$d/sig" >/dev/null 2>&1; then
		rc=0
	else
		rc=1
	fi
	rm -rf "$d"
	return "$rc"
}

# release_ok <tag>: the release in release_dir is complete and its packages
# are those of its SHA256SUMS
release_ok() {
	local dir f want
	dir=$(release_dir "$1")
	for f in "$(release_asset "$1" deb)" "$(release_asset "$1" rpm)"; do
		[ -s "$dir/$f" ] && [ -s "$dir/SHA256SUMS" ] || return 1
		want=$(awk -v f="$f" '$2 == f || $2 == "*" f {print $1}' "$dir/SHA256SUMS")
		[ -n "$want" ] && [ "$(sha256sum "$dir/$f" | cut -d' ' -f1)" = "$want" ] || return 1
	done
	[ -s "$dir/bootstrap.sh" ]
}

# fetch_release <tag>: download the release's deb, rpm, SHA256SUMS and its
# signature (gh release download; curl when gh is missing or fails), check them and take its
# bootstrap.sh from the tag. TB_REPO=owner/name when origin is not GitHub;
# TB_RELEASE_KEY is the key the signature is checked with (the one the
# panel has built in, internal/updater/release.pub, by default).
fetch_release() {
	local tag=$1 dir repo f key rc=0
	dir=$(release_dir "$tag")
	mkdir -p "$dir"
	if ! release_ok "$tag"; then
		repo=${TB_REPO:-$(git -C "$root" remote get-url origin 2>/dev/null | sed -nE 's#^(git@|https://)github\.com[:/]([^/]+/[^/.]+)(\.git)?$#\2#p')}
		[ -n "$repo" ] || {
			echo "testbed.sh: origin of this tree is not on GitHub: set TB_REPO=owner/name" >&2
			return 1
		}
		log "downloading $tag from $repo"
		rm -f "$dir/SHA256SUMS.sig"
		if command -v gh >/dev/null 2>&1 &&
			gh release download "$tag" --repo "$repo" --dir "$dir" --clobber \
				--pattern "$(release_asset "$tag" deb)" --pattern "$(release_asset "$tag" rpm)" --pattern SHA256SUMS; then
			gh release download "$tag" --repo "$repo" --dir "$dir" --clobber --pattern SHA256SUMS.sig 2>/dev/null || true
		else
			# no gh, or one that is not signed in: the public download URLs
			for f in "$(release_asset "$tag" deb)" "$(release_asset "$tag" rpm)" SHA256SUMS; do
				curl -fsSL -o "$dir/$f" "https://github.com/$repo/releases/download/$tag/$f"
			done
			curl -fsSL -o "$dir/SHA256SUMS.sig" "https://github.com/$repo/releases/download/$tag/SHA256SUMS.sig" 2>/dev/null || rm -f "$dir/SHA256SUMS.sig"
		fi
		git -C "$root" show "$tag:scripts/testbed/bootstrap.sh" >"$dir/bootstrap.sh" || {
			echo "testbed.sh: $tag has no scripts/testbed/bootstrap.sh" >&2
			return 1
		}
		release_ok "$tag" || {
			echo "testbed.sh: the packages of $tag do not match its SHA256SUMS" >&2
			return 1
		}
	fi
	if [ -s "$dir/SHA256SUMS.sig" ]; then
		key=${TB_RELEASE_KEY:-$(tr -d ' \n' <"$root/internal/updater/release.pub")}
		verify_sums_signature "$dir/SHA256SUMS" "$dir/SHA256SUMS.sig" "$key" || rc=$?
		case "$rc" in
		0) log "$tag: packages match SHA256SUMS, signed with the release key" ;;
		2) log "$tag: packages match SHA256SUMS; this openssl cannot check an ed25519 signature" ;;
		*)
			echo "testbed.sh: the signature of $tag's SHA256SUMS does not match the release key (TB_RELEASE_KEY)" >&2
			return 1
			;;
		esac
	else
		log "$tag: packages match SHA256SUMS; the release is not signed"
	fi
}

# upgrade_skip <tag> <name>: why an upgrade from that release cannot be tried
# on the VM at all; nothing when it can. TB_UPGRADE_ALL=1 tries anyway.
upgrade_skip() {
	local v=${1#v}
	# at or below 0.8.19 (sort -V puts the lower one first)
	if [ "$2" = ubuntu2604 ] && [ "$(printf '%s\n%s\n' "$v" 0.8.19 | sort -V | head -1)" = "$v" ]; then
		echo "$v cannot install PHP on Ubuntu 26.04 now that ppa:ondrej/php publishes resolute: it pins only the PPA's rsa1024 key, which apt 3 refuses"
		return 0
	fi
	return 1
}

# upgrade_one <name>: the whole upgrade of one VM, in a process of its own
# (testbed.sh upgrade-one): reset; the previous release with its own
# bootstrap.sh (no Cloudflare token: up to 0.8.19 it passed the token on a
# command line, and the panel stays on its self-signed certificate);
# upgrade-seed.sh with that release's mp; this build's package installed
# with apt or dnf alone, no mp setup; checks.sh with TB_CARRIED=1. The step
# reached goes to $logs/.upgrade-<name> for the summary.
upgrade_one() {
	local name=$1 vm="mp-$1" from=${TB_FROM:?} rel why addr family extra oldpkg newpkg st
	st="$logs/.upgrade-$name"
	rel=$(release_dir "$from")
	if why=$(upgrade_skip "$from" "$name") && [ -z "${TB_UPGRADE_ALL:-}" ]; then
		echo "SKIP $why" >"$st"
		log "$vm: no upgrade from $from: $why"
		return 0
	fi
	echo "reset" >"$st"
	remote reset "$name"
	wait_ssh "$name"
	family=$(vm_family "$vm")
	extra=$(extras "$family")
	# the Apache-mode site upgrade-seed.sh makes needs Apache, whatever TB_EXTRA says
	[ "$family" != deb ] || [[ " $extra " == *" apache "* ]] || extra="apache${extra:+ $extra}"
	addr=$(vm_addr "$name")
	oldpkg=$(release_asset "$from" "$family")
	echo "deploy $from" >"$st"
	log "$vm ($family): $from from its release${extra:+, with $extra}"
	scp -q "$rel/$oldpkg" "$vm:/tmp/$oldpkg"
	scp -q "$rel/bootstrap.sh" "$vm:/tmp/bootstrap-$from.sh"
	# shellcheck disable=SC2029
	ssh "$vm" "TB_PHP=$(printf %q "${TB_PHP:-}") TB_DB=$(printf %q "${TB_DB:-}") TB_EXTRA=$(printf %q "$extra") bash /tmp/bootstrap-$from.sh /tmp/$oldpkg $addr" </dev/null
	echo "seed" >"$st"
	log "$vm: the objects the upgrade has to carry, made with mp $from"
	scp -q "$root/scripts/testbed/upgrade-seed.sh" "$vm:/root/tb-upgrade-seed.sh"
	# shellcheck disable=SC2029
	ssh -n "$vm" "bash /root/tb-upgrade-seed.sh $name"
	echo "package upgrade" >"$st"
	[ -n "${TB_PREBUILT:-}" ] || (cd "$root" && make -s "$family" >/dev/null)
	newpkg=$(dist_package "$family")
	log "$vm: $newpkg with $([ "$family" = deb ] && echo apt-get || echo dnf) alone, no mp setup"
	scp -q "$root/dist/$newpkg" "$vm:/tmp/$newpkg"
	# What mp update apply runs (osprofile InstallArgv, its wait for the dpkg
	# lock included: a machine minutes after its boot may be running
	# unattended-upgrades), but for downgrades: a development build
	# (0.8.19~dirty, 0.8.19~21-g…) sorts below the release it follows, and
	# its scripts see an upgrade all the same. The script goes as an
	# argument: on stdin, apt or dnf could read its rest.
	local script
	script=$(
		cat <<'UPGRADE'
set -euo pipefail
pkg=$1
export DEBIAN_FRONTEND=noninteractive LC_ALL=C.UTF-8
echo "before: $(mp version | head -1)"
case "$pkg" in
*.deb)
	want=$(dpkg-deb -f "$pkg" Version)
	apt-get -q -y -o DPkg::Lock::Timeout=600 -o Dpkg::Options::=--force-confold --no-install-recommends --allow-downgrades install "$pkg"
	have=$(dpkg-query -W -f='${Version}' monopanel)
	;;
*.rpm)
	want=$(rpm -qp --qf '%{VERSION}-%{RELEASE}' "$pkg")
	dnf -q -y install "$pkg" || true
	have=$(rpm -q --qf '%{VERSION}-%{RELEASE}' monopanel)
	if [ "$have" != "$want" ]; then
		dnf -q -y downgrade "$pkg"
		have=$(rpm -q --qf '%{VERSION}-%{RELEASE}' monopanel)
	fi
	;;
*) echo "not a package: $pkg" >&2; exit 2 ;;
esac
[ "$have" = "$want" ] || { echo "monopanel is $have after the install, not $want" >&2; exit 1; }
echo "after: $(mp version | head -1), package $have"
UPGRADE
	)
	# shellcheck disable=SC2029
	ssh -n "$vm" "bash -c $(printf %q "$script") upgrade /tmp/$newpkg"
	echo "checks" >"$st"
	TB_CARRIED=1 checks "$name"
	echo "ok" >"$st"
}

# upgrade [name...]: upgrade_one for each VM in parallel, one log each
# (.dev/testbed/logs/upgrade-<name>.log) and a table. The packages are built
# and the release fetched once, before.
upgrade() {
	local names=("$@") n i pids=() rcs=() res failed=0
	TB_FROM=$(previous_release)
	export TB_FROM
	fetch_release "$TB_FROM"
	if [ -z "${TB_PREBUILT:-}" ]; then
		log "building the packages once for ${names[*]}"
		(cd "$root" && make -s deb >/dev/null && make -s rpm >/dev/null)
	fi
	export TB_PREBUILT=1
	log "upgrade from $TB_FROM to this build: ${names[*]}"
	for n in "${names[@]}"; do
		rm -f "$logs/.upgrade-$n"
		if [ "${#names[@]}" -eq 1 ]; then
			# one VM: on the terminal as well
			bash "$self" upgrade-one "$n" 2>&1 | tee "$logs/upgrade-$n.log" &
		else
			bash "$self" upgrade-one "$n" >"$logs/upgrade-$n.log" 2>&1 &
		fi
		pids+=($!)
		sleep 2
	done
	for i in "${!names[@]}"; do
		if wait "${pids[$i]}"; then rcs+=(0); else rcs+=(1); fi
	done
	echo
	printf '%-12s %s\n' VM RESULT
	for i in "${!names[@]}"; do
		n=${names[$i]}
		res=$(cat "$logs/.upgrade-$n" 2>/dev/null || echo "no step")
		case "${rcs[$i]} $res" in
		"0 ok") res=ok ;;
		"0 SKIP "*) ;;
		*)
			res="FAIL at $res"
			failed=1
			;;
		esac
		printf '%-12s %s\n' "$n" "$res"
		rm -f "$logs/.upgrade-$n"
	done
	echo "from $TB_FROM; logs: $logs/upgrade-<name>.log"
	return "$failed"
}

cmd=${1:-status}; shift || true
# shellcheck disable=SC2046 # VM names are single words
case "$cmd" in
up)
	[ $# -gt 0 ] || set -- $(remote names)
	remote up "$@"
	ssh_config
	for n in "$@"; do wait_cloud_init "$n"; done
	remote snapshot "$@"
	;;
reset)
	[ $# -gt 0 ] || set -- $(remote names)
	remote reset "$@"
	for n in "$@"; do wait_ssh "$n"; done
	;;
down) remote down "$@"; [ $# -eq 0 ] && rm -f "$HOME/.ssh/known_hosts.monopanel-testbed"; true ;;
status | names | list) remote "$cmd" ;;
ssh-config) ssh_config ;;
deploy) [ $# -ge 1 ] || { echo "usage: testbed.sh deploy <name> [--binary]" >&2; exit 2; }; deploy "$@" ;;
e2e) [ $# -eq 1 ] || { echo "usage: testbed.sh e2e <name>" >&2; exit 2; }; e2e "$1" ;;
checks) [ $# -eq 1 ] || { echo "usage: testbed.sh checks <name>" >&2; exit 2; }; checks "$1" ;;
matrix) [ $# -gt 0 ] || set -- $(remote names); matrix "$@" ;;
upgrade) [ $# -gt 0 ] || set -- $(remote names); upgrade "$@" ;;
upgrade-one) [ $# -eq 1 ] || { echo "usage: testbed.sh upgrade-one <name> (testbed.sh upgrade runs it)" >&2; exit 2; }; upgrade_one "$1" ;;
migrate) [ $# -eq 2 ] || { echo "usage: testbed.sh migrate <source> <target>" >&2; exit 2; }; exec "$root/scripts/testbed/migrate.sh" "$1" "$2" ;;
sources) exec "$root/scripts/testbed/sources.sh" "$@" ;;
cms)
	# Real CMSs on top of the presets; needs the VM names in DNS (dns up).
	[ $# -ge 1 ] || { echo "usage: testbed.sh cms <name> [wordpress|joomla|opencart|bitrix ...]" >&2; exit 2; }
	[ -n "${TB_ZONE:-}" ] || { echo "set TB_ZONE in $env_file: the sites need public names" >&2; exit 2; }
	n=$1; shift; [ $# -gt 0 ] || set -- wordpress joomla opencart bitrix
	scp -q "$root/scripts/testbed/cms.sh" "mp-$n:/root/"
	ssh "mp-$n" "TB_ZONE=$TB_ZONE TB_DNS_LABEL=${TB_DNS_LABEL:-tb} bash /root/cms.sh $n $*"
	;;
dns)
	[ -n "${TB_ZONE:-}" ] && [ -n "${TB_CF_TOKEN:-}" ] || { echo "set TB_ZONE and TB_CF_TOKEN in $env_file" >&2; exit 2; }
	export TB_ZONE TB_CF_TOKEN TB_DNS_LABEL
	case "${1:-up}" in
	up) exec python3 "$root/scripts/testbed/cfdns.py" up $(remote list | awk '{print $1, $3}' | tr '\n' ' ') ;;
	down | list) exec python3 "$root/scripts/testbed/cfdns.py" "$1" ;;
	*) echo "usage: testbed.sh dns [up|down|list]" >&2; exit 2 ;;
	esac
	;;
*) echo "usage: testbed.sh up|reset|down [name...] | status | ssh-config | deploy <name> | e2e <name> | checks <name> | matrix [name...] | upgrade [name...] | migrate <src> <dst> | dns [up|down|list]" >&2; exit 2 ;;
esac
