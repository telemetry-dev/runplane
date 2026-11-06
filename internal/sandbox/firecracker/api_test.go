package firecracker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/netrules"
)

type apiRequest struct{ method, path, body string }

// fakeAPI serves the Firecracker API on a Unix socket. It records each
// request and answers 400 with a fault message for the paths in fail.
func fakeAPI(t *testing.T, fail map[string]string) (*apiClient, func() []apiRequest) {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []apiRequest
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, apiRequest{r.Method, r.URL.Path, string(b)})
		mu.Unlock()
		if msg, ok := fail[r.URL.Path]; ok {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"fault_message": msg})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return newAPIClient(sock), func() []apiRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

func checkRequests(t *testing.T, got, want []apiRequest) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("got %d requests, want %d", len(got), len(want))
	}
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			t.Errorf("request %d:\n got %v\nwant %v", i, got[i], want[i])
		}
	}
}

var testLease = netrules.Lease{Tap: "rp-3", Host: netip.MustParseAddr("10.214.0.13"), Guest: netip.MustParseAddr("10.214.0.14")}

func TestBoot(t *testing.T) {
	api, requests := fakeAPI(t, nil)
	if err := api.boot(context.Background(), sandbox.Spec{VCPU: 2, MemoryMB: 4096}, testLease, 100); err != nil {
		t.Fatal(err)
	}
	got := requests()
	if len(got) == 0 || got[0].method != "PUT" || got[0].path != "/boot-source" {
		t.Fatalf("first request %v, want PUT /boot-source", got)
	}
	var boot struct {
		Kernel string `json:"kernel_image_path"`
		Args   string `json:"boot_args"`
	}
	if err := json.Unmarshal([]byte(got[0].body), &boot); err != nil {
		t.Fatal(err)
	}
	if boot.Kernel != "/vmlinux" {
		t.Errorf("kernel %q, want /vmlinux", boot.Kernel)
	}
	args := strings.Fields(boot.Args)
	for _, want := range []string{
		"console=ttyS0",            // the console FIFO reads the serial port
		"reboot=k",                 // the reboot of init ends Firecracker
		"panic=1",                  // the exit of init resets the VM
		"init=/sbin/runplane-init", // where the root disk has runplane-init
		"runplane.mode=vm",         // runplane-init reads it to find VM mode
		"ip=10.214.0.14::10.214.0.13:255.255.255.252::eth0:off", // the guest address of the lease, without DHCP
	} {
		if !slices.Contains(args, want) {
			t.Errorf("boot args %q have no %s", boot.Args, want)
		}
	}
	limit := `{"bandwidth":{"size":12500000,"refill_time":1000}}`
	checkRequests(t, got[1:], []apiRequest{
		// The order makes vda the root disk, vdb the runner, vdc scratch.
		{"PUT", "/drives/rootfs", `{"drive_id":"rootfs","path_on_host":"/rootfs.ext4","is_root_device":true,"is_read_only":true}`},
		{"PUT", "/drives/runner", `{"drive_id":"runner","path_on_host":"/runner.ext4","is_root_device":false,"is_read_only":true}`},
		{"PUT", "/drives/scratch", `{"drive_id":"scratch","path_on_host":"/scratch.ext4","is_root_device":false,"is_read_only":false}`},
		{"PUT", "/machine-config", `{"vcpu_count":2,"mem_size_mib":4096}`},
		{"PUT", "/network-interfaces/eth0", `{"iface_id":"eth0","host_dev_name":"rp-3","guest_mac":"06:00:0a:d6:00:0e","rx_rate_limiter":` + limit + `,"tx_rate_limiter":` + limit + `}`},
		{"PUT", "/vsock", `{"guest_cid":3,"uds_path":"/v.sock"}`},
		{"PUT", "/entropy", `{}`},
		{"PUT", "/actions", `{"action_type":"InstanceStart"}`},
	})
}

func TestBootWithoutBandwidthLimit(t *testing.T) {
	api, requests := fakeAPI(t, nil)
	if err := api.boot(context.Background(), sandbox.Spec{VCPU: 1, MemoryMB: 512}, testLease, 0); err != nil {
		t.Fatal(err)
	}
	got := requests()
	i := slices.IndexFunc(got, func(r apiRequest) bool { return r.path == "/network-interfaces/eth0" })
	if i < 0 {
		t.Fatal("no network interface request")
	}
	if want := `{"iface_id":"eth0","host_dev_name":"rp-3","guest_mac":"06:00:0a:d6:00:0e"}`; got[i].body != want {
		t.Errorf("network interface %s, want %s", got[i].body, want)
	}
}

func TestBootStopsAtTheFirstError(t *testing.T) {
	api, requests := fakeAPI(t, map[string]string{"/drives/runner": "Unable to open the block device"})
	err := api.boot(context.Background(), sandbox.Spec{VCPU: 1, MemoryMB: 512}, testLease, 0)
	if err == nil || !strings.Contains(err.Error(), "Unable to open the block device") {
		t.Fatalf("got error %v, want the fault message", err)
	}
	if slices.ContainsFunc(requests(), func(r apiRequest) bool { return r.path == "/actions" }) {
		t.Fatal("the VM booted after a failed request")
	}
}

func TestSetBandwidth(t *testing.T) {
	tests := []struct {
		mbps  int
		limit string
	}{
		{50, `{"bandwidth":{"size":6250000,"refill_time":1000}}`},
		// A bucket of size 0 turns the limiter off in the running VM.
		{0, `{"bandwidth":{"size":0,"refill_time":0}}`},
	}
	for _, tt := range tests {
		api, requests := fakeAPI(t, nil)
		if err := api.setBandwidth(context.Background(), tt.mbps); err != nil {
			t.Fatal(err)
		}
		checkRequests(t, requests(), []apiRequest{
			{"PATCH", "/network-interfaces/eth0", `{"iface_id":"eth0","rx_rate_limiter":` + tt.limit + `,"tx_rate_limiter":` + tt.limit + `}`},
		})
	}
}
