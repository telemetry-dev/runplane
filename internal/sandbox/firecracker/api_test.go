package firecracker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type apiRequest struct {
	Method, Path string
	Body         any
}

// fakeAPI serves the Firecracker API on a Unix socket. It records each
// request and answers 400 with a fault message for the paths in fail.
func fakeAPI(t *testing.T, fail map[string]string) (*apiClient, func() []apiRequest) {
	t.Helper()
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []apiRequest
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body any
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("%s %s: body is not JSON: %q", r.Method, r.URL.Path, b)
		}
		mu.Lock()
		got = append(got, apiRequest{r.Method, r.URL.Path, body})
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
		return got
	}
}

// shortTempDir is a temp folder with a short path, because Unix socket
// paths have a limit of about 100 bytes.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rpfc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func jsonValue(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad JSON in test: %v: %s", err, s)
	}
	return v
}

func checkRequests(t *testing.T, got []apiRequest, want []apiRequest) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("got %d requests, want %d", len(got), len(want))
	}
	for i := range min(len(got), len(want)) {
		g, w := got[i], want[i]
		w.Body = jsonValue(t, w.Body.(string))
		if !reflect.DeepEqual(g, w) {
			gb, _ := json.Marshal(g.Body)
			wb, _ := json.Marshal(w.Body)
			t.Errorf("request %d:\n got %s %s %s\nwant %s %s %s", i, g.Method, g.Path, gb, w.Method, w.Path, wb)
		}
	}
}

func TestConfigureBootsTheVM(t *testing.T) {
	api, requests := fakeAPI(t, nil)
	m := machine{
		vcpu:          2,
		memoryMB:      4096,
		tap:           "rp-3",
		guest:         netip.MustParseAddr("10.214.0.14"),
		host:          netip.MustParseAddr("10.214.0.13"),
		bandwidthMbps: 100,
	}
	if err := configure(context.Background(), api, m); err != nil {
		t.Fatal(err)
	}
	limit := `{"bandwidth":{"size":12500000,"refill_time":1000}}`
	checkRequests(t, requests(), []apiRequest{
		{"PUT", "/boot-source", `{"kernel_image_path":"/vmlinux","boot_args":"console=ttyS0 quiet reboot=k panic=1 init=/sbin/runplane-init runplane.mode=vm ip=10.214.0.14::10.214.0.13:255.255.255.252::eth0:off random.trust_cpu=on"}`},
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

func TestConfigureWithoutBandwidthLimit(t *testing.T) {
	api, requests := fakeAPI(t, nil)
	m := machine{vcpu: 1, memoryMB: 512, tap: "rp-0", guest: netip.MustParseAddr("10.214.0.2"), host: netip.MustParseAddr("10.214.0.1")}
	if err := configure(context.Background(), api, m); err != nil {
		t.Fatal(err)
	}
	for _, r := range requests() {
		if r.Path == "/network-interfaces/eth0" {
			want := jsonValue(t, `{"iface_id":"eth0","host_dev_name":"rp-0","guest_mac":"06:00:0a:d6:00:02"}`)
			if !reflect.DeepEqual(r.Body, want) {
				t.Errorf("network interface %v, want %v", r.Body, want)
			}
			return
		}
	}
	t.Error("no network interface request")
}

func TestConfigureStopsAtTheFirstError(t *testing.T) {
	api, requests := fakeAPI(t, map[string]string{"/drives/runner": "Unable to open the block device"})
	m := machine{vcpu: 1, memoryMB: 512, tap: "rp-0", guest: netip.MustParseAddr("10.214.0.2"), host: netip.MustParseAddr("10.214.0.1")}
	err := configure(context.Background(), api, m)
	if err == nil || !strings.Contains(err.Error(), "Unable to open the block device") {
		t.Fatalf("got error %v, want the fault message", err)
	}
	for _, r := range requests() {
		if r.Path == "/actions" {
			t.Fatal("the VM booted after a failed request")
		}
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
