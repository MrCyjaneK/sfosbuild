#!/bin/sh
# specify (spectacle) yaml -> spec, then rpmbuild inside a Sailfish SDK target.
set -eu

SFOS_ARCH="${SFOS_ARCH:-aarch64}"
SRC=/build
TOP=/rpmbuild

case "$SFOS_ARCH" in
aarch64) RPM_TARGET=aarch64-meego-linux ;;
i486) RPM_TARGET=i486-meego-linux ;;
armv7hl) RPM_TARGET=armv7hl-meego-linux ;;
*)
	echo "unknown SFOS_ARCH=$SFOS_ARCH" >&2
	exit 1
	;;
esac

if ! command -v specify >/dev/null; then
	zypper --non-interactive in --force-resolution spectacle
fi

mkdir -p "$TOP/SPECS" "$TOP/BUILD" "$TOP/RPMS" "$TOP/SRPMS"

yaml=$(echo "$TOP"/rpm/*.yaml)
if [ -f "$yaml" ]; then
	base=$(basename "$yaml" .yaml)
	specify -N -n -o "$TOP/SPECS/${base}.spec" "$yaml"
fi
spec=$(echo "$TOP"/SPECS/*.spec)
test -f "$spec"

inplace=
if [ "${SFOS_INPLACE:-}" = 1 ]; then
	inplace=--build-in-place
	if [ -d "$SRC/rpm" ]; then
		SOURCEDIR="$SRC/rpm"
	else
		SOURCEDIR="$SRC"
	fi
	cd "$SRC"
else
	SOURCEDIR="$TOP/SOURCES"
fi

mode=-bb
if [ "${SFOS_SOURCE:-}" = 1 ]; then
	mode=-ba
fi

rpmbuild $inplace $mode \
	--define "_topdir ${TOP}" \
	--define "_sourcedir ${SOURCEDIR}" \
	--define "_missing_build_ids_terminate_build 0" \
	--define "certs_version ${CERTS_VERSION:-unknown}" \
	--target "$RPM_TARGET" \
	"$spec"

# Host copies these off the /rpmbuild mount. The output directory is not
# bind-mounted: it lives under $HOME, and VirtioFS drops that second mount.
if [ -n "${HOST_UID:-}" ]; then
	chown -R "${HOST_UID}:${HOST_GID:-$HOST_UID}" "$TOP/RPMS" "$TOP/SRPMS" || true
fi
