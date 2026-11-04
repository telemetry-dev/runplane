#!/bin/sh
# Runs e2e/linux/run.sh on macOS in the Lima VM rp-e2e (Ubuntu 24.04, vz,
# nested virtualization), with the fake GitHub on the Mac, and reboots the VM
# between the flow test and the test after the reboot. It ends with the
# uninstall and a new install as on a new server. Then it stops the VM, and
# "limactl start rp-e2e" starts it again.
#
#   sh e2e/linux/lima.sh container            # container mode with Sysbox
#   sh e2e/linux/lima.sh vm                   # VM mode with Firecracker
#   E2E_REBOOT=crash sh e2e/linux/lima.sh vm  # power off the VM, as in a host crash
#
# The VM mounts the repository read-only. The base image comes from Docker on
# the Mac when it has runplane/runner-ubuntu-24.04:local; otherwise the VM
# builds it. VM mode uses the guest kernel kernel/out/vmlinux-arm64, and
# builds it in Docker on the Mac when it is missing.
set -eu
cd "$(dirname "$0")/../.."
VM=${E2E_VM:-rp-e2e}
PORT=${E2E_FAKEGH_PORT:-19100}
MODE=${1:-}
case $MODE in
container | vm) ;;
*) echo "usage: sh e2e/linux/lima.sh container|vm" >&2 && exit 2 ;;
esac
IMAGE=runplane/runner-ubuntu-24.04:local
REPO=$(pwd)

make build-linux-arm64
GOOS=linux GOARCH=arm64 go test -c -tags e2e -o bin/e2e-linux-arm64.test ./e2e
go build -o bin/fakegh ./internal/github/fakegh/cmd/fakegh
if [ "$MODE" = vm ] && [ ! -f kernel/out/vmlinux-arm64 ]; then
	mkdir -p kernel/out
	docker run --rm -v "$REPO:/src:ro" -v "$REPO/kernel/out:/out" -v rp-kernel-work:/work -e OUT=/out -e WORK=/work debian:13 sh -c '
		apt-get update -q >/dev/null &&
		DEBIAN_FRONTEND=noninteractive apt-get install -y -q build-essential flex bison bc libelf-dev libssl-dev curl xz-utils ca-certificates python3 >/dev/null &&
		sh /src/kernel/build.sh aarch64'
fi

if ! limactl list -q | grep -qx "$VM"; then
	limactl create --name "$VM" --vm-type vz --nested-virt --cpus 6 --memory 12 --disk 50 \
		--containerd none --mount-only "$REPO" --tty=false \
		--set '.portForwards = [{"guestIP":"0.0.0.0","guestIPMustBeZero":false,"proto":"any","ignore":true},{"guestIP":"127.0.0.1","proto":"any","ignore":true}]' \
		template:ubuntu-24.04
fi
limactl start --tty=false "$VM"
vm() { limactl shell --workdir "$REPO" "$VM" sudo env E2E_MODE="$MODE" E2E_REBOOT="${E2E_REBOOT:-}" "$@"; }
vm test -c /dev/kvm

# The fake GitHub runs here, so it keeps its state while the VM reboots. The
# VM reaches the Mac at host.lima.internal (192.168.5.2).
bin/fakegh -addr "127.0.0.1:$PORT" -runner-url http://198.51.100.2:9100 >bin/fakegh.log 2>&1 &
fake=$!
trap 'kill $fake 2>/dev/null' EXIT

vm env E2E_GITHUB="192.168.5.2:$PORT" sh e2e/linux/run.sh net
# Container mode gets Docker from install.sh. VM mode builds its root disk
# from the image in Docker, so the image comes before the install.
[ "$MODE" != container ] || vm sh e2e/linux/run.sh install
if docker image inspect "$IMAGE" >/dev/null 2>&1 && vm sh -c 'command -v docker' >/dev/null 2>&1 &&
	! vm docker image inspect "$IMAGE" >/dev/null 2>&1; then
	docker save "$IMAGE" | limactl shell "$VM" sudo docker load
fi
if [ "$MODE" = vm ]; then
	vm sh e2e/linux/run.sh image install flow
else
	vm sh e2e/linux/run.sh image flow
fi

if [ "${E2E_REBOOT:-}" = crash ]; then
	limactl stop --force "$VM"
else
	limactl stop "$VM"
fi
limactl start --tty=false "$VM"
vm sh e2e/linux/run.sh after-reboot uninstall fresh
limactl stop "$VM"
