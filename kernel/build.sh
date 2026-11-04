#!/bin/sh
# Builds the runplane guest kernel for Firecracker VMs: the Firecracker microVM
# guest config with kernel/runplane.config on top.
#
# Usage: kernel/build.sh [--config-only] [x86_64] [aarch64]
#
# With no arch, it builds the two. --config-only stops after "make
# olddefconfig" and the config checks, without a kernel build.
#
# Output in $OUT (default kernel/out):
#   vmlinux-x86_64          ELF vmlinux for x86_64 hosts
#   vmlinux-arm64           arm64 Image for aarch64 hosts
#   <name>.config           the final kernel config
#   <name>.check-config.txt the report of the Moby check-config.sh script
#
# Environment:
#   KERNEL_SERIES    6.1 (default) or 6.18
#   KERNEL_VERSION   exact version, for example 6.1.191 (default: latest of KERNEL_SERIES)
#   FIRECRACKER_REF  ref of firecracker-microvm/firecracker for the base config
#   MOBY_REF         ref of moby/moby for contrib/check-config.sh
#   CROSS_COMPILE    compiler prefix for a different arch (default: <arch>-linux-gnu-)
#   JOBS             parallel build jobs (default: nproc)
#
# Build packages on Debian or Ubuntu: build-essential flex bison bc libelf-dev
# libssl-dev curl xz-utils, and gcc-x86-64-linux-gnu or gcc-aarch64-linux-gnu
# for a cross build.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
FRAGMENT=$here/runplane.config
KERNEL_SERIES=${KERNEL_SERIES:-6.1}
KERNEL_VERSION=${KERNEL_VERSION:-}
FIRECRACKER_REF=${FIRECRACKER_REF:-v1.17.0}
MOBY_REF=${MOBY_REF:-master}
OUT=${OUT:-$here/out}
WORK=${WORK:-$here/.work}
JOBS=${JOBS:-$(nproc)}

main() {
	config_only=0
	arches=
	for arg in "$@"; do
		case $arg in
		--config-only) config_only=1 ;;
		x86_64 | aarch64) arches="$arches $arg" ;;
		*) die "unknown argument: $arg (use --config-only, x86_64, aarch64)" ;;
		esac
	done
	arches=${arches:-x86_64 aarch64}

	mkdir -p "$OUT" "$WORK"
	resolve_version
	fetch_source
	fetch "https://raw.githubusercontent.com/moby/moby/$MOBY_REF/contrib/check-config.sh" "$WORK/check-config.sh"
	for arch in $arches; do
		build "$arch" "$config_only"
	done
}

resolve_version() {
	if [ -z "$KERNEL_VERSION" ]; then
		series=$(printf '%s' "$KERNEL_SERIES" | sed 's/\./\\./g')
		KERNEL_VERSION=$(curl -fsSL https://www.kernel.org/releases.json |
			grep -o "\"version\": *\"$series\.[0-9]*\"" | grep -o '[0-9][0-9.]*' | sort -V | tail -n 1)
		[ -n "$KERNEL_VERSION" ] || die "kernel.org lists no $KERNEL_SERIES release. Set KERNEL_VERSION."
	fi
	case $KERNEL_VERSION in
	"$KERNEL_SERIES".*) ;;
	*) die "KERNEL_VERSION $KERNEL_VERSION is not in the series $KERNEL_SERIES" ;;
	esac
	say "Linux $KERNEL_VERSION, Firecracker config $FIRECRACKER_REF"
}

fetch_source() {
	SRC=$WORK/linux-$KERNEL_VERSION
	[ -f "$SRC/Makefile" ] && return 0
	base=https://cdn.kernel.org/pub/linux/kernel/v${KERNEL_VERSION%%.*}.x
	tarball=linux-$KERNEL_VERSION.tar.xz
	fetch "$base/$tarball" "$WORK/$tarball"
	fetch "$base/sha256sums.asc" "$WORK/sha256sums.asc"
	want=$(sed -n "s/^\([0-9a-f]\{64\}\)  $tarball\$/\1/p" "$WORK/sha256sums.asc")
	have=$(sha256sum "$WORK/$tarball" | cut -d ' ' -f 1)
	[ -n "$want" ] && [ "$want" = "$have" ] || die "SHA-256 of $tarball is $have, but kernel.org gives ${want:-nothing}"
	say "Extract $tarball"
	tar -xJf "$WORK/$tarball" -C "$WORK"
	rm "$WORK/$tarball"
}

# build makes the config for one arch, checks it, and builds the kernel.
build() {
	arch=$1
	case $arch in
	x86_64) karch=x86_64 target=vmlinux image=vmlinux name=vmlinux-x86_64 ;;
	aarch64) karch=arm64 target=Image image=arch/arm64/boot/Image name=vmlinux-arm64 ;;
	esac
	cross=
	[ "$(uname -m)" = "$arch" ] || cross=${CROSS_COMPILE:-$arch-linux-gnu-}
	obj=$WORK/build-$arch
	mkdir -p "$obj"

	say "== $name: config"
	base=$WORK/microvm-kernel-ci-$arch-$KERNEL_SERIES.config
	fetch "https://raw.githubusercontent.com/firecracker-microvm/firecracker/$FIRECRACKER_REF/resources/guest_configs/${base##*/}" "$base"
	"$SRC/scripts/kconfig/merge_config.sh" -m -O "$obj" "$base" "$FRAGMENT" >"$obj/merge.log"
	kmake olddefconfig
	check_fragment "$obj/.config"
	cp "$obj/.config" "$OUT/$name.config"

	# check-config.sh also examines the build host, so only the "Generally
	# Necessary" config flags count.
	report=$OUT/$name.check-config.txt
	NO_COLOR=1 sh "$WORK/check-config.sh" "$obj/.config" >"$report" 2>&1 || true
	missing=$(sed -n '/^Generally Necessary:/,/^Optional Features:/p' "$report" | grep -o 'CONFIG_[A-Z0-9_]*: missing' || true)
	[ -z "$missing" ] || die "$name misses options that Docker needs (see $report):
$missing"
	say "$name: all fragment options are set, and check-config.sh finds no missing necessary option"

	[ "$2" = 0 ] || return 0
	say "== $name: build"
	kmake -j"$JOBS" "$target"
	cp "$obj/$image" "$OUT/$name"
	say "$name: $(wc -c <"$OUT/$name") bytes"
}

kmake() {
	make -s -C "$SRC" O="$obj" ARCH="$karch" CROSS_COMPILE="$cross" \
		KBUILD_BUILD_USER=runplane KBUILD_BUILD_HOST=runplane "$@"
}

# check_fragment fails when "make olddefconfig" dropped an option of the
# fragment, for example because an option that it depends on is off.
check_fragment() {
	bad=
	while IFS= read -r line; do
		case $line in
		CONFIG_*=*)
			grep -qxF "$line" "$1" || bad="$bad
  want $line, have: $(grep -E "^(# )?${line%%=*}[= ]" "$1" || echo 'not set')"
			;;
		"# CONFIG_"*" is not set")
			option=${line#\# }
			option=${option%% *}
			! grep -q "^$option=" "$1" || bad="$bad
  want $line, have: $(grep "^$option=" "$1")"
			;;
		esac
	done <"$FRAGMENT"
	[ -z "$bad" ] || die "the config does not have these options of $FRAGMENT:$bad"
}

fetch() {
	curl -fsSL --retry 3 -o "$2" "$1" || die "download failed: $1"
}

say() { printf '%s\n' "$*"; }

die() {
	printf 'Error: %s\n' "$*" >&2
	exit 1
}

main "$@"
