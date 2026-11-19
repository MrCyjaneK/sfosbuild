#!/bin/sh
# Point zypper at the public release mirrors and install development tools.
# ssu plugin repos need Jolla credentials; use the public release mirrors.
# arm and aarch64 are not detected inside the container, so pin them.
# i486 is left to rpm: docker reports i686, and that accepts the i486 packages.
set -eu

SFOS_VERSION="${SFOS_VERSION:-5.1.0.11}"
SFOS_ARCH="${SFOS_ARCH:-aarch64}"

case "$SFOS_ARCH" in
aarch64|armv7hl)
	mkdir -p /etc/rpm /etc/zypp
	printf '%s\n' "${SFOS_ARCH}-meego-linux" > /etc/rpm/platform
	if [ -f /etc/zypp/zypp.conf ] && grep -q '^arch =' /etc/zypp/zypp.conf; then
		sed -i "s/^arch =.*/arch = ${SFOS_ARCH}/" /etc/zypp/zypp.conf
	else
		printf 'arch = %s\n' "$SFOS_ARCH" >> /etc/zypp/zypp.conf
	fi
	;;
esac

if command -v ldconfig >/dev/null 2>&1; then
	ldconfig || true
fi
if command -v update-ca-trust >/dev/null 2>&1; then
	update-ca-trust || true
fi

ssu dr customer-jolla 2>/dev/null || true
ssu dr apps 2>/dev/null || true
ssu dr adaptation-common 2>/dev/null || true
ssu dr hotfixes 2>/dev/null || true
ssu dr sdk 2>/dev/null || true
ssu ur || true
rm -f /etc/zypp/repos.d/ssu_*.repo

zypper --non-interactive rr jolla 2>/dev/null || true
zypper --non-interactive rr hotfixes 2>/dev/null || true
zypper --non-interactive rr adaptation-common 2>/dev/null || true

zypper ar -f "https://releases.jolla.com/releases/${SFOS_VERSION}/jolla/${SFOS_ARCH}/" jolla
zypper ar -f "https://releases.jolla.com/releases/${SFOS_VERSION}/hotfixes/${SFOS_ARCH}/" hotfixes
zypper ar -f "https://releases.jolla.com/releases/${SFOS_VERSION}/jolla-hw/adaptation-common/${SFOS_ARCH}/" adaptation-common

zypper --non-interactive --gpg-auto-import-keys refresh

zypper --non-interactive in --force-resolution \
	patterns-sailfish-development-tools \
	make \
	tar \
	spectacle

rpm --rebuilddb || true
