#!/bin/sh
# Unpack an official Sailfish SDK Target .tar.7z into /sfos (host-arch stage).
set -eu

if [ -n "${TARGET_7Z_MD5:-}" ]; then
	echo "${TARGET_7Z_MD5}  /dl/target.tar.7z" | md5sum -c
fi
mkdir -p /tmp/unpacked /sfos
7z x -o/tmp/unpacked /dl/target.tar.7z
tarfile=$(find /tmp/unpacked -maxdepth 1 -type f -name '*.tar')
tar --numeric-owner -xf "$tarfile" -C /sfos \
	--exclude='./dev' --exclude='./proc' --exclude='./sys'
rm -rf /tmp/unpacked /dl/target.tar.7z
mkdir -p /sfos/dev /sfos/proc /sfos/sys /sfos/tmp
chmod 1777 /sfos/tmp
test -d /sfos/usr
