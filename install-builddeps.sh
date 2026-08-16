#!/bin/sh
# Install rpmspec BuildRequires into an SDK target image layer (cached by Docker).
set -eu

pkg="$1"
case "$pkg" in
*.yaml)
	specify -N -n -o /tmp/sfosbuild-spec.spec "$pkg"
	spec=/tmp/sfosbuild-spec.spec
	;;
*)
	spec="$pkg"
	;;
esac

deps=$(rpmspec -q --buildrequires "$spec")
if [ -n "$deps" ]; then
	zypper --non-interactive in --force-resolution $deps
fi
