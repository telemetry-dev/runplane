package fleet

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runplane/runplane/internal/audit"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

func addImage(t *testing.T, st *store.Store, name, ref string, isDefault bool) string {
	t.Helper()
	now := time.Now()
	img := store.Image{ID: ids.New(), Name: name, Ref: ref, IsDefault: isDefault, Source: "registry", CreatedAt: now, UpdatedAt: now}
	if err := st.CreateImage(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	return img.ID
}

func addPool(t *testing.T, st *store.Store, label, arch, imageID string, enabled bool, serverIDs []string) {
	t.Helper()
	now := time.Now()
	p := store.Pool{
		ID: ids.New(), Name: label, Label: label, Arch: arch, VCPU: 2, MemoryMB: 4096, DiskGB: 20, ImageID: imageID,
		MaxRunners: 4, ServerIDs: serverIDs, Enabled: enabled, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreatePool(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func TestFirstStartHasDefaultImage(t *testing.T) {
	h := newHarness(t, newFakeNode("arm64"))
	h.run()
	waitFor(t, "the default image", func() bool {
		ref, ok := h.f.Image(h.localID(), "")
		return ok && ref.Ref == "builtin:ubuntu-24.04" && ref.Digest == "sha256:b0b0"
	})
}

// The default label of the first start is a pool change as any other: it
// has an audit entry, and the scheduler and GitHub get events.PoolsChanged.
func TestFirstStartAddsTheDefaultLabel(t *testing.T) {
	ctx := t.Context()
	h := newHarness(t, newFakeNode("arm64"))
	changed := events.Subscribe[events.PoolsChanged](ctx, h.bus)
	h.run()
	recv(t, changed)
	pools, err := h.st.ListPools(ctx)
	if err != nil || len(pools) != 1 || pools[0].Label != "runplane" || pools[0].Arch != "arm64" {
		t.Fatalf("pools %+v, %v; want the label runplane for arm64", pools, err)
	}
	// The pools service publishes the event before it records the entry.
	var entries []store.AuditEntry
	waitFor(t, "the audit entry", func() bool {
		entries, _, err = h.st.ListAudit(ctx, "", 10)
		return err == nil && len(entries) > 0
	})
	if len(entries) != 1 || entries[0].Action != "pool.create" || entries[0].Target != pools[0].ID || entries[0].UserID != "" {
		t.Errorf("audit entries %+v; want one pool.create by runplane", entries)
	}
}

func TestProvisionImagesAndRunner(t *testing.T) {
	ctx := t.Context()
	local := newFakeNode("arm64")
	local.set(func(n *fakeNode) {
		n.digests["registry.example.com/app:1"] = "sha256:a001"
		n.digests["registry.example.com/x64:1"] = "sha256:x001"
		n.digests["registry.example.com/off:1"] = "sha256:0ff1"
		n.digests["registry.example.com/other:1"] = "sha256:0711"
		n.runners["2.329.0"] = true
	})
	h := newHarness(t, local)
	addImage(t, h.st, "ubuntu-24.04", "builtin:ubuntu-24.04", true)
	app := addImage(t, h.st, "app", "registry.example.com/app:1", false)
	x64 := addImage(t, h.st, "x64", "registry.example.com/x64:1", false)
	off := addImage(t, h.st, "off", "registry.example.com/off:1", false)
	other := addImage(t, h.st, "other", "registry.example.com/other:1", false)
	broken := addImage(t, h.st, "broken", "registry.example.com/broken:1", false)
	addPool(t, h.st, "app", "arm64", app, true, nil)
	addPool(t, h.st, "x64", "x64", x64, true, nil)
	addPool(t, h.st, "off", "arm64", off, false, nil)
	addPool(t, h.st, "other", "arm64", other, true, []string{"01otherserver"})
	addPool(t, h.st, "broken", "arm64", broken, true, nil)
	// 2.330.0 is the newest release, but it does not download on this
	// server. 2.329.0 is ready on other servers.
	now := time.Now()
	if err := h.st.SetRunnerBundleBuild(ctx, store.RunnerBundle{Version: "2.329.0", Arch: "arm64", Status: store.BundleReady}, now); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetRunnerBundleDates(ctx, "2.330.0", "arm64", &now, nil, now); err != nil {
		t.Fatal(err)
	}
	h.run()
	id := h.localID()

	waitFor(t, "the failed pull", func() bool {
		b, err := h.st.GetImageBuild(ctx, broken, id)
		return err == nil && b.Status == store.BuildFailed
	})
	waitFor(t, "a runner", func() bool { _, ok := h.f.RunnerVersion(id); return ok })
	waitFor(t, "the default and the pool image", func() bool {
		_, def := h.f.Image(id, "")
		_, pool := h.f.Image(id, app)
		return def && pool
	})

	tests := []struct {
		name    string
		imageID string
		digest  string // "" = not ready
	}{
		{"default image", "", "sha256:b0b0"},
		{"image of an enabled pool", app, "sha256:a001"},
		{"image of an x64 pool", x64, ""},
		{"image of a disabled pool", off, ""},
		{"image of a pool for other servers", other, ""},
		{"image that fails", broken, ""},
	}
	for _, tt := range tests {
		ref, ok := h.f.Image(id, tt.imageID)
		if ok != (tt.digest != "") || ref.Digest != tt.digest {
			t.Errorf("%s: Image = %+v, %v; want digest %q", tt.name, ref, ok, tt.digest)
		}
	}
	if b, _ := h.st.GetImageBuild(ctx, broken, id); !strings.Contains(b.Error, "not found") {
		t.Errorf("failed build error %q, want the pull error", b.Error)
	}
	// The size is the space of the image on the server, not the bytes of
	// the download.
	if b, _ := h.st.GetImageBuild(ctx, app, id); b.Status != store.BuildReady || b.Digest != "sha256:a001" || b.SizeBytes != imageSize {
		t.Errorf("pool image build %+v, want ready with its digest and size", b)
	}

	if v, _ := h.f.RunnerVersion(id); v != "2.329.0" {
		t.Errorf("runner version %q while 2.330.0 fails, want the older ready 2.329.0", v)
	}
	if b, _ := h.st.GetRunnerBundle(ctx, "2.330.0", "arm64"); b.Status != store.BundleFailed || !strings.Contains(b.Error, "404") {
		t.Errorf("bundle 2.330.0: status %q, error %q; want failed with the download error", b.Status, b.Error)
	}

	// The server tries the newest version again after a wait.
	local.set(func(n *fakeNode) { n.runners["2.330.0"] = true })
	h.clock.add(retryAfter)
	h.f.provision(ctx)
	waitFor(t, "runner version 2.330.0", func() bool { v, _ := h.f.RunnerVersion(id); return v == "2.330.0" })
	b, _ := h.st.GetRunnerBundle(ctx, "2.330.0", "arm64")
	if b.Status != store.BundleReady || b.SHA256 != "9e2f2.330.0" || b.ReleasedAt == nil {
		t.Errorf("bundle 2.330.0 after the second try: %+v; want ready with its build and its release date", b)
	}

	// An agent where no runner downloads gets no runner version, and its
	// failure does not mark the bundles of the other server as failed.
	agentNode := newFakeNode("arm64")
	_, agentID := h.connectAgent("arm-2", agentNode)
	waitFor(t, "the runner downloads on the agent", func() bool { return len(agentNode.args("EnsureRunner")) == 2 })
	if got := agentNode.args("EnsureRunner"); !slices.Equal(got, []any{"2.330.0", "2.329.0"}) {
		t.Errorf("runner versions tried on the agent %v, want the newest, then the older ready one", got)
	}
	if v, ok := h.f.RunnerVersion(agentID); ok {
		t.Errorf("agent runner version %q, want none", v)
	}
	if b, _ := h.st.GetRunnerBundle(ctx, "2.330.0", "arm64"); b.Status != store.BundleReady {
		t.Errorf("bundle 2.330.0 status %q after a failure on the agent, want ready", b.Status)
	}
}

func TestRefreshAndRemoveImage(t *testing.T) {
	ctx := t.Context()
	local := newFakeNode("arm64")
	local.set(func(n *fakeNode) { n.digests["registry.example.com/app:1"] = "sha256:a001" })
	h := newHarness(t, local)
	addImage(t, h.st, "ubuntu-24.04", "builtin:ubuntu-24.04", true)
	app := addImage(t, h.st, "app", "registry.example.com/app:1", false)
	addPool(t, h.st, "app", "arm64", app, true, nil)
	h.run()
	id := h.localID()
	digest := func() string {
		ref, ok := h.f.Image(id, app)
		if !ok {
			return ""
		}
		return ref.Digest
	}
	waitFor(t, "the pool image", func() bool { return digest() == "sha256:a001" })

	// A new pull of the same ref keeps the image in use until it is ready.
	gate := make(chan struct{})
	local.set(func(n *fakeNode) {
		n.gates["registry.example.com/app:1"] = gate
		n.digests["registry.example.com/app:1"] = "sha256:a002"
	})
	if err := h.f.RefreshImage(ctx, audit.Actor{}, app); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second pull", func() bool { return count(local.args("EnsureImage"), "registry.example.com/app:1") == 2 })
	if got := digest(); got != "sha256:a001" {
		t.Errorf("digest during the pull %q, want the old sha256:a001", got)
	}
	close(gate)
	waitFor(t, "the new digest", func() bool { return digest() == "sha256:a002" })
	// The pull calls the GC after it makes the new digest ready.
	waitFor(t, "an image GC that drops sha256:a001", func() bool {
		return slices.ContainsFunc(local.args("GCImages"), func(keep any) bool { return !slices.Contains(keep.([]string), "sha256:a001") })
	})

	// After a new ref, the image of the old ref is not used.
	gate = make(chan struct{})
	local.set(func(n *fakeNode) {
		n.gates["registry.example.com/app:2"] = gate
		n.digests["registry.example.com/app:2"] = "sha256:b001"
	})
	if err := h.st.UpdateImage(ctx, store.Image{ID: app, Name: "app", Ref: "registry.example.com/app:2"}); err != nil {
		t.Fatal(err)
	}
	if err := h.f.RefreshImage(ctx, audit.Actor{}, app); err != nil {
		t.Fatal(err)
	}
	if got := digest(); got != "" {
		t.Errorf("digest %q while the new ref is pulled, want none", got)
	}
	close(gate)
	waitFor(t, "the image of the new ref", func() bool { return digest() == "sha256:b001" })

	// Remove keeps the files of the other images and of live sandboxes, on
	// the primary and on the agents.
	agentNode := newFakeNode("arm64")
	agentNode.set(func(n *fakeNode) { n.digests["registry.example.com/app:2"] = "sha256:b001" })
	_, agentID := h.connectAgent("arm-2", agentNode)
	waitFor(t, "the images on the agent", func() bool {
		_, def := h.f.Image(agentID, "")
		_, pool := h.f.Image(agentID, app)
		return def && pool
	})
	err := h.st.InsertSandbox(ctx, store.Sandbox{
		ID: ids.New(), PoolID: "p1", ServerID: id, Mode: sandbox.ModeContainer, State: sandbox.StateBusy,
		ImageDigest: "sha256:in-use", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	before, agentBefore := len(local.args("GCImages")), len(agentNode.args("GCImages"))
	if err := h.f.RemoveImage(ctx, audit.Actor{}, app); err != nil {
		t.Fatal(err)
	}
	if got := digest(); got != "" {
		t.Errorf("digest %q after Remove, want none", got)
	}
	if kept := local.args("GCImages")[before:]; !reflect.DeepEqual(kept, []any{[]string{"sha256:b0b0", "sha256:in-use"}}) {
		t.Errorf("image GC on the primary kept %v, want one GC that keeps sha256:b0b0 and sha256:in-use", kept)
	}
	if kept := agentNode.args("GCImages")[agentBefore:]; !reflect.DeepEqual(kept, []any{[]string{"sha256:b0b0"}}) {
		t.Errorf("image GC on the agent kept %v, want one GC that keeps sha256:b0b0", kept)
	}
}

func count(list []any, v any) int {
	n := 0
	for _, x := range list {
		if x == v {
			n++
		}
	}
	return n
}

// Each server gets each new runner release, so each server must also drop
// the old ones, not only the primary. A version that a live sandbox uses
// stays.
func TestNewRunnerPrunesOldBundlesOnAgents(t *testing.T) {
	ctx := t.Context()
	local := newFakeNode("arm64")
	local.set(func(n *fakeNode) { n.runners["2.330.0"], n.runners["2.331.0"] = true, true })
	h := newHarness(t, local)
	h.run()
	agentNode := newFakeNode("arm64")
	agentNode.set(func(n *fakeNode) { n.runners["2.330.0"], n.runners["2.331.0"] = true, true })
	_, agentID := h.connectAgent("arm-2", agentNode)
	err := h.st.InsertSandbox(ctx, store.Sandbox{ID: "01sandboxonagent0000000000", PoolID: "p1", ServerID: agentID,
		Mode: sandbox.ModeContainer, State: sandbox.StateBusy, RunnerName: "runplane-x", RunnerVersion: "2.328.0", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if err := h.st.SetRunnerBundleDates(ctx, "2.331.0", "arm64", &now, nil, now); err != nil {
		t.Fatal(err)
	}
	h.f.provision(ctx)
	waitFor(t, "a prune on the agent that keeps the live version", func() bool {
		return slices.ContainsFunc(agentNode.args("PruneRunners"), func(keep any) bool { return slices.Equal(keep.([]string), []string{"2.328.0"}) })
	})
	waitFor(t, "a prune on the primary", func() bool { return len(local.args("PruneRunners")) > 0 })
}

// The builtin images get a new build each week under the same tag, so a
// server pulls a tagged image again each day. The image stays ready while
// it does, also when the pull fails. A digest never changes.
func TestTaggedImagesArePulledAgainEachDay(t *testing.T) {
	ctx := t.Context()
	local := newFakeNode("arm64")
	pinnedRef := "registry.example.com/app@sha256:" + strings.Repeat("a", 64)
	local.set(func(n *fakeNode) { n.digests[pinnedRef] = "sha256:a001" })
	h := newHarness(t, local)
	def := addImage(t, h.st, "ubuntu-24.04", "builtin:ubuntu-24.04", true)
	app := addImage(t, h.st, "app", pinnedRef, false)
	addPool(t, h.st, "app", "arm64", app, true, nil)
	h.run()
	id := h.localID()
	waitFor(t, "both images", func() bool {
		_, d := h.f.Image(id, "")
		_, a := h.f.Image(id, app)
		return d && a
	})

	h.clock.add(23 * time.Hour)
	h.f.provision(ctx)
	// provision marks each pull that it starts until the pull ends.
	waitFor(t, "the end of the pulls", func() bool {
		h.f.mu.Lock()
		defer h.f.mu.Unlock()
		return len(h.f.nodes[id].pulls) == 0
	})
	if n := count(local.args("EnsureImage"), "builtin:ubuntu-24.04"); n != 1 {
		t.Errorf("%d pulls of the default image after 23 hours, want 1", n)
	}

	// The registry fails: the server keeps its image.
	local.set(func(n *fakeNode) { delete(n.digests, "builtin:ubuntu-24.04") })
	h.clock.add(2 * time.Hour)
	h.f.provision(ctx)
	waitFor(t, "a second pull that fails", func() bool {
		h.f.mu.Lock()
		defer h.f.mu.Unlock()
		return !h.f.nodes[id].failed[def].IsZero()
	})
	if _, ok := h.f.Image(id, ""); !ok {
		t.Error("the default image is not ready after a failed pull again")
	}
	if b, _ := h.st.GetImageBuild(ctx, def, id); b.Status != store.BuildReady {
		t.Errorf("default image build %q after a failed pull again, want ready", b.Status)
	}
	if n := count(local.args("EnsureImage"), pinnedRef); n != 1 {
		t.Errorf("%d pulls of the image with a digest, want 1", n)
	}
}

// The default label must fit the server it is made for; else a first job on
// a small VPS waits forever.
func TestDefaultPoolFitsTheServer(t *testing.T) {
	tests := []struct {
		name              string
		vcpu, memoryMB    int // after the host reserve
		wantVCPU, wantMem int
		wantMax           int
	}{
		{"12 threads and 64 GB", 11, 63488, 2, 8192, 5},
		{"8 vCPU and 16 GB", 7, 14336, 2, 4096, 3},
		{"2 vCPU and 4 GB", 1, 2048, 1, 2048, 1},
	}
	for _, tt := range tests {
		p := defaultPool("x64", tt.vcpu, tt.memoryMB)
		if p.Label != "runplane" || p.VCPU != tt.wantVCPU || p.MemoryMB != tt.wantMem || p.MaxRunners != tt.wantMax || p.MinIdle != 1 || !p.Enabled {
			t.Errorf("%s: got %s %d vCPU %d MB max %d idle %d enabled %v, want runplane %d vCPU %d MB max %d idle 1 enabled",
				tt.name, p.Label, p.VCPU, p.MemoryMB, p.MaxRunners, p.MinIdle, p.Enabled, tt.wantVCPU, tt.wantMem, tt.wantMax)
		}
	}
}
