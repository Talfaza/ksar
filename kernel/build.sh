#!/usr/bin/env bash
# Builds the Ksar guest kernel: tinyconfig + kernel/ksar.config.
# Output: build/bzImage
set -euo pipefail

VERSION=${KSAR_KERNEL_VERSION:-6.18.57}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK="$ROOT/build/kernel"
SRC="$WORK/linux-$VERSION"
TARBALL="$WORK/linux-$VERSION.tar.xz"
URL="https://cdn.kernel.org/pub/linux/kernel/v${VERSION%%.*}.x"

mkdir -p "$WORK"
if [ ! -f "$TARBALL" ]; then
	echo ">> downloading linux-$VERSION"
	curl -sSfL -o "$TARBALL.part" "$URL/linux-$VERSION.tar.xz"
	mv "$TARBALL.part" "$TARBALL"
fi
echo ">> verifying checksum"
(cd "$WORK" && curl -sSfL "$URL/sha256sums.asc" | grep " linux-$VERSION.tar.xz\$" | sha256sum -c --quiet)

if [ ! -d "$SRC" ]; then
	echo ">> extracting"
	tar -C "$WORK" -xf "$TARBALL"
fi

cd "$SRC"
echo ">> configuring"
make -s ARCH=x86_64 tinyconfig
./scripts/kconfig/merge_config.sh -m -O . .config "$ROOT/kernel/ksar.config" >/dev/null
make -s ARCH=x86_64 olddefconfig

# merge_config silently drops options whose dependencies are unmet; fail
# loudly instead so the guest never boots without something we rely on.
missing=0
while IFS= read -r line; do
	case "$line" in
	CONFIG_*=y)
		opt=${line%%=*}
		if ! grep -qx "$opt=y" .config; then
			echo "!! $opt requested but not enabled" >&2
			missing=1
		fi
		;;
	esac
done <"$ROOT/kernel/ksar.config"
[ "$missing" = 0 ] || exit 1

echo ">> building with $(nproc) jobs"
make -s ARCH=x86_64 -j"$(nproc)" bzImage
cp arch/x86/boot/bzImage "$ROOT/build/bzImage"
echo ">> done: build/bzImage ($(du -h "$ROOT/build/bzImage" | cut -f1))"
