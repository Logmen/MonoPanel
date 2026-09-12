#!/bin/sh
# MonoPanel bootstrap: installs the latest release package for this system.
#
#   curl -fsSL https://raw.githubusercontent.com/Logmen/MonoPanel/main/packaging/install.sh | sh
#
# Set MONOPANEL_VERSION to pin a version (0.6.0), MONOPANEL_REPO to install
# from a fork, and MONOPANEL_TOKEN to read a private repository.
#
# The package is checked against the SHA256SUMS published with the release.
# That is a transport check, not a signature: it proves the download arrived
# intact from GitHub. Once the panel is installed it verifies the release
# signature itself on every later update (mp update trust).
set -eu

REPO="${MONOPANEL_REPO:-Logmen/MonoPanel}"
API="${MONOPANEL_API:-https://api.github.com}"
TOKEN="${MONOPANEL_TOKEN:-}"

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }

case "$(uname -m)" in
x86_64) ARCH=amd64; RPMARCH=x86_64 ;;
aarch64 | arm64) ARCH=arm64; RPMARCH=aarch64 ;;
*) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

. /etc/os-release
case "${ID_LIKE:-} ${ID:-}" in
*debian* | *ubuntu*) KIND=deb ;;
*rhel* | *fedora* | *centos*) KIND=rpm ;;
*) echo "unsupported distribution: ${PRETTY_NAME:-unknown}" >&2; exit 1 ;;
esac

# fetch <url> <output-file|-> [accept]. A variable assignment in front of a
# function call persists in POSIX sh, so the accept header is an argument.
fetch() {
	accept="${3:-application/vnd.github+json}"
	if [ "$2" = "-" ]; then
		if [ -n "$TOKEN" ]; then
			curl -fsSL -H "Authorization: Bearer $TOKEN" -H "Accept: $accept" "$1"
		else
			curl -fsSL -H "Accept: $accept" "$1"
		fi
	elif [ -n "$TOKEN" ]; then
		curl -fsSL -H "Authorization: Bearer $TOKEN" -H "Accept: $accept" -o "$2" "$1"
	else
		curl -fsSL -H "Accept: $accept" -o "$2" "$1"
	fi
}

# A public repository needs no API call at all: GitHub redirects
# /releases/latest to the tag page, and every asset has a plain download URL.
# The API allows 60 anonymous requests an hour per address — easily used up
# behind a shared NAT — so it is only used with a token, to read a private
# repository.
json=""
if [ -n "$TOKEN" ]; then
	release_url="$API/repos/$REPO/releases/latest"
	[ "${MONOPANEL_VERSION:-latest}" = "latest" ] || release_url="$API/repos/$REPO/releases/tags/v${MONOPANEL_VERSION#v}"
	json=$(fetch "$release_url" -) || { echo "cannot read releases of $REPO with the token" >&2; exit 1; }
	tag=$(printf '%s' "$json" | grep -o '"tag_name": *"[^"]*"' | head -1 | cut -d'"' -f4)
elif [ "${MONOPANEL_VERSION:-latest}" = "latest" ]; then
	# A repository without releases redirects to /releases instead of a tag.
	tag=$(curl -fsSI -o /dev/null -w '%{redirect_url}' "https://github.com/$REPO/releases/latest") || tag=""
	tag="${tag##*/}"
	case "$tag" in
	v[0-9]*) ;;
	*) echo "no release found in $REPO (private repository? set MONOPANEL_TOKEN)" >&2; exit 1 ;;
	esac
else
	tag="v${MONOPANEL_VERSION#v}"
fi
[ -n "$tag" ] || { echo "no release found in $REPO" >&2; exit 1; }
version="${tag#v}"

if [ "$KIND" = deb ]; then
	package="monopanel_${version}_${ARCH}.deb"
else
	package="monopanel-${version}.${RPMARCH}.rpm"
fi

# A private repository serves assets only through the API, by id; a public one
# has a plain download URL (which also answers 404 for a pinned version that
# does not exist — curl reports it).
asset_url() { # asset_url <file name>
	if [ -n "$TOKEN" ]; then
		# In GitHub's JSON an asset's "id" comes a few lines before its
		# "name", so remember the last id seen and print it at the match.
		id=$(printf '%s' "$json" | awk -v want="\"$1\"" '
			/"id": *[0-9]+,?$/ { n = $NF; gsub(/[^0-9]/, "", n); last = n }
			/"name": / && index($0, want) { print last; exit }')
		[ -n "$id" ] || { echo "release $tag has no $1" >&2; exit 1; }
		echo "$API/repos/$REPO/releases/assets/$id"
	else
		echo "https://github.com/$REPO/releases/download/$tag/$1"
	fi
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "MonoPanel $version ($package)"
fetch "$(asset_url "$package")" "$tmp/$package" application/octet-stream ||
	{ echo "cannot download $package of release $tag from $REPO" >&2; exit 1; }
fetch "$(asset_url SHA256SUMS)" "$tmp/SHA256SUMS" application/octet-stream ||
	{ echo "release $tag of $REPO has no SHA256SUMS" >&2; exit 1; }
(cd "$tmp" && sha256sum -c --ignore-missing SHA256SUMS >/dev/null) ||
	{ echo "checksum mismatch: refusing to install $package" >&2; exit 1; }

if [ "$KIND" = deb ]; then
	apt-get -q update
	DEBIAN_FRONTEND=noninteractive apt-get -q -y install "$tmp/$package"
else
	dnf -q -y install "$tmp/$package"
fi

echo
echo "Установлено. Дальше:"
echo "  mp setup     # администратор, каталоги, сервисы"
echo "  mp update    # обновления: релизы $REPO (сборка знает свой репозиторий; сменить — mp update settings --repo owner/name)"
