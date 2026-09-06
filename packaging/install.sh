#!/bin/sh
# MonoPanel bootstrap: detects the OS, adds the package repository and runs `mp setup`.
# Usage: curl -fsSL https://get.monopanel.example | sh
set -eu
REPO_BASE="${MONOPANEL_REPO:-https://repo.monopanel.example}"
if [ "$(id -u)" -ne 0 ]; then echo "run as root" >&2; exit 1; fi
. /etc/os-release
case "${ID_LIKE:-} ${ID}" in
  *debian*|*ubuntu*)
    apt-get -q update && apt-get -q -y install ca-certificates curl gnupg
    install -d -m 0755 /etc/apt/keyrings
    curl -fsSL "$REPO_BASE/monopanel.asc" -o /etc/apt/keyrings/monopanel.asc
    echo "deb [signed-by=/etc/apt/keyrings/monopanel.asc] $REPO_BASE/deb ${VERSION_CODENAME} main" > /etc/apt/sources.list.d/monopanel.list
    apt-get -q update && apt-get -q -y install monopanel ;;
  *rhel*|*fedora*|*centos*)
    cat > /etc/yum.repos.d/monopanel.repo <<REPO
[monopanel]
name=MonoPanel
baseurl=$REPO_BASE/rpm/el\$releasever/\$basearch
enabled=1
gpgcheck=1
gpgkey=$REPO_BASE/monopanel.asc
REPO
    dnf -q -y install monopanel ;;
  *) echo "unsupported distribution: $PRETTY_NAME" >&2; exit 1 ;;
esac
mp setup "$@"
