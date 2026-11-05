#!/bin/sh
# End-to-end test of container mode or VM mode on a Linux host: the real
# install.sh, the job flow of e2e/dev_test.go against the installed runplane,
# the checks of the mode (e2e/container_test.go or e2e/vm_test.go), and the
# cleanup after a reboot. e2e/linux/lima.sh runs it in a Lima VM.
#
# Run it as root on a disposable Ubuntu 22.04 or 24.04 host. It removes an
# installed runplane and its data first.
#
#   sudo E2E_GITHUB=<host:port> sh e2e/linux/run.sh   # net, install, image, flow
#   sudo reboot
#   sudo sh e2e/linux/run.sh after-reboot uninstall fresh
#
# Steps (without arguments: net, install, image, flow):
#   net           test network: a public address for the fake GitHub, and a
#                 web server on addresses that sandboxes may not reach
#   install       install.sh --binary bin/runplane-linux-<arch> --mode <mode>,
#                 in VM mode also with --kernel
#   image         the sandbox image runplane/runner-ubuntu-24.04:local in Docker
#                 (VM mode builds its root disk from it)
#   flow          TestContainerFlow or TestVMFlow
#   after-reboot  TestContainerAfterReboot or TestVMAfterReboot
#   uninstall     install.sh --uninstall while a scratch disk is mounted and a
#                 TAP device exists, as runplane leaves them when it stops
#                 before its sandboxes
#   fresh         an install as on a new server, without the fake GitHub:
#                 install.sh from stdin, as "curl ... | sudo sh" gives it. Then
#                 the first admin with the printed setup code, as the dashboard
#                 makes it, runplane doctor, and the uninstall
#
# Inputs:
#   E2E_MODE       container (default) or vm; later steps read it from
#                  /etc/runplane/runplane.env
#   E2E_REBOOT     crash: the host lost power before after-reboot. Without it,
#                  the job that ran during the reboot must end as cancelled.
#   E2E_GITHUB     host:port of the fake GitHub (internal/github/fakegh/cmd/fakegh
#                  started with -runner-url http://198.51.100.2:9100). It must run
#                  outside this host, so that it keeps its state over the reboot.
#   E2E_IMAGE_TAR  docker save of runplane/runner-ubuntu-24.04:local (optional;
#                  without it, the image step builds images/ubuntu-24.04)
#   E2E_KERNEL     VM mode: the guest kernel (default kernel/out/vmlinux-<arch>
#                  of kernel/build.sh)
#   bin/runplane-linux-<arch>   make build-linux-<arch>
#   bin/e2e-linux-<arch>.test   GOOS=linux GOARCH=<arch> go test -c -tags e2e -o bin/e2e-linux-<arch>.test ./e2e
set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
BIN=$ROOT/bin
WORK=/var/lib/rp-e2e
LIB=/usr/local/lib/rp-e2e
NS=rpgh
# TEST-NET-2 is not in a range that the sandbox rules block, so it acts as
# the public internet. The fake GitHub answers on PUBLIC:9100.
HOST_ADDR=198.51.100.1
PUBLIC=198.51.100.2
# Each of these answers on port 80 from the host, and a sandbox may not
# reach any of them.
BLOCKED="169.254.169.254 168.63.129.16 10.99.0.2 172.16.99.2 192.168.99.2 100.64.99.2"
IMAGE=runplane/runner-ubuntu-24.04:local

case $(uname -m) in
x86_64) ARCH=amd64 KERNEL_NAME=vmlinux-x86_64 ;;
aarch64) ARCH=arm64 KERNEL_NAME=vmlinux-arm64 ;;
*) echo "unknown arch $(uname -m)" >&2 && exit 1 ;;
esac
MODE=${E2E_MODE:-$(sed -n 's/^RUNPLANE_MODE=//p' /etc/runplane/runplane.env 2>/dev/null || true)}
case ${MODE:=container} in
container) FLOW=TestContainerFlow AFTER=TestContainerAfterReboot ;;
vm) FLOW=TestVMFlow AFTER=TestVMAfterReboot ;;
*) echo "E2E_MODE must be container or vm" >&2 && exit 1 ;;
esac

main() {
	[ "$(id -u)" = 0 ] || die "run this script as root"
	[ $# -gt 0 ] || set -- net install image flow
	for step in "$@"; do
		case $step in
		net | install | image | flow | uninstall | fresh) "$step" ;;
		after-reboot) after_reboot ;;
		*) die "unknown step $step" ;;
		esac
	done
}

# net makes the network namespace $NS behind a veth pair, as units that
# come back after a reboot: the relay to the fake GitHub on $PUBLIC:9100 and
# a web server on port 80 of $PUBLIC and each $BLOCKED address.
net() {
	step "test network"
	[ -n "${E2E_GITHUB:-}" ] || die "set E2E_GITHUB to host:port of the fake GitHub"
	command -v socat >/dev/null || { apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -y -q socat; }
	mkdir -p "$LIB" "$WORK/canary"
	echo canary >"$WORK/canary/index.html"
	cat >"$LIB/net" <<EOF
#!/bin/sh
set -eu
case \$1 in
up)
	ip netns add $NS
	ip link add ${NS}0 type veth peer name eth0 netns $NS
	ip addr add $HOST_ADDR/24 dev ${NS}0
	ip link set ${NS}0 up
	ip -n $NS addr add $PUBLIC/24 dev eth0
	for a in $BLOCKED; do
		ip -n $NS addr add \$a/32 dev eth0
		ip route add \$a/32 via $PUBLIC
	done
	ip -n $NS link set lo up
	ip -n $NS link set eth0 up
	;;
down)
	ip link del ${NS}0 || true
	ip netns del $NS || true
	;;
esac
EOF
	chmod 0755 "$LIB/net"
	unit rp-e2e-net "Type=oneshot
RemainAfterExit=yes
ExecStart=$LIB/net up
ExecStop=$LIB/net down" ""
	# Two hops: the namespace has no route to the internet, and a host
	# address would be closed to sandboxes.
	unit rp-e2e-github "ExecStart=/bin/sh -c 'socat TCP-LISTEN:19100,bind=$HOST_ADDR,fork,reuseaddr TCP:$E2E_GITHUB & exec ip netns exec $NS socat TCP-LISTEN:9100,bind=$PUBLIC,fork,reuseaddr TCP:$HOST_ADDR:19100'
Restart=always" rp-e2e-net
	unit rp-e2e-canary "ExecStart=ip netns exec $NS python3 -m http.server 80 --bind 0.0.0.0 --directory $WORK/canary
Restart=always" rp-e2e-net
	systemctl daemon-reload
	systemctl restart rp-e2e-net rp-e2e-github rp-e2e-canary
	wait_for "the fake GitHub at $PUBLIC:9100" curl -fsS -o /dev/null "http://$PUBLIC:9100/_fake/jobs"
	for a in $PUBLIC $BLOCKED; do
		wait_for "the web server at $a" curl -fsS -o /dev/null "http://$a/"
	done
}

# unit writes and enables the service $1 with the [Service] lines $2,
# after the unit $3. At a shutdown, systemd stops it after runplane, so that
# the runners can report the end of their jobs.
unit() {
	deps=
	[ -z "$3" ] || deps="Requires=$3.service
After=$3.service"
	cat >"/etc/systemd/system/$1.service" <<EOF
[Unit]
Description=runplane end-to-end test: $1
Before=runplane.service
$deps

[Service]
$2

[Install]
WantedBy=multi-user.target
EOF
	systemctl enable "$1.service" >/dev/null 2>&1
}

install() {
	step "install runplane"
	if [ -f /etc/systemd/system/runplane.service ]; then
		sh "$ROOT/install.sh" --uninstall --yes
	fi
	rm -rf "$WORK/fakerunner" "$WORK/state.json"
	cp -R "$ROOT/testdata/fakerunner" "$WORK/fakerunner"
	# VM mode gets the fake runner as a release of the fake GitHub, so it
	# makes the runner drive as for a real release.
	runner=
	kernel=
	if [ "$MODE" = vm ]; then
		kernel="--kernel ${E2E_KERNEL:-$ROOT/kernel/out/$KERNEL_NAME}"
	else
		runner="Environment=RUNPLANE_DEV_RUNNER_DIR=$WORK/fakerunner"
	fi
	mkdir -p /etc/systemd/system/runplane.service.d
	cat >/etc/systemd/system/runplane.service.d/e2e.conf <<EOF
[Unit]
Wants=rp-e2e-github.service
After=rp-e2e-github.service

[Service]
Environment=RUNPLANE_GITHUB_API_URL=http://$PUBLIC:9100/api/v3
Environment=RUNPLANE_GITHUB_URL=http://$PUBLIC:9100
$runner
EOF
	# shellcheck disable=SC2086 # $kernel is a flag and its value, or nothing
	sh "$ROOT/install.sh" --binary "$BIN/runplane-linux-$ARCH" --mode "$MODE" $kernel --yes
}

image() {
	step "sandbox image $IMAGE"
	# VM mode makes its root disks from this image, and so it runs this step
	# before install.sh, which installs Docker.
	if ! command -v docker >/dev/null; then
		apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -y -q docker.io
	fi
	if docker image inspect "$IMAGE" >/dev/null 2>&1; then
		echo "$IMAGE exists"
	elif [ -n "${E2E_IMAGE_TAR:-}" ]; then
		docker load -i "$E2E_IMAGE_TAR"
	else
		docker build -t "$IMAGE" "$ROOT/images/ubuntu-24.04"
	fi
}

flow() {
	step "$FLOW"
	run_test "$FLOW"
}

after_reboot() {
	step "$AFTER"
	run_test "$AFTER"
}

uninstall() {
	step "uninstall"
	data=/var/lib/runplane
	disk=$data/sandboxes/01m6k00000000000000000e2e0
	mkdir -p "$disk/scratch"
	truncate -s 64M "$disk/scratch.img"
	mkfs.ext4 -q "$disk/scratch.img"
	mount -o loop "$disk/scratch.img" "$disk/scratch"
	ip tuntap add rp-9999 mode tap
	sh "$ROOT/install.sh" --uninstall --yes
	removed
	# A later install on this host is then a normal one, without the fake GitHub.
	rm -rf /etc/systemd/system/runplane.service.d/e2e.conf
	systemctl daemon-reload
}

fresh() {
	step "fresh install in $MODE mode"
	sh "$ROOT/install.sh" --uninstall --yes
	rm -rf /etc/systemd/system/runplane.service.d/e2e.conf
	systemctl daemon-reload
	kernel=
	[ "$MODE" != vm ] || kernel="--kernel ${E2E_KERNEL:-$ROOT/kernel/out/$KERNEL_NAME}"
	mkdir -p "$WORK"
	out=$WORK/fresh-install.log
	# shellcheck disable=SC2086 # $kernel is a flag and its value, or nothing
	sh -s -- --binary "$BIN/runplane-linux-$ARCH" --mode "$MODE" $kernel <"$ROOT/install.sh" >"$out" 2>&1 ||
		{ cat "$out"; die "install.sh failed"; }
	cat "$out"
	code=$(sed -n 's/^ *Setup code: *//p' "$out")
	[ -n "$code" ] || die "install.sh printed no setup code"

	url=http://127.0.0.1:8484
	curl -fsS "$url/setup" | grep -qi '<!doctype html' || die "the dashboard does not serve the setup page"
	# The setup page sends the code to the host check, then makes the admin.
	curl -fsS -o /dev/null "$url/api/setup/host?code=$code" || die "the host check does not accept the setup code $code"
	jar=$WORK/fresh-cookies
	curl -fsS -o /dev/null -c "$jar" -H 'X-Runplane-CSRF: 1' -H 'Content-Type: application/json' \
		-d "{\"code\":\"$code\",\"name\":\"Fresh Admin\",\"email\":\"fresh@example.com\",\"password\":\"fresh-password-1234\"}" \
		"$url/api/setup" || die "the setup with the code $code failed"
	curl -fsS -b "$jar" "$url/api/session" | grep -q 'fresh@example.com' || die "the new admin has no session"
	echo "the admin fresh@example.com exists and has a session"

	runplane doctor || die "runplane doctor found failures"
	sh "$ROOT/install.sh" --uninstall --yes
	removed
	rm -f "$out" "$jar"
}

# removed stops the test if a part of runplane stays after the uninstall.
removed() {
	data=/var/lib/runplane
	for path in "$data" /etc/runplane /usr/local/bin/runplane /etc/systemd/system/runplane.service; do
		[ ! -e "$path" ] || die "$path is left"
	done
	! pgrep -x runplane || die "a runplane process is left"
	! findmnt -rn -o TARGET | grep "^$data/" || die "mounts in $data are left"
	! losetup -l -n -O BACK-FILE | grep "^$data/" || die "loop devices of $data are left"
	! ip -o link show | grep ': rp-' || die "TAP devices are left"
	! docker ps -aq --filter label=dev.runplane.sandbox 2>/dev/null | grep . || die "sandbox containers are left"
	! id runplane-vm 2>/dev/null || die "the user runplane-vm is left"
	! grep -q '^runplane-vm:' /etc/subuid /etc/subgid 2>/dev/null || die "the ID range of runplane-vm is left"
	echo "nothing of runplane is left"
}

run_test() {
	test=$BIN/e2e-linux-$ARCH.test
	[ -x "$test" ] || die "$test is missing; build it: GOOS=linux GOARCH=$ARCH go test -c -tags e2e -o bin/e2e-linux-$ARCH.test ./e2e"
	E2E_FAKEGH=http://$PUBLIC:9100 E2E_STATE=$WORK/state.json "$test" -test.run "^$1\$" -test.v -test.count 1 -test.timeout 2h
}

wait_for() {
	what=$1
	shift
	i=0
	until "$@" 2>/dev/null; do
		i=$((i + 1))
		[ $i -lt 30 ] || die "$what does not answer"
		sleep 1
	done
}

step() { printf '\n==> %s\n' "$*"; }

die() {
	echo "Error: $*" >&2
	exit 1
}

main "$@"
