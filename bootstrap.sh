#!/bin/bash
# Install a Sailfish development rootfs from the public release repositories.
# Fedora's rpm writes the database as ndb under /var/lib/rpm, which is what
# the target's rpm 4.16 reads.
set -euo pipefail

SFOS_VERSION="${SFOS_VERSION:?}"
SFOS_ARCH="${SFOS_ARCH:?}"

mkdir -p /etc/rpm
printf '%s\n' '%_dbpath /var/lib/rpm' '%_db_backend ndb' > /etc/rpm/macros.sfos

case "$SFOS_ARCH" in
aarch64|armv7hl|i486) ;;
*)
	echo "unknown SFOS_ARCH=$SFOS_ARCH" >&2
	exit 1
	;;
esac

base="https://releases.jolla.com/releases/${SFOS_VERSION}"
repo_args=()
repo_ids=()
add_repo() {
	name=$1
	url=$2
	if curl -fsS -A sfosbuild -o /dev/null "${url}/repodata/repomd.xml"; then
		echo "sfosbuild: repo ${name} ${url}"
		repo_args+=(--repofrompath="${name},${url}" --setopt="${name}.gpgcheck=0")
		repo_ids+=("$name")
	else
		echo "sfosbuild: skip ${name} (${url})"
	fi
}
add_repo jolla "${base}/jolla/${SFOS_ARCH}"
add_repo hotfixes "${base}/hotfixes/${SFOS_ARCH}"
add_repo sdk "${base}/sdk/${SFOS_ARCH}"
add_repo adaptation-common "${base}/jolla-hw/adaptation-common/${SFOS_ARCH}"

found_jolla=
for id in "${repo_ids[@]}"; do
	if [ "$id" = jolla ]; then
		found_jolla=1
	fi
done
if [ -z "$found_jolla" ]; then
	echo "no jolla repository for ${SFOS_VERSION} ${SFOS_ARCH}" >&2
	exit 1
fi

ids=$(IFS=,; echo "${repo_ids[*]}")
mkdir -p /sfos
dnf -y \
	--installroot=/sfos \
	--forcearch="$SFOS_ARCH" \
	--setopt=install_weak_deps=False \
	--setopt=tsflags=noscripts \
	--nogpgcheck \
	"${repo_args[@]}" \
	--repo="$ids" \
	install \
	patterns-sailfish-configuration-sdk-target \
	patterns-sailfish-development-tools \
	spectacle \
	gnu-tar \
	ca-certificates

rm -rf \
	/sfos/var/cache/libdnf5 \
	/sfos/var/cache/dnf \
	/sfos/var/lib/dnf \
	/sfos/var/log/dnf.log \
	/sfos/var/log/hawkey.log \
	/sfos/var/log/dnf.librepo.log

mkdir -p /sfos/dev /sfos/proc /sfos/sys /sfos/tmp /sfos/root /sfos/run
chmod 1777 /sfos/tmp
if [ ! -e /sfos/dev/null ]; then
	mknod -m 666 /sfos/dev/null c 1 3 || true
	mknod -m 666 /sfos/dev/zero c 1 5 || true
	mknod -m 666 /sfos/dev/random c 1 8 || true
	mknod -m 666 /sfos/dev/urandom c 1 9 || true
	mknod -m 666 /sfos/dev/tty c 5 0 || true
fi

test -x /sfos/usr/bin/rpm
test -x /sfos/usr/bin/zypper
test -d /sfos/var/lib/rpm
