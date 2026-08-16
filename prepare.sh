#!/bin/sh
# Bootstrap the SDK target rootfs for sfosbuild.
# ssu plugin repos need Jolla credentials; use the public release mirrors.
set -eu

SFOS_VERSION="${SFOS_VERSION:-5.1.0.11}"
SFOS_ARCH="${SFOS_ARCH:-aarch64}"

ssu dr customer-jolla 2>/dev/null || true
ssu dr apps 2>/dev/null || true
ssu dr adaptation-common 2>/dev/null || true
ssu dr hotfixes 2>/dev/null || true
ssu dr jolla 2>/dev/null || true
ssu dr sdk 2>/dev/null || true
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
