#!/bin/sh
# runplane installer for the primary server and for agents.
#
#   curl -fsSL https://<host>/install.sh | sudo sh -s -- [flags]
#
# Run "sh install.sh --help" for the flags.
set -eu

# Defaults. The primary sets these values when it serves this script: it puts
# NAME='value' lines after the first line. "make dist" sets the repository
# and the key in the release copy.
RUNPLANE_REPO=${RUNPLANE_REPO:-runplane/runplane}
RUNPLANE_VERSION=${RUNPLANE_VERSION:-latest}
RUNPLANE_PRIMARY=${RUNPLANE_PRIMARY:-}
RUNPLANE_FINGERPRINT=${RUNPLANE_FINGERPRINT:-}
# Base64 of the ed25519 key that signs SHA256SUMS. With a key, the script
# installs only release files with a valid signature.
RUNPLANE_PUBLIC_KEY=${RUNPLANE_PUBLIC_KEY:-}
# VM mode installs this Firecracker release, the one that the runplane tests
# use. The script downloads it directly and does not read the release list
# from api.github.com, which has a low rate limit without a token.
RUNPLANE_FIRECRACKER_VERSION=${RUNPLANE_FIRECRACKER_VERSION:-v1.17.0}

BIN=/usr/local/bin/runplane
CONFIG_DIR=/etc/runplane
DATA_DIR=/var/lib/runplane
ENV_FILE=$CONFIG_DIR/runplane.env
AGENT_FILE=$CONFIG_DIR/agent.json
KERNEL_FILE=$DATA_DIR/kernel/vmlinux
UNIT_DIR=/etc/systemd/system
VM_USER=runplane-vm
# The uids and gids of the VMs (reserve_vm_ids): one for each TAP address
# block of 10.214.0.0/16. The range starts after the container ranges of
# systemd (https://systemd.io/UIDS-GIDS/), in IDs that no tool allocates.
VM_ID_START=1879048192
VM_ID_COUNT=16384
SYSBOX_MIN=0.7.1

usage() {
	cat <<EOF
Install runplane: self-hosted GitHub Actions runners.

Usage: curl -fsSL <url>/install.sh | sudo sh -s -- [flags]

On a runplane agent, a run without --token updates the agent.

Flags:
  --mode auto|container|vm
                             Sandbox mode (default: auto, VM mode if /dev/kvm exists)
  --version <v>              Release to install (default: $RUNPLANE_VERSION)
  --repo <owner/name>        GitHub repository of the releases (default: $RUNPLANE_REPO)
  --listen <addr>            Dashboard address of the primary (default: :8484)
  --binary <path>            Install this runplane binary, not a release download
  --kernel <path>            Install this guest kernel (VM mode), not a release download
  --token <token>            Install an agent with this one-time server token
  --primary <host:port>      Agent: address of the agent listener of the primary
  --fingerprint <sha256:..>  Agent: certificate fingerprint of the primary
  --dry-run                  Show each action, but change nothing
  --uninstall                Stop and remove runplane
  --yes                      Answer yes to all questions
  --help                     Show this help
EOF
}

main() {
	parse_flags "$@"
	TMP=$(mktemp -d)
	trap 'rm -rf "$TMP"' EXIT
	trap 'exit 130' INT TERM

	if [ "$UNINSTALL" = 1 ]; then
		uninstall
		return
	fi

	check_host
	check_role
	select_mode
	fetch_runplane
	case $MODE in
	container) setup_container ;;
	vm)
		setup_vm
		check_base_image
		;;
	esac
	install_runplane
	write_config
	write_units
	store_mode
	start_service
	finish
}

parse_flags() {
	MODE=auto
	VERSION=$RUNPLANE_VERSION
	REPO=$RUNPLANE_REPO
	LISTEN=
	TOKEN=
	PRIMARY=$RUNPLANE_PRIMARY
	FINGERPRINT=$RUNPLANE_FINGERPRINT
	BINARY=
	KERNEL=
	DRY_RUN=0
	UNINSTALL=0
	YES=0
	DOCKER_BUILDS=0

	while [ $# -gt 0 ]; do
		flag=$1
		case $flag in
		--*=*)
			value=${flag#*=}
			flag=${flag%%=*}
			shift
			;;
		--mode | --version | --repo | --listen | --binary | --kernel | --token | --primary | --fingerprint)
			[ $# -ge 2 ] || die "$flag needs a value"
			value=$2
			shift 2
			;;
		*)
			value=
			shift
			;;
		esac
		case $flag in
		--mode) MODE=$value ;;
		--version) VERSION=$value ;;
		--repo) REPO=$value ;;
		--listen) LISTEN=$value ;;
		--binary) BINARY=$value ;;
		--kernel) KERNEL=$value ;;
		--token) TOKEN=$value ;;
		--primary) PRIMARY=$value ;;
		--fingerprint) FINGERPRINT=$value ;;
		--dry-run) DRY_RUN=1 ;;
		--uninstall) UNINSTALL=1 ;;
		--yes | -y) YES=1 ;;
		--help | -h)
			usage
			exit 0
			;;
		*) die "unknown argument: $flag (see --help)" ;;
		esac
	done

	case $MODE in
	auto) MODE_SET=0 ;;
	container | vm) MODE_SET=1 ;;
	*) die "--mode must be auto, container, or vm" ;;
	esac
	case $REPO in
	*/*) ;;
	*) die "--repo must be owner/name" ;;
	esac
	case $VERSION in
	[0-9]*) VERSION=v$VERSION ;;
	esac
	if [ -n "$BINARY" ] && [ ! -f "$BINARY" ]; then
		die "--binary: $BINARY does not exist"
	fi
	if [ -n "$KERNEL" ] && [ ! -f "$KERNEL" ]; then
		die "--kernel: $KERNEL does not exist"
	fi

	ROLE=primary
	SERVICE=runplane
	[ -n "$TOKEN" ] || return 0

	ROLE=agent
	SERVICE=runplane-agent
	[ -z "$LISTEN" ] || die "--listen is for the primary only"
	[ -n "$PRIMARY" ] || die "an agent needs --primary <host:port>"
	[ -n "$FINGERPRINT" ] || die "an agent needs --fingerprint <sha256:...>"
	# These values go into JSON without escapes, so permit only safe characters.
	only_chars "$TOKEN" 'A-Za-z0-9_-' || die "--token has characters that are not permitted"
	case $PRIMARY in
	*:[0-9]*) only_chars "$PRIMARY" 'A-Za-z0-9.:[]-' || die "--primary has characters that are not permitted" ;;
	*) die "--primary must be host:port" ;;
	esac
	hex=${FINGERPRINT#sha256:}
	if [ "$hex" = "$FINGERPRINT" ] || [ ${#hex} -ne 64 ] || ! only_chars "$hex" '0-9a-fA-F'; then
		die "--fingerprint must be sha256: and 64 hex digits"
	fi
}

# check_host makes sure that the host can operate runplane. It sets ARCH,
# OS_ID, OS_VERSION, and OS_CODENAME.
check_host() {
	step "Check the host"
	[ "$(id -u)" = 0 ] || fatal "run this script as root, for example: curl -fsSL <url>/install.sh | sudo sh"
	[ -d /run/systemd/system ] || fatal "runplane needs systemd, but systemd is not the init system of this host"
	[ "$(uname -s)" = Linux ] || die "runplane operates on Linux only"

	case $(uname -m) in
	x86_64 | amd64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	*) die "CPU architecture $(uname -m) is not supported. Use x86_64 or aarch64." ;;
	esac

	OS_ID=$(os_field ID)
	OS_VERSION=$(os_field VERSION_ID)
	OS_CODENAME=$(os_field UBUNTU_CODENAME)
	[ -n "$OS_CODENAME" ] || OS_CODENAME=$(os_field VERSION_CODENAME)
	say "OS: ${OS_ID:-unknown} $OS_VERSION, kernel $(uname -r), $ARCH"

	command -v curl >/dev/null 2>&1 && return 0
	if [ "$DRY_RUN" = 1 ] || ! command -v apt-get >/dev/null 2>&1; then
		die "install curl, then run this script again"
	fi
	apt_install curl ca-certificates
}

# check_role keeps the role of an installed server. A run without --token on
# an agent updates the agent: a primary next to it would take its ports and
# remove its sandboxes. An agent install on a primary stops.
check_role() {
	if [ "$ROLE" = agent ]; then
		[ ! -f "$UNIT_DIR/runplane.service" ] ||
			die "this server is a runplane primary. Install the agent on a different server, or remove the primary first (--uninstall)."
		return 0
	fi
	[ -f "$AGENT_FILE" ] || [ -f "$UNIT_DIR/runplane-agent.service" ] || return 0
	[ -z "$LISTEN" ] || die "this server is a runplane agent, and --listen is for the primary only"
	PRIMARY=$(sed -n 's/.*"primary": *"\([^"]*\)".*/\1/p' "$AGENT_FILE" 2>/dev/null | head -n 1)
	[ -n "$PRIMARY" ] ||
		die "this server is a runplane agent, but $AGENT_FILE has no primary. Run the agent install command from the Servers page of the dashboard."
	ROLE=agent
	SERVICE=runplane-agent
	say "This server is an agent of $PRIMARY. Update the agent and keep its registration."
	say "To make it a primary, remove the agent and its data first (--uninstall)."
}

# select_mode sets MODE to container or vm.
select_mode() {
	if [ "$MODE" = auto ]; then
		# A primary keeps its mode in the dashboard settings.
		current=
		from="the dashboard settings"
		[ "$ROLE" = agent ] || current=$(running_mode)
		if [ -z "$current" ] && [ -f "$ENV_FILE" ]; then
			current=$(sed -n 's/^RUNPLANE_MODE=//p' "$ENV_FILE" | tail -n 1)
			from=$ENV_FILE
		fi
		case $current in
		container | vm)
			MODE=$current
			say "Mode: $MODE (from $from)"
			;;
		esac
	fi
	if [ "$MODE" = auto ]; then
		if have_kvm; then
			MODE=vm
			say "Mode: vm (/dev/kvm exists, each job gets a Firecracker microVM)"
		else
			MODE=container
			say "Mode: container (no /dev/kvm, each job gets a Sysbox container)"
			say "      A host with KVM gives each job its own kernel (VM mode)."
		fi
	elif [ "$MODE" = vm ] && ! have_kvm; then
		fatal "VM mode needs KVM, but /dev/kvm does not exist. Use --mode container, or enable nested virtualization."
	fi

	case $OS_ID-$OS_VERSION in
	ubuntu-22.04 | ubuntu-24.04) ;;
	ubuntu-*) warn "runplane supports Ubuntu 22.04 and 24.04. Ubuntu $OS_VERSION is best effort." ;;
	debian-12 | debian-13)
		if [ "$MODE" = container ]; then
			warn "Sysbox does not support Debian. Container mode on Debian $OS_VERSION is best effort."
		else
			warn "runplane supports Ubuntu 22.04 and 24.04. Debian $OS_VERSION is best effort."
		fi
		;;
	*)
		[ "$MODE" = vm ] || die "container mode needs Ubuntu 22.04 or 24.04 (Debian 12 or 13 is best effort)"
		warn "runplane supports Ubuntu 22.04 and 24.04. ${OS_ID:-This OS} is best effort."
		;;
	esac
}

# have_kvm reports if /dev/kvm exists. If the CPU has virtualization but the
# kvm module is not loaded, it loads the module first.
have_kvm() {
	if [ ! -e /dev/kvm ] && grep -qwE 'vmx|svm' /proc/cpuinfo 2>/dev/null; then
		if grep -qw vmx /proc/cpuinfo; then
			run modprobe kvm_intel || true
		else
			run modprobe kvm_amd || true
		fi
	fi
	[ -c /dev/kvm ]
}

# fetch_runplane puts the binary from the release archive (or from --binary)
# at $TMP/runplane.
fetch_runplane() {
	step "Get runplane"
	if [ -n "$BINARY" ]; then
		say "Binary: $BINARY"
		run cp "$BINARY" "$TMP/runplane"
		return
	fi
	resolve_version
	asset=runplane_${VERSION#v}_linux_${ARCH}.tar.gz
	say "Release: $REPO $VERSION"
	fetch_sums
	download "$(release_url "$asset")" "$TMP/$asset"
	verify_sum "$TMP/$asset" "$(sum_for "$asset")"
	run tar -xzf "$TMP/$asset" -C "$TMP" runplane
}

# resolve_version changes VERSION from "latest" to the tag of the latest
# release.
resolve_version() {
	[ "$VERSION" = latest ] || return 0
	url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" 2>/dev/null) || url=
	case ${url##*/} in
	v*) VERSION=${url##*/} ;;
	*)
		# With --binary, only the guest kernel comes from a release.
		if [ -n "$BINARY" ]; then
			fatal "no release of $REPO found to get the guest kernel from. Use --kernel <path> (kernel/build.sh builds one) or --version."
		else
			fatal "no release of $REPO found. Use --version or --binary."
		fi
		;;
	esac
}

# fetch_sums downloads SHA256SUMS of the release. With RUNPLANE_PUBLIC_KEY,
# it checks the signature of the file, as "runplane update" does.
fetch_sums() {
	download "$(release_url SHA256SUMS)" "$TMP/SHA256SUMS"
	[ -n "$RUNPLANE_PUBLIC_KEY" ] || return 0
	download "$(release_url SHA256SUMS.sig)" "$TMP/SHA256SUMS.sig"
	command -v openssl >/dev/null 2>&1 || apt_install openssl
	[ "$DRY_RUN" = 0 ] || return 0
	# OpenSSL reads the key as PEM. "MCowBQYDK2VwAyEA" is the base64 of the
	# 12-byte DER header of an ed25519 public key, and the raw key follows it.
	printf '%s\n' '-----BEGIN PUBLIC KEY-----' "MCowBQYDK2VwAyEA$RUNPLANE_PUBLIC_KEY" '-----END PUBLIC KEY-----' >"$TMP/release-key.pem"
	openssl pkeyutl -verify -rawin -pubin -inkey "$TMP/release-key.pem" \
		-in "$TMP/SHA256SUMS" -sigfile "$TMP/SHA256SUMS.sig" >/dev/null ||
		die "SHA256SUMS of $REPO $VERSION has no valid signature of the release key. Do not install these files."
	say "Signature is correct: SHA256SUMS"
}

install_runplane() {
	step "Install runplane to $BIN"
	if [ -x "$BIN" ] && [ "$DRY_RUN" = 0 ]; then
		say "Replace $("$BIN" version 2>/dev/null || echo 'the installed version')"
	fi
	# A rename keeps the old file intact for the process that uses it.
	run install -m 0755 "$TMP/runplane" "$BIN.new"
	run mv -f "$BIN.new" "$BIN"
}

setup_container() {
	step "Container mode: Docker Engine and Sysbox"
	command -v apt-get >/dev/null 2>&1 || die "container mode needs apt-get (Ubuntu or Debian)"
	apt_install ca-certificates curl jq lsb-release
	ensure_docker
	if [ "$DRY_RUN" = 1 ] && ! command -v jq >/dev/null 2>&1; then
		say "+ (without jq, the dry run cannot show the Sysbox steps)"
		return 0
	fi
	ensure_sysbox
}

ensure_docker() {
	if command -v snap >/dev/null 2>&1 && snap list docker >/dev/null 2>&1; then
		die "Docker from the snap package does not operate with Sysbox. Remove it (snap remove docker), then run this script again."
	fi
	if command -v dockerd >/dev/null 2>&1; then
		say "Docker Engine: $(dockerd --version 2>/dev/null || echo found)"
		run systemctl enable --now docker.service
		return
	fi

	[ -n "$OS_CODENAME" ] || die "/etc/os-release gives no release code name"
	say "Install Docker Engine from download.docker.com"
	run install -m 0755 -d /etc/apt/keyrings
	download "https://download.docker.com/linux/$OS_ID/gpg" /etc/apt/keyrings/docker.asc
	run chmod a+r /etc/apt/keyrings/docker.asc
	write_file /etc/apt/sources.list.d/docker.sources 0644 <<EOF
Types: deb
URIs: https://download.docker.com/linux/$OS_ID
Suites: $OS_CODENAME
Components: stable
Architectures: $ARCH
Signed-By: /etc/apt/keyrings/docker.asc
EOF
	APT_UPDATED=0
	apt_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
	run systemctl enable --now docker.service
}

ensure_sysbox() {
	installed=$(dpkg-query -W -f='${Version}' sysbox-ce 2>/dev/null | sed -n 's/^\([0-9][0-9.]*[0-9]\).*/\1/p')
	if [ -n "$installed" ] && version_ge "$installed" "$SYSBOX_MIN"; then
		say "Sysbox: $installed"
		ensure_sysbox_runtime
		return
	fi
	if [ -n "$installed" ]; then
		warn "Sysbox $installed is too old. The upgrade stops all Sysbox containers."
	fi

	release=$(curl -fsSL -H 'Accept: application/vnd.github+json' \
		https://api.github.com/repos/nestybox/sysbox/releases/latest) ||
		die "cannot read the Sysbox releases from api.github.com"
	tag=$(printf '%s' "$release" | jq -r .tag_name)
	version_ge "${tag#v}" "$SYSBOX_MIN" || die "the latest Sysbox release is $tag, but runplane needs $SYSBOX_MIN or newer"
	asset=$(printf '%s' "$release" | jq -c --arg arch "$ARCH" \
		'first(.assets[] | select(.name | test("^sysbox-ce_.*linux_" + $arch + "\\.deb$")))')
	[ -n "$asset" ] || die "Sysbox $tag has no package for $ARCH"
	url=$(printf '%s' "$asset" | jq -r .browser_download_url)
	sum=$(printf '%s' "$asset" | jq -r '.digest // "" | sub("^sha256:"; "")')

	pin_docker_networks

	say "Install Sysbox $tag"
	deb=$TMP/${url##*/}
	download "$url" "$deb"
	verify_sum "$deb" "$sum"
	# apt reads local packages as the _apt user.
	run chmod 0755 "$TMP"
	run chmod 0644 "$deb"
	run env DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$deb"
	ensure_sysbox_runtime
}

# pin_docker_networks writes the bip and default-address-pools values that
# Docker uses now to daemon.json. Without them, the Sysbox package sets new
# values and Docker must restart, which stops the containers on this host.
pin_docker_networks() {
	command -v docker >/dev/null 2>&1 || return 0
	[ -n "$(docker ps -aq 2>/dev/null)" ] || return 0

	cfg=/etc/docker/daemon.json
	current='{}'
	if [ -s "$cfg" ]; then
		current=$(jq . "$cfg") || die "$cfg is not valid JSON"
	fi
	missing=$(printf '%s' "$current" | jq '(.bip // "") == "" or ((."default-address-pools" // []) | length) == 0')
	[ "$missing" = true ] || return 0

	bip=$(ip -4 -o addr show dev docker0 2>/dev/null | sed -n 's/.* inet \([0-9.]*\/[0-9]*\).*/\1/p' | head -n 1)
	pools=$(docker info --format '{{json .DefaultAddressPools}}' 2>/dev/null || true)
	case $pools in
	'' | null | '[]') pools=$(docker_default_pools) ;;
	*) pools=$(printf '%s' "$pools" | jq -c '[.[] | {base: .Base, size: .Size}]') ;;
	esac

	say "Containers exist on this host. Write their Docker networks to $cfg, so that Docker does not restart."
	printf '%s' "$current" | jq --arg bip "$bip" --argjson pools "$pools" '
		(if (.bip // "") == "" and $bip != "" then .bip = $bip else . end)
		| (if ((."default-address-pools" // []) | length) == 0 then ."default-address-pools" = $pools else . end)
	' | write_file "$cfg" 0644
}

# docker_default_pools prints the address pools of Docker when daemon.json
# has none.
docker_default_pools() {
	i=17
	pools=
	while [ $i -le 31 ]; do
		pools="$pools{\"base\":\"172.$i.0.0/16\",\"size\":16},"
		i=$((i + 1))
	done
	printf '[%s{"base":"192.168.0.0/16","size":20}]' "$pools"
}

ensure_sysbox_runtime() {
	run systemctl enable --now sysbox.service
	[ "$DRY_RUN" = 0 ] || return 0
	# "enable --now" returns 0 also when the start fails.
	systemctl is-active --quiet sysbox.service ||
		die "Sysbox does not operate. Examine: journalctl -u sysbox-mgr -u sysbox-fs"
	if ! docker info --format '{{json .Runtimes}}' | grep -q sysbox-runc; then
		systemctl reload docker.service
		sleep 2
	fi
	docker info --format '{{json .Runtimes}}' | grep -q sysbox-runc ||
		die "docker info does not show the sysbox-runc runtime. Examine: journalctl -u sysbox -u docker"
	say "Docker has the sysbox-runc runtime"
}

# check_base_image makes sure that VM mode can get the default image.
# runplane pulls it from ghcr.io/<repo>, or else builds it with Docker from
# the Dockerfile in its binary. Docker then builds images only. Jobs run in
# VMs.
check_base_image() {
	image=$(printf '%s' "$REPO" | tr '[:upper:]' '[:lower:]')/runner-ubuntu-24.04
	if ghcr_public "$image"; then
		say "Base image: ghcr.io/$image:latest"
		return 0
	fi
	say "Base image: ghcr.io/$image:latest cannot be pulled without a login, so runplane builds it with Docker."
	DOCKER_BUILDS=1
	if docker info >/dev/null 2>&1; then
		say "Docker Engine: $(docker version --format '{{.Server.Version}}' 2>/dev/null || echo found)"
		return 0
	fi
	case $OS_ID in
	ubuntu | debian) ;;
	*)
		warn "install Docker Engine for the image build, then restart runplane. The Docker Engine install of this script supports Ubuntu and Debian only."
		return 0
		;;
	esac
	say "Install Docker Engine. runplane uses it only to build images."
	ensure_docker
}

# ghcr_public reports if ghcr.io gives the manifest of <name>:latest without
# a login.
ghcr_public() {
	token=$(curl -fsS --connect-timeout 10 "https://ghcr.io/token?scope=repository:$1:pull" 2>/dev/null |
		sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
	[ -n "$token" ] || return 1
	curl -fsS -o /dev/null --connect-timeout 10 -H "Authorization: Bearer $token" \
		-H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
		"https://ghcr.io/v2/$1/manifests/latest" 2>/dev/null
}

# setup_vm installs Firecracker, the jailer, the guest kernel, and the jailer
# user with the ID range of the VMs.
setup_vm() {
	step "VM mode: Firecracker and the guest kernel"
	case $ARCH in
	amd64) fc_arch=x86_64 kernel_asset=vmlinux-x86_64 ;;
	arm64) fc_arch=aarch64 kernel_asset=vmlinux-arm64 ;;
	esac

	fc_tag=$RUNPLANE_FIRECRACKER_VERSION
	case $fc_tag in
	v[0-9]*.[0-9]*.[0-9]*) ;;
	*) die "RUNPLANE_FIRECRACKER_VERSION=$fc_tag is not a Firecracker release tag such as v1.17.0" ;;
	esac
	have=$(firecracker --version 2>/dev/null | sed -n '1s/^Firecracker \(v[0-9.]*\).*/\1/p' || true)
	if [ "$have" = "$fc_tag" ] && [ -x /usr/local/bin/jailer ]; then
		say "Firecracker: $fc_tag"
	else
		say "Install Firecracker $fc_tag"
		fc=firecracker-$fc_tag-$fc_arch
		url=https://github.com/firecracker-microvm/firecracker/releases/download/$fc_tag/$fc.tgz
		download "$url.sha256.txt" "$TMP/$fc.tgz.sha256.txt"
		download "$url" "$TMP/$fc.tgz"
		sum=
		[ "$DRY_RUN" = 1 ] || sum=$(cut -d ' ' -f 1 "$TMP/$fc.tgz.sha256.txt")
		verify_sum "$TMP/$fc.tgz" "$sum"
		run tar -xzf "$TMP/$fc.tgz" -C "$TMP"
		dir=$TMP/release-$fc_tag-$fc_arch
		run install -m 0755 "$dir/firecracker-$fc_tag-$fc_arch" /usr/local/bin/firecracker
		run install -m 0755 "$dir/jailer-$fc_tag-$fc_arch" /usr/local/bin/jailer
	fi

	run install -d -m 0700 "$DATA_DIR" "$(dirname "$KERNEL_FILE")"
	# The jailer makes device nodes in the VM chroots under the data folder
	# and runs a copy of Firecracker there.
	if [ "$DRY_RUN" = 0 ]; then
		options=$(findmnt -n -o OPTIONS --target "$DATA_DIR" 2>/dev/null || true)
		case ,$options, in
		*,nodev,* | *,noexec,*) die "$DATA_DIR is on a file system mounted with nodev or noexec ($options). VM mode needs both." ;;
		esac
	fi
	if [ -n "$KERNEL" ]; then
		say "Guest kernel: $KERNEL"
		run install -m 0644 "$KERNEL" "$KERNEL_FILE"
	else
		if [ -n "$BINARY" ]; then
			resolve_version
			fetch_sums
		fi
		say "Install the guest kernel $kernel_asset from $REPO $VERSION"
		download "$(release_url "$kernel_asset")" "$TMP/$kernel_asset"
		verify_sum "$TMP/$kernel_asset" "$(sum_for "$kernel_asset")"
		run install -m 0644 "$TMP/$kernel_asset" "$KERNEL_FILE"
	fi

	if ! id "$VM_USER" >/dev/null 2>&1; then
		run useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$VM_USER"
	fi
	reserve_vm_ids

	# VMs use TAP devices, and their traffic goes through the host. runplane
	# turns on IP forwarding when it starts, after Docker (write_units). A
	# sysctl file would turn it on before Docker, and Docker then keeps the
	# FORWARD policy ACCEPT.
	write_file /etc/modules-load.d/runplane.conf 0644 <<'EOF'
tun
EOF
	[ -c /dev/net/tun ] || run modprobe tun
}

# reserve_vm_ids gives the jailer user the ID range of the VMs in
# /etc/subuid and /etc/subgid. Firecracker of the VM with TAP rp-<n> runs
# with uid and gid VM_ID_START + n. A range that the user has stays.
reserve_vm_ids() {
	last=$((VM_ID_START + VM_ID_COUNT - 1))
	for kind in uid gid; do
		file=/etc/sub$kind
		if grep -q "^$VM_USER:" "$file" 2>/dev/null; then
			continue
		fi
		# With a shared ID, Firecracker could signal or trace a process of
		# a different owner.
		if [ -f "$file" ] && awk -F: -v a="$VM_ID_START" -v b="$last" \
			'$2 <= b && $2 + $3 > a { bad = 1 } END { exit !bad }' "$file"; then
			die "$file has a range in $VM_ID_START-$last, which runplane needs for its VMs"
		fi
		db=passwd
		[ "$kind" = uid ] || db=group
		if getent "$db" | awk -F: -v a="$VM_ID_START" -v b="$last" \
			'$3 >= a && $3 <= b { bad = 1 } END { exit !bad }'; then
			die "the $db database has a $kind in $VM_ID_START-$last, which runplane needs for its VMs"
		fi
		# usermod writes to the file only when it exists.
		[ -f "$file" ] || run install -m 0644 /dev/null "$file"
		run usermod "--add-sub${kind}s" "$VM_ID_START-$last" "$VM_USER"
	done
}

write_config() {
	step "Write the configuration"
	run install -d -m 0700 "$CONFIG_DIR" "$DATA_DIR"

	if [ -f "$ENV_FILE" ]; then
		env_set RUNPLANE_MODE "$MODE"
		[ -z "$LISTEN" ] || env_set RUNPLANE_LISTEN "$LISTEN"
	elif [ "$ROLE" = primary ]; then
		write_file "$ENV_FILE" 0600 <<EOF
# Environment of runplane.service. After a change: systemctl restart runplane
RUNPLANE_MODE=$MODE
RUNPLANE_LISTEN=${LISTEN:-:8484}
RUNPLANE_AGENT_LISTEN=:8485
RUNPLANE_SERVICE_PORT=8486
RUNPLANE_SSH_LISTEN=:2222
RUNPLANE_DATA_DIR=$DATA_DIR
RUNPLANE_CONFIG_DIR=$CONFIG_DIR
EOF
	else
		write_file "$ENV_FILE" 0600 <<EOF
# Environment of runplane-agent.service. After a change: systemctl restart runplane-agent
RUNPLANE_MODE=$MODE
RUNPLANE_SERVICE_PORT=8486
RUNPLANE_DATA_DIR=$DATA_DIR
RUNPLANE_CONFIG_DIR=$CONFIG_DIR
EOF
	fi

	[ "$ROLE" = agent ] || return 0
	# The token is for one use only. A registered agent keeps its server ID and
	# secret, so a second run of the same command, or a run without --token,
	# does not break it.
	if [ -z "$TOKEN" ] || { [ -f "$AGENT_FILE" ] && grep -q '"server_id": *"[^"]' "$AGENT_FILE" &&
		grep -q "\"primary\": *\"$PRIMARY\"" "$AGENT_FILE"; }; then
		say "Keep the registration in $AGENT_FILE"
		return 0
	fi
	write_file "$AGENT_FILE" 0600 <<EOF
{
  "primary": "$PRIMARY",
  "fingerprint": "$FINGERPRINT",
  "token": "$TOKEN"
}
EOF
}

write_units() {
	step "Write the systemd units"
	# Docker sets the FORWARD policy to DROP only when it turns on IP
	# forwarding itself. runplane turns on forwarding when it starts, so it
	# starts after Docker.
	deps='After=docker.service'
	# runplane connects to Docker at its start, also in VM mode for image
	# builds.
	if [ "$MODE" = container ] || [ "$DOCKER_BUILDS" = 1 ]; then
		deps="Wants=docker.service
$deps"
	fi
	if [ "$ROLE" = primary ]; then
		description='runplane: self-hosted GitHub Actions runners'
		command=serve
	else
		description="runplane agent for $PRIMARY"
		command=agent
	fi

	# KillMode=process: a restart stops only runplane. The sandboxes continue,
	# and runplane connects to them again when it starts.
	write_file "$UNIT_DIR/$SERVICE.service" 0644 <<EOF
[Unit]
Description=$description
Documentation=https://github.com/$REPO
Wants=network-online.target
After=network-online.target
$deps

[Service]
Type=simple
EnvironmentFile=$ENV_FILE
ExecStart=$BIN $command
Restart=always
RestartSec=3
KillMode=process
LimitNOFILE=1048576
TasksMax=infinity

[Install]
WantedBy=multi-user.target
EOF

	write_file "$UNIT_DIR/runplane.slice" 0644 <<'EOF'
[Unit]
Description=runplane sandboxes
Before=slices.target

[Slice]
# Sandboxes get less CPU and disk time than other services (default 100).
CPUWeight=20
IOWeight=20
EOF
}

# store_mode writes --mode into the settings of an installed primary,
# because the settings win over RUNPLANE_MODE after the first start.
store_mode() {
	if [ "$ROLE" != primary ] || [ "$MODE_SET" = 0 ] || [ ! -f "$DATA_DIR/runplane.db" ]; then
		return 0
	fi
	step "Write the mode to the settings"
	run with_env "$BIN" set-mode "$MODE" || die "runplane cannot write $MODE mode to its settings"
}

start_service() {
	step "Start $SERVICE"
	run systemctl daemon-reload
	run systemctl enable "$SERVICE.service"
	run systemctl restart "$SERVICE.service"
	[ "$DRY_RUN" = 0 ] || return 0

	i=0
	until systemctl is-active --quiet "$SERVICE.service"; do
		i=$((i + 1))
		if [ $i -ge 30 ]; then
			journalctl -u "$SERVICE.service" -n 30 --no-pager >&2 || true
			die "$SERVICE did not start. Examine: journalctl -u $SERVICE"
		fi
		sleep 1
	done
	[ "$ROLE" = primary ] || return 0
	i=0
	until curl -fsS -o /dev/null "$(local_url)/api/status" 2>/dev/null; do
		i=$((i + 1))
		if [ $i -ge 30 ]; then
			warn "the dashboard does not answer at $(local_url). Examine: journalctl -u $SERVICE"
			return 0
		fi
		sleep 1
	done
}

finish() {
	step "Run runplane doctor"
	if [ "$DRY_RUN" = 1 ]; then
		say "+ $BIN doctor"
	elif ! with_env "$BIN" doctor; then
		warn "runplane doctor found problems. Jobs can fail until you correct them."
	fi

	step "Done"
	if [ "$ROLE" = agent ]; then
		say "The runplane agent operates in $MODE mode and connects to $PRIMARY."
		say "The server shows on the Servers page of the dashboard."
		say "Logs: journalctl -u $SERVICE -f"
		return
	fi

	addr=$(listen_addr)
	port=${addr##*:}
	host=${addr%:*}
	ip=$(host_ip)
	case $host in
	'' | 0.0.0.0 | '[::]') host=$ip ;;
	esac
	mode=$MODE
	[ "$DRY_RUN" = 1 ] || mode=$(running_mode)
	say "runplane operates in ${mode:-$MODE} mode."
	if [ -n "$mode" ] && [ "$mode" != "$MODE" ]; then
		warn "the dashboard settings keep $mode mode. For $MODE mode, run this script again with --mode $MODE."
	fi
	say ""
	say "  Dashboard:  http://$host:$port"
	if [ "$DRY_RUN" = 1 ]; then
		say "  Setup code: (from runplane setup-code)"
	elif curl -fsS "$(local_url)/api/status" 2>/dev/null | grep -q '"needs_setup": *true'; then
		code=$(with_env "$BIN" setup-code 2>/dev/null) || code=
		if [ -n "$code" ]; then
			say "  Setup code: $code"
			say ""
			say "Open the dashboard and type the setup code to make the admin account."
		else
			warn "cannot get a setup code. Run: sudo runplane setup-code"
		fi
	fi
	case $host in
	10.* | 192.168.* | 172.1[6-9].* | 172.2[0-9].* | 172.3[01].*)
		say "This address is private. From a different network, use the public address of this server."
		;;
	esac
	say ""
	say "The dashboard uses HTTP. Set a domain in Settings > General to get HTTPS,"
	say "or connect through an SSH tunnel: ssh -L $port:localhost:$port <user>@$ip"
	say "Logs: journalctl -u $SERVICE -f"
}

uninstall() {
	step "Remove runplane"
	[ "$(id -u)" = 0 ] || fatal "run this script as root"

	for unit in runplane.service runplane-agent.service; do
		if [ -f "$UNIT_DIR/$unit" ]; then
			run systemctl disable --now "$unit"
		fi
	done

	say "Stop the sandboxes"
	if command -v docker >/dev/null 2>&1; then
		# docker prints the ID of each container and network that it removes.
		ids=$(docker ps -aq --filter label=dev.runplane.sandbox 2>/dev/null || true)
		if [ -n "$ids" ]; then
			# shellcheck disable=SC2086 # one argument for each container ID
			run docker rm -f $ids >/dev/null
		fi
		for net in $(docker network ls -q --filter label=dev.runplane.managed=true 2>/dev/null || true); do
			run docker network rm "$net" >/dev/null
		done
	fi
	remove_docker_rules
	# Each sandbox has a scratch disk on a loop device, mounted in the data
	# folder. rm cannot remove a mount point, and the loop device would stay.
	findmnt -rn -o TARGET 2>/dev/null | grep "^$DATA_DIR/" | sort -r | while IFS= read -r mnt; do
		run umount "$mnt" || run umount -l "$mnt"
	done
	if systemctl is-active --quiet runplane.slice 2>/dev/null; then
		run systemctl stop runplane.slice
	fi
	# Each VM has a persistent TAP device rp-<n>, which stays after its
	# Firecracker stops.
	for tap in $(ip -o link show 2>/dev/null | sed -n 's/^[0-9]*: \(rp-[0-9]*\)[:@].*/\1/p'); do
		run ip link del "$tap"
	done
	for table in "inet runplane" "bridge runplane"; do
		# shellcheck disable=SC2086 # family and name are two arguments
		if command -v nft >/dev/null 2>&1 && nft list table $table >/dev/null 2>&1; then
			run nft delete table $table
		fi
	done

	run rm -f "$UNIT_DIR/runplane.service" "$UNIT_DIR/runplane-agent.service" "$UNIT_DIR/runplane.slice" \
		/etc/modules-load.d/runplane.conf "$BIN"
	run systemctl daemon-reload
	if id "$VM_USER" >/dev/null 2>&1; then
		run userdel "$VM_USER"
	fi
	# userdel also removes the ID ranges of the user, but not all versions do.
	for file in /etc/subuid /etc/subgid; do
		if grep -q "^$VM_USER:" "$file" 2>/dev/null; then
			run sed -i "/^$VM_USER:/d" "$file"
		fi
	done

	if [ -d "$DATA_DIR" ] || [ -d "$CONFIG_DIR" ]; then
		if confirm "Remove all runplane data: $DATA_DIR (database, logs, caches) and $CONFIG_DIR (keys)?"; then
			run rm -rf "$DATA_DIR" "$CONFIG_DIR"
		else
			say "Kept $DATA_DIR and $CONFIG_DIR. A new install uses them again."
		fi
	fi

	step "Done"
	say "runplane is removed. Docker, Sysbox, and Firecracker stay installed."
	say "If GitHub shows runplane runner scale sets, remove them in the GitHub settings (Actions > Runners)."
}

# remove_docker_rules deletes the rules with the comment "runplane" from the
# INPUT, FORWARD, and DOCKER-USER chains. Docker and host firewalls use the
# nf_tables or the legacy backend, so look in both. A listing with
# iptables-legacy can load the legacy tables, so it runs only when they are
# loaded.
remove_docker_rules() {
	for ipt in iptables-nft iptables-legacy iptables; do
		command -v "$ipt" >/dev/null 2>&1 || continue
		if [ "$ipt" = iptables-legacy ] && ! grep -qx filter /proc/net/ip_tables_names 2>/dev/null; then
			continue
		fi
		for chain in INPUT FORWARD DOCKER-USER; do
			"$ipt" -S "$chain" 2>/dev/null | sed -n "s/^-A \($chain .*--comment runplane .*\)\$/-D \1/p" |
				while IFS= read -r rule; do
					set -f
					# shellcheck disable=SC2086 # one argument for each word of the rule
					run "$ipt" $rule || true
				done
		done
	done
}

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
warn() { printf 'Warning: %s\n' "$*" >&2; }

die() {
	printf 'Error: %s\n' "$*" >&2
	exit 1
}

# fatal stops the install. A dry run shows the problem and continues.
fatal() {
	if [ "$DRY_RUN" = 1 ]; then
		warn "$* (a real install stops here)"
		return 0
	fi
	die "$@"
}

# run does a command, or shows it in a dry run.
run() {
	if [ "$DRY_RUN" = 1 ]; then
		say "+ $*"
		return 0
	fi
	"$@"
}

# write_file writes stdin to the file $1 with the mode $2. The file appears
# complete or not at all.
write_file() {
	if [ "$DRY_RUN" = 1 ]; then
		say "+ write $1 (mode $2)"
		sed 's/^/    | /'
		return 0
	fi
	mkdir -p "$(dirname "$1")"
	tmp_file=$(mktemp "$1.XXXXXX")
	cat >"$tmp_file"
	chmod "$2" "$tmp_file"
	mv -f "$tmp_file" "$1"
}

env_set() {
	if [ "$DRY_RUN" = 1 ]; then
		say "+ set $1=$2 in $ENV_FILE"
		return 0
	fi
	if grep -q "^$1=" "$ENV_FILE"; then
		sed -i "s|^$1=.*|$1=$2|" "$ENV_FILE"
	else
		printf '%s=%s\n' "$1" "$2" >>"$ENV_FILE"
	fi
}

download() {
	if [ "$DRY_RUN" = 1 ]; then
		say "+ download $1"
		return 0
	fi
	curl -fsSL --retry 3 --connect-timeout 20 -o "$2" "$1" || die "download failed: $1"
}

release_url() {
	printf 'https://github.com/%s/releases/download/%s/%s' "$REPO" "$VERSION" "$1"
}

# sum_for prints the SHA-256 of a release asset from SHA256SUMS.
sum_for() {
	[ "$DRY_RUN" = 0 ] || return 0
	sed -n "s/^\([0-9a-f]\{64\}\)  \*\{0,1\}$1\$/\1/p" "$TMP/SHA256SUMS"
}

verify_sum() {
	[ "$DRY_RUN" = 0 ] || return 0
	[ -n "$2" ] || die "the release gives no SHA-256 for $(basename "$1")"
	actual=$(sha256sum "$1" | cut -d ' ' -f 1)
	[ "$actual" = "$2" ] || die "the SHA-256 of $(basename "$1") is $actual, but the release gives $2"
	say "SHA-256 is correct: $(basename "$1")"
}

APT_UPDATED=0
apt_install() {
	missing=
	for pkg in "$@"; do
		dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null | grep -q 'ok installed' || missing="$missing $pkg"
	done
	[ -n "$missing" ] || return 0
	if [ "$APT_UPDATED" = 0 ]; then
		run apt-get update -q
		APT_UPDATED=1
	fi
	# shellcheck disable=SC2086 # one argument for each package
	run env DEBIAN_FRONTEND=noninteractive apt-get install -y -q $missing
}

# version_ge reports if version $1 is $2 or newer.
version_ge() {
	[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n 1)" = "$2" ]
}

# only_chars reports if $1 has only the characters of the tr set $2.
only_chars() {
	[ -z "$(printf '%s' "$1" | tr -d "$2")" ]
}

os_field() {
	[ -r /etc/os-release ] || return 0
	sed -n "s/^$1=//p" /etc/os-release | tr -d "\"'"
}

confirm() {
	[ "$YES" = 0 ] || return 0
	if [ "$DRY_RUN" = 1 ]; then
		say "? $1 (a real run asks you)"
		return 1
	fi
	# When the script comes through a pipe, stdin is the script.
	(: </dev/tty) 2>/dev/null || return 1
	printf '%s [y/N] ' "$1" >/dev/tty
	read -r answer </dev/tty || answer=
	case $answer in
	[yY] | [yY][eE][sS]) return 0 ;;
	esac
	return 1
}

with_env() {
	(
		set -a
		# shellcheck disable=SC1090 # this script writes the file
		. "$ENV_FILE"
		set +a
		"$@"
	)
}

listen_addr() {
	addr=${LISTEN:-$(sed -n 's/^RUNPLANE_LISTEN=//p' "$ENV_FILE" 2>/dev/null | tail -n 1)}
	printf '%s' "${addr:-:8484}"
}

# local_url is the dashboard address for requests from this host.
local_url() {
	addr=$(listen_addr)
	host=${addr%:*}
	case $host in
	'' | 0.0.0.0 | '[::]') host=127.0.0.1 ;;
	esac
	printf 'http://%s:%s' "$host" "${addr##*:}"
}

# running_mode prints the sandbox mode of the primary that runs on this host,
# or nothing.
running_mode() {
	curl -fsS --max-time 5 "$(local_url)/api/status" 2>/dev/null | sed -n 's/.*"mode": *"\([a-z]*\)".*/\1/p'
}

# host_ip prints the address of the interface with the default route.
host_ip() {
	addr=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')
	[ -n "$addr" ] || addr=$(hostname -I 2>/dev/null | cut -d ' ' -f 1)
	printf '%s' "${addr:-<server-ip>}"
}

main "$@"
