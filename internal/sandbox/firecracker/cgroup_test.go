package firecracker

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
)

func TestMetricsOfBusyVMs(t *testing.T) {
	root := t.TempDir()
	d := testDriver(t, root, &fakeHost{})
	cgroup := func(id, cpu, current string) {
		dir := filepath.Join(root, "cgroup", "runplane.slice", "runplane-vm-"+id+".scope")
		writeFile(t, filepath.Join(dir, "cpu.stat"), "usage_usec "+cpu+"\nuser_usec 1\nsystem_usec 1\nnr_periods 0\n")
		writeFile(t, filepath.Join(dir, "memory.current"), current+"\n")
		writeFile(t, filepath.Join(dir, "memory.stat"), "anon 300000000\nfile 200000000\nactive_file 65782272\ninactive_file 134217728\n")
	}
	add := func(id string, vcpu int, state sandbox.State) {
		v := newVM(d, sandbox.Spec{ID: id, VCPU: vcpu}, d.taps.Lease(0), time.Now())
		v.State = state
		close(v.ready)
		d.vms[id] = v
	}
	add("busy", 2, sandbox.StateBusy)
	add("idle", 2, sandbox.StateIdle)
	cgroup("busy", "1000000", "536870912")
	cgroup("idle", "1000000", "536870912")

	t0 := time.Now()
	d.sample(t0) // the first reading has no CPU percent yet
	cgroup("busy", "6000000", "536870912")
	cgroup("idle", "6000000", "536870912")
	d.sample(t0.Add(5 * time.Second))

	ev := nextEvent(t, d)
	// 5 s of CPU time in 5 s on 2 vCPUs; 512 MiB minus 128 MiB inactive file cache.
	want := sandbox.Metrics{CPUPercent: 50, MemoryMB: 384}
	if ev.SandboxID != "busy" || ev.Type != sandbox.EventMetrics || *ev.Metrics != want {
		t.Fatalf("got %+v %+v, want metrics %+v of the busy VM", ev, ev.Metrics, want)
	}

	// The Firecracker threads can add a little CPU time above the vCPUs.
	cgroup("busy", "17000000", "536870912")
	d.sample(t0.Add(10 * time.Second))
	if ev := nextEvent(t, d); ev.SandboxID != "busy" || ev.Metrics.CPUPercent != 100 {
		t.Errorf("got %+v %+v, want CPU 100%%", ev, ev.Metrics)
	}
	select {
	case ev := <-d.Events():
		t.Errorf("unexpected event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}
