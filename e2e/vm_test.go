//go:build e2e && linux

package e2e

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/github/fakegh"
)

// TestVMFlow runs the job flow on this Linux host, where install.sh
// installed runplane in VM mode with Firecracker. It also checks what VM
// mode adds: Firecracker in the jailer and a systemd scope, the guest
// kernel, the scratch drive with the Docker data, the TAP network rules,
// the cache URL of the patched runner, and that each VM powers off by
// itself. Like TestContainerFlow, it ends with a job that runs while the
// host reboots. e2e/linux/run.sh runs it with E2E_MODE=vm.
func TestVMFlow(t *testing.T) {
	e := newInstalledEnv(t)
	if e.mode != "vm" {
		t.Skipf("runplane operates in %s mode, not in VM mode", e.mode)
	}
	f := &flow{env: e, defaultImage: true}
	f.connect()
	f.makePool()
	v := &vmFlow{&containerFlow{flow: f}}
	v.warmVM()
	first := f.runJob()
	f.rejectPublic()
	f.scaleOut()
	f.restartDuringJob()
	f.debugRerun(first)
	f.mirror()
	v.guest()
	v.dockerInSandbox()
	v.networkRules()
	v.actionsCache(`"$(sed -n 's/^ACTIONS_RESULTS_URL=//p' /home/runner/actions-runner/.env)"`)
	// The doctor of the service does the VM internet probe.
	v.defaultImage()
	f.dashboard()
	v.poweredOff()
	v.beforeReboot()
}

// TestVMAfterReboot checks that runplane removes the VMs that the reboot
// ended and operates again. Then it removes the pool, disconnects GitHub,
// and checks that no VM, scope, TAP, or chroot is left.
func TestVMAfterReboot(t *testing.T) {
	e := newInstalledEnv(t)
	st := readRebootState(t)
	f := &flow{env: e, poolID: st.PoolID, scaleSetID: st.ScaleSetID}
	v := &vmFlow{&containerFlow{flow: f}}
	v.afterReboot(st)
	f.teardown()
	v.poweredOff()
	v.checkNoVMs()
}

type vmFlow struct{ *containerFlow }

// warmVM checks that the warm sandbox is Firecracker in the systemd scope
// of the VM, with its own TAP device rp-<n> and its own uid and gid: the
// start of the runplane-vm range in /etc/subuid and /etc/subgid plus n. It
// also measures the memory of the idle VM.
func (v *vmFlow) warmVM() {
	v.t.Log("Firecracker and the idle VM")
	warm := v.waitIdle(nil)
	guest, err := netip.ParseAddr(warm.IP)
	if err != nil || !netip.MustParsePrefix("10.214.0.0/16").Contains(guest) {
		v.t.Fatalf("VM IP %q, want an address of the TAP range 10.214.0.0/16", warm.IP)
	}
	tap := tapOf(v.t, tapHost(guest))
	n, err := strconv.Atoi(strings.TrimPrefix(tap, "rp-"))
	if err != nil {
		v.t.Fatalf("the gateway %s of the VM is on %q, want a TAP device rp-<n>", tapHost(guest), tap)
	}
	uid := strconv.Itoa(subID(v.t, "/etc/subuid") + n)
	gid := strconv.Itoa(subID(v.t, "/etc/subgid") + n)
	for file, want := range map[string]string{"owner": uid, "group": gid} {
		if got, _ := os.ReadFile("/sys/class/net/" + tap + "/" + file); strings.TrimSpace(string(got)) != want {
			v.t.Errorf("TAP %s has %s %q, want the ID %s of the VM", tap, file, got, want)
		}
	}

	scope := "/sys/fs/cgroup/runplane.slice/runplane-vm-" + warm.ID + ".scope"
	procs, err := os.ReadFile(filepath.Join(scope, "cgroup.procs"))
	pids := strings.Fields(string(procs))
	if err != nil || len(pids) != 1 {
		v.t.Fatalf("processes of the VM scope %s: %q, %v; want Firecracker only", scope, procs, err)
	}
	comm, _ := os.ReadFile("/proc/" + pids[0] + "/comm")
	status, _ := os.ReadFile("/proc/" + pids[0] + "/status")
	ids := regexp.MustCompile(`(?m)^Uid:\s+` + uid + `\s[\s\S]*^Gid:\s+` + gid + `\s`)
	if strings.TrimSpace(string(comm)) != "firecracker" || !ids.Match(status) {
		v.t.Errorf("the process of the VM scope is %q with\n%s\nwant firecracker with uid %s and gid %s", comm, status, uid, gid)
	}

	v.bootPhases(warm)

	// The runner and dockerd settle after the runner starts to listen.
	time.Sleep(10 * time.Second)
	current, err := readUint(filepath.Join(scope, "memory.current"))
	if err != nil {
		v.t.Fatalf("idle VM memory: %v", err)
	}
	stat, err := os.ReadFile(filepath.Join(scope, "memory.stat"))
	if err != nil {
		v.t.Fatal(err)
	}
	v.measured("idle VM memory (1 vCPU, 1024 MB)", fmt.Sprintf("%d MiB (anon %d MiB, file %d MiB)",
		current>>20, memStat(stat, "anon")>>20, memStat(stat, "file")>>20))
}

// bootPhases measures the parts of a VM start from the times in its
// sandbox log: until Firecracker starts the VM, until runplane-init has its
// config, and until the runner listens.
func (v *vmFlow) bootPhases(b sandboxRow) {
	var text string
	v.must("GET", "/api/sandboxes/"+b.ID+"/log", nil, &text)
	var start, config, listening time.Time
	for _, line := range strings.Split(text, "\n") {
		stamp, rest, _ := strings.Cut(line, " ")
		t, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			continue
		}
		switch {
		case start.IsZero() && strings.HasPrefix(rest, "console ") && strings.Contains(rest, "InstanceStart"):
			start = t
		case config.IsZero() && strings.HasPrefix(rest, "init ") && strings.Contains(rest, "got the config"):
			config = t
		case listening.IsZero() && strings.HasPrefix(rest, "runner ") && strings.Contains(rest, "Listening for Jobs"):
			listening = t
		}
	}
	if start.IsZero() || config.IsZero() || listening.IsZero() {
		v.t.Logf("the sandbox log of %s lacks a boot step (start %v, config %v, listening %v):\n%s", b.ID, start, config, listening, text)
		return
	}
	v.measured("idle VM start: host, kernel and init, runner", fmt.Sprintf("%s, %s, %s",
		start.Sub(b.CreatedAt).Round(time.Millisecond), config.Sub(start).Round(time.Millisecond), listening.Sub(config).Round(time.Millisecond)))
}

// guest checks in a job that the VM runs the runplane kernel with
// runplane-init, the size of the pool, the scratch drive /dev/vdc with the
// Docker data on it, and the registry mirror and cache URL on its gateway.
func (v *vmFlow) guest() {
	v.t.Log("the guest")
	out := v.script("acme/api", "guest", `
fail=0
check() { if eval "$1"; then echo "PASS $2"; else echo "FAIL $2"; fail=1; fi; }
gw=$(ip -4 route show default | awk '{print $3; exit}')
echo "kernel $(uname -r), init $(cat /proc/1/comm), gateway $gw, $(nproc) vCPU, $(free -m | awk '/^Mem:/ {print $2}') MiB"
check '[ "$(uname -r | sed "s/.*-//")" = runplane ]' "the runplane guest kernel"
check '[ "$(cat /proc/1/comm)" = runplane-init ]' "runplane-init is PID 1"
check '[ "$(nproc)" = 1 ]' "1 vCPU"
mem=$(awk '/^MemTotal:/ {print int($2 / 1024)}' /proc/meminfo)
check '[ "$mem" -gt 900 ] && [ "$mem" -le 1024 ]' "about 1024 MiB memory ($mem)"
df -h / /mnt/scratch /var/lib/docker /tmp
check '[ "$(findmnt -n -o SOURCE --target /mnt/scratch)" = /dev/vdc ]' "/mnt/scratch is /dev/vdc"
size=$(df -B1G --output=size /mnt/scratch | tail -n 1 | tr -d ' ')
check '[ "$size" -ge 7 ] && [ "$size" -le 8 ]' "an 8G scratch drive (${size}G)"
dev() { stat -c %d "$1"; }
check '[ "$(dev /var/lib/docker)" = "$(dev /mnt/scratch)" ]' "/var/lib/docker is on the scratch drive"
check '[ "$(findmnt -n -o OPTIONS --target /opt/runplane/runner | cut -d, -f1)" = ro ]' "the runner drive is read-only"
check 'touch /home/runner/actions-runner/e2e-write' "the runner folder is writable"
check 'grep -q "^ACTIONS_RESULTS_URL=http://$gw:8486/" /home/runner/actions-runner/.env' "the runner .env has the cache URL on the gateway"
for i in $(seq 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
echo "dockerd: $(docker info -f '{{.DockerRootDir}} {{.Driver}} {{.RegistryConfig.Mirrors}}')"
check '[ "$(docker info -f "{{.Driver}}")" = overlay2 ]' "dockerd uses overlay2"
check 'docker info -f "{{.RegistryConfig.Mirrors}}" | grep -q "http://$gw:8486/"' "dockerd uses the registry mirror on the gateway"
before=$(df -B1 --output=used /mnt/scratch | tail -n 1)
docker pull -q alpine:3.20
after=$(df -B1 --output=used /mnt/scratch | tail -n 1)
echo "scratch used: $before -> $after bytes"
check '[ "$after" -gt "$before" ]' "the image pull wrote to the scratch drive"
exit $fail
`)
	v.t.Logf("guest job:\n%s", out)
}

// networkRules checks from a VM what it can reach: the internet, a public
// address, and the service port on its gateway, but no metadata service,
// private ranges, other VM, or other host port. Each blocked target
// answers from the host first, so a pass means a rule, not a missing
// server.
func (v *vmFlow) networkRules() {
	v.t.Log("network rules")
	// The other VM serves port 8000 during its job.
	req := v.fake.Queue(fakegh.JobSpec{
		Repo: "acme/api", Name: "listener", Labels: []string{label},
		Steps: []fakegh.Step{{Name: "Serve", Run: "timeout 300 python3 -m http.server 8000 --bind 0.0.0.0 >/dev/null 2>&1; true"}},
	})
	listener := v.waitJob("listener", 1, "running", 2*time.Minute)
	var other sandboxRow
	v.must("GET", "/api/sandboxes/"+listener.SandboxID, nil, &other)
	otherIP, err := netip.ParseAddr(other.IP)
	if err != nil {
		v.t.Fatalf("IP of VM %s: %q", other.ID, other.IP)
	}
	// All TAP host addresses are the same host, so the gateway of the
	// other VM shows what answers on a gateway.
	gw := tapHost(otherIP).String()
	lan := hostLANAddr(v.t)
	blocked := []struct{ addr, what string }{
		{net.JoinHostPort(other.IP, "8000"), "the other VM"},
		{hostTestNet + ":8484", "the dashboard on a public host address"},
		{net.JoinHostPort(lan, "8484"), "the dashboard on the host LAN address"},
		{gw + ":8486", "the service port on the gateway of the other VM"},
	}
	blocked = append(blocked, blockedTargets...)
	gwPorts := []struct{ port, what string }{
		{"8484", "the dashboard"}, {"8485", "the agent listener"}, {"2222", "the SSH gateway"}, {"22", "sshd"},
	}
	for _, b := range blocked {
		v.waitFor(b.addr+" answers the host", 30*time.Second, func() (bool, error) {
			err := dial(b.addr)
			return err == nil, err
		})
	}
	for _, p := range gwPorts {
		if err := dial(net.JoinHostPort(gw, p.port)); err != nil {
			v.t.Fatalf("%s does not answer the host on the gateway %s: %v", p.what, gw, err)
		}
	}

	var checks strings.Builder
	for _, b := range blocked {
		fmt.Fprintf(&checks, "no %s %q\n", b.addr, b.what)
	}
	for _, p := range gwPorts {
		fmt.Fprintf(&checks, "no $gw:%s %q\n", p.port, p.what+" on the gateway")
	}
	out := v.script("acme/api", "network rules", `
fail=0
gw=$(ip -4 route show default | awk '{print $3; exit}')
reach() { timeout 5 bash -c "exec 3<>/dev/tcp/${1%:*}/${1##*:}" 2>/dev/null; }
ok() { if reach "$1"; then echo "PASS reached $1 ($2)"; else echo "FAIL cannot reach $1 ($2)"; fail=1; fi; }
no() { if reach "$1"; then echo "FAIL reached $1 ($2)"; fail=1; else echo "PASS blocked $1 ($2)"; fi; }
ok $gw:8486 "the service port on the gateway"
ok `+publicAddr+`:80 "a public address"
`+checks.String()+`
if curl -fsS -m 20 -o /dev/null https://api.github.com/zen; then echo "PASS internet: https://api.github.com"; else echo "FAIL internet"; fail=1; fi
exit $fail
`)
	v.t.Logf("network job:\n%s", out)
	v.fake.(remoteFake).call("POST", fmt.Sprintf("/_fake/jobs/%d/cancel", req), nil, nil)
	listener = v.waitJob("listener", 1, "completed", 2*time.Minute)
	if listener.result() != "cancelled" {
		v.t.Errorf("the canceled listener job: %s, want cancelled", listener.result())
	}
}

var vmEnded = regexp.MustCompile(`msg="VM ended" .*sandbox=(\S+) .*killed=(\w+)`)

// poweredOff checks in the runplane log that each VM that ended in this
// test powered off: runplane-init stopped it, and Firecracker exited. A
// killed VM means that the power off did not end Firecracker.
func (v *vmFlow) poweredOff() {
	v.t.Helper()
	ended, killed := 0, []string{}
	for _, m := range vmEnded.FindAllStringSubmatch(v.log(), -1) {
		ended++
		if m[2] != "false" {
			killed = append(killed, m[1])
		}
	}
	if ended == 0 {
		v.t.Error("the runplane log has no ended VM")
	}
	if len(killed) > 0 {
		v.t.Errorf("runplane killed %d of %d VMs: %v; want each to power off", len(killed), ended, killed)
	}
	v.t.Logf("%d VMs powered off by themselves", ended)
}

// checkNoVMs fails the test if a Firecracker process, a VM scope, a TAP
// device, or a chroot is left after the last VM ended.
func (v *vmFlow) checkNoVMs() {
	v.t.Helper()
	scopes, _ := filepath.Glob("/sys/fs/cgroup/runplane.slice/runplane-vm-*.scope")
	jails, _ := filepath.Glob(filepath.Join(v.dataDir, "jail", "*", "*"))
	var taps []string
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if strings.HasPrefix(i.Name, "rp-") {
			taps = append(taps, i.Name)
		}
	}
	var procs []string
	comms, _ := filepath.Glob("/proc/[0-9]*/comm")
	for _, c := range comms {
		if b, _ := os.ReadFile(c); strings.TrimSpace(string(b)) == "firecracker" {
			procs = append(procs, filepath.Base(filepath.Dir(c)))
		}
	}
	for _, left := range []struct {
		what string
		list []string
	}{{"VM scopes", scopes}, {"chroots", jails}, {"TAP devices", taps}, {"Firecracker processes", procs}} {
		if len(left.list) > 0 {
			v.t.Errorf("%s are left: %v", left.what, left.list)
		}
	}
}

// tapHost is the host side of the /30 of a VM: the address below the
// guest address.
func tapHost(guest netip.Addr) netip.Addr { return guest.Prev() }

// tapOf returns the name of the interface that has the address.
func tapOf(t *testing.T, addr netip.Addr) string {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ifaces {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr() == addr {
				return i.Name
			}
		}
	}
	return ""
}

// subID returns the start of the runplane-vm range in a subuid or subgid
// file, which install.sh writes.
func subID(t *testing.T, path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(b)) {
		if f := strings.Split(strings.TrimSpace(line), ":"); len(f) == 3 && f[0] == "runplane-vm" {
			start, err := strconv.Atoi(f[1])
			if err != nil {
				t.Fatalf("%s: %q", path, line)
			}
			return start
		}
	}
	t.Fatalf("%s has no range for runplane-vm:\n%s", path, b)
	return 0
}
