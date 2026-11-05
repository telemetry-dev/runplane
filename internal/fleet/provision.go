package fleet

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/runplane/runplane/images"
	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/api"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/ids"
	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
	"github.com/runplane/runplane/internal/update"
)

// liveStates are the states of a sandbox that still operates.
var liveStates = []sandbox.State{sandbox.StateStarting, sandbox.StateIdle, sandbox.StateBusy, sandbox.StateStopping}

// seedImages adds the builtin images at the first start, and reports if
// this is the first start.
func (f *Fleet) seedImages(ctx context.Context) (bool, error) {
	list, err := f.st.ListImages(ctx)
	if err != nil || len(list) > 0 {
		return false, err
	}
	dirs, err := fs.ReadDir(images.FS, ".")
	if err != nil {
		return false, fmt.Errorf("list builtin images: %w", err)
	}
	now := f.now()
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		img := store.Image{
			ID: ids.New(), Name: d.Name(), Ref: "builtin:" + d.Name(), IsDefault: d.Name() == images.Default,
			Source: "builtin", CreatedAt: now, UpdatedAt: now,
		}
		if err := f.st.CreateImage(ctx, img); err != nil {
			return false, fmt.Errorf("add builtin image: %w", err)
		}
	}
	return true, nil
}

// seedPool adds the label "runplane" at the first start, so that a workflow
// can use runs-on: runplane at once. Its size fits the local server: 2 vCPU
// when the server has room, and memory shared by the runners that fit.
func (f *Fleet) seedPool(ctx context.Context, st store.Settings) error {
	local, err := f.st.GetLocalServer(ctx)
	if err != nil {
		return err
	}
	p := defaultPool(local.Arch, local.VCPU-st.ReserveVCPU, local.MemoryMB-st.ReserveMemoryMB)
	now := f.now()
	p.ID, p.CreatedAt, p.UpdatedAt = ids.New(), now, now
	if err := f.st.CreatePool(ctx, p); err != nil {
		return fmt.Errorf("add the default label: %w", err)
	}
	f.log.Info("added the default label", "label", p.Label, "vcpu", p.VCPU, "memory_mb", p.MemoryMB, "max_runners", p.MaxRunners)
	return nil
}

// defaultPool sizes the default label for the vCPU and memory that the
// server gives to sandboxes.
func defaultPool(arch string, vcpu, memoryMB int) store.Pool {
	size := min(2, max(1, vcpu))
	runners := max(1, vcpu/size)
	memory := min(8192, max(2048, memoryMB/runners/1024*1024))
	return store.Pool{
		Name: "Default", Label: "runplane", Arch: arch, VCPU: size, MemoryMB: memory, DiskGB: 40,
		MinIdle: 1, MaxRunners: runners, Enabled: true,
	}
}

// provision makes the default image, the images of the enabled pools, and
// the newest runner bundle ready on each online server. It starts the work
// in the background and does not start a pull or a download that is in
// progress or that failed a short time ago.
func (f *Fleet) provision(ctx context.Context) {
	imgs, err := f.st.ListImages(ctx)
	if err != nil {
		f.log.Warn("list images", "err", err)
		return
	}
	pools, err := f.st.ListPools(ctx)
	if err != nil {
		f.log.Warn("list pools", "err", err)
		return
	}
	bundles, err := f.st.ListRunnerBundles(ctx)
	if err != nil {
		f.log.Warn("list runner bundles", "err", err)
		return
	}
	type pull struct {
		n   *node
		img store.Image
	}
	type download struct {
		n        *node
		versions []string
	}
	var pulls []pull
	var downloads []download

	f.mu.Lock()
	f.images = map[string]store.Image{}
	f.defaultID = ""
	for _, img := range imgs {
		f.images[img.ID] = img
		if img.IsDefault {
			f.defaultID = img.ID
		}
	}
	if len(bundles) > 0 && update.Compare(bundles[0].Version, f.latest) > 0 {
		f.latest = bundles[0].Version
	}
	now := f.now()
	for _, n := range f.nodes {
		if n.status != store.ServerOnline {
			continue
		}
		for _, id := range f.wanted(n, pools) {
			img, ok := f.images[id]
			r, ready := n.images[id]
			fresh := ready && r.ref == img.Ref && (pinned(img.Ref) || now.Sub(r.at) < refreshEvery)
			if !ok || f.dropped[id] || fresh || n.pulls[id] != "" || now.Sub(n.failed[id]) < retryAfter {
				continue
			}
			n.pulls[id] = img.Ref
			pulls = append(pulls, pull{n, img})
		}
		if f.latest != "" && n.runner != f.latest && !n.runnerBusy && now.Sub(n.runnerFailed) >= retryAfter {
			n.runnerBusy = true
			downloads = append(downloads, download{n, runnerVersions(f.latest, n.arch, bundles)})
		}
	}
	f.mu.Unlock()

	for _, p := range pulls {
		f.goBg(func(ctx context.Context) { f.pull(ctx, p.n, p.img) })
	}
	for _, d := range downloads {
		f.goBg(func(ctx context.Context) {
			if err := f.ensureRunner(ctx, d.n, d.versions); err != nil {
				f.log.Warn("make runner ready", "server_id", d.n.id, "err", err)
			}
			f.mu.Lock()
			d.n.runnerBusy = false
			f.mu.Unlock()
		})
	}
}

// pinned tells if an image reference names a digest, which never changes.
func pinned(ref string) bool { return strings.Contains(ref, "@sha256:") }

// wanted returns the IDs of the images that the server needs: the default
// image and the images of the enabled pools that can use the server.
// f.mu must be held.
func (f *Fleet) wanted(n *node, pools []store.Pool) []string {
	var list []string
	if f.defaultID != "" {
		list = append(list, f.defaultID)
	}
	for _, p := range pools {
		if !p.Enabled || p.Arch != n.arch || len(p.ServerIDs) > 0 && !slices.Contains(p.ServerIDs, n.id) {
			continue
		}
		if id := p.ImageID; id != "" && !slices.Contains(list, id) {
			list = append(list, id)
		}
	}
	return list
}

// runnerVersions returns the versions to try for a server: the newest, then
// up to two older versions that are ready on other servers of the arch.
func runnerVersions(latest, arch string, bundles []store.RunnerBundle) []string {
	list := []string{latest}
	for _, b := range bundles { // newest first
		if len(list) == 3 {
			break
		}
		if b.Arch == arch && b.Status == store.BundleReady && update.Compare(b.Version, latest) < 0 {
			list = append(list, b.Version)
		}
	}
	return list
}

// pull makes an image ready on a server and records each step in the image
// build row of the server. A pull again of a ready image, to get a new
// build of its tag, keeps the row ready while it runs and when it fails:
// the server goes on with the image it has.
func (f *Fleet) pull(ctx context.Context, n *node, img store.Image) {
	defer func() {
		f.mu.Lock()
		if n.pulls[img.ID] == img.Ref {
			delete(n.pulls, img.ID)
		}
		f.mu.Unlock()
	}()
	f.mu.Lock()
	r, had := n.images[img.ID]
	refresh := had && r.ref == img.Ref
	f.mu.Unlock()
	b, err := f.st.GetImageBuild(ctx, img.ID, n.id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		f.log.Warn("read image build", "image", img.ID, "server_id", n.id, "err", err)
	}
	b.ImageID, b.ServerID = img.ID, n.id
	before := b.Digest
	if !refresh {
		f.saveBuild(ctx, &b, store.BuildPending, "")
	}
	var total int64
	ref, err := n.EnsureImage(ctx, img.Ref, func(p image.Progress) {
		total = max(total, p.Total)
		switch s := string(p.Status); s {
		case store.BuildPulling, store.BuildBuilding, store.BuildConverting:
			if s != b.Status && !refresh {
				f.saveBuild(ctx, &b, s, "")
			}
		}
	})
	if err != nil {
		f.mu.Lock()
		n.failed[img.ID] = f.now()
		f.mu.Unlock()
		f.log.Warn("make image ready", "image", img.Ref, "server_id", n.id, "err", err)
		if !refresh {
			f.saveBuild(ctx, &b, store.BuildFailed, err.Error())
		}
		return
	}
	if ref.Digest != before {
		b.Digest, b.SizeBytes, b.Path = ref.Digest, total, ""
	}
	f.mu.Lock()
	dropped := f.dropped[img.ID]
	f.mu.Unlock()
	if dropped {
		return
	}
	// The row says ready before the scheduler can use the image.
	f.saveBuild(ctx, &b, store.BuildReady, "")
	f.mu.Lock()
	r, had = n.images[img.ID]
	wasReady := had && r.ref == img.Ref
	if !f.dropped[img.ID] {
		n.images[img.ID] = readyImage{ref: img.Ref, image: ref, at: f.now()}
		delete(n.failed, img.ID)
	}
	f.mu.Unlock()
	if !wasReady {
		f.capacityChanged()
	}
	if before != "" && before != ref.Digest {
		f.gcImages(ctx, n, "")
	}
}

// saveBuild records the status of an image build and publishes it.
func (f *Fleet) saveBuild(ctx context.Context, b *store.ImageBuild, status, msg string) {
	b.Status, b.Error, b.UpdatedAt = status, msg, f.now()
	if err := f.st.UpsertImageBuild(ctx, *b); err != nil {
		f.log.Warn("record image build", "image", b.ImageID, "server_id", b.ServerID, "err", err)
		return
	}
	f.bus.Publish(events.Event{Type: events.ImageState, Data: *b})
}

// ensureRunner makes the first version in versions that works ready on the
// server. It returns the errors of the versions before it.
func (f *Fleet) ensureRunner(ctx context.Context, n *node, versions []string) error {
	var errs []error
	for i, v := range versions {
		b, err := n.EnsureRunner(ctx, v, n.arch)
		if err != nil {
			errs = append(errs, fmt.Errorf("server %s: %w", n.id, err))
			// A lost connection says nothing about the bundle.
			if ctx.Err() == nil && !errors.Is(err, agent.ErrClosed) {
				f.saveBundle(ctx, store.RunnerBundle{Version: v, Arch: n.arch, Status: store.BundleFailed, Error: err.Error()})
			}
			continue
		}
		f.saveBundle(ctx, store.RunnerBundle{
			Version: v, Arch: b.Arch, SHA256: b.SHA256, Patched: b.Patched, Status: store.BundleReady, Path: b.Dir,
		})
		f.mu.Lock()
		first := n.runner == ""
		if first || update.Compare(v, n.runner) > 0 {
			n.runner = v
		}
		if i == 0 {
			n.runnerFailed = time.Time{}
		} else {
			n.runnerFailed = f.now()
		}
		f.mu.Unlock()
		if first {
			f.capacityChanged()
		}
		if i == 0 {
			f.pruneRunners(ctx, n)
		}
		return errors.Join(errs...)
	}
	f.mu.Lock()
	n.runnerFailed = f.now()
	f.mu.Unlock()
	return errors.Join(errs...)
}

// saveBundle records a bundle build and publishes the row when its status
// or error changed.
func (f *Fleet) saveBundle(ctx context.Context, b store.RunnerBundle) {
	before, _ := f.st.GetRunnerBundle(ctx, b.Version, b.Arch)
	if err := f.st.SetRunnerBundleBuild(ctx, b); err != nil {
		f.log.Warn("record runner bundle", "version", b.Version, "arch", b.Arch, "err", err)
		return
	}
	after, err := f.st.GetRunnerBundle(ctx, b.Version, b.Arch)
	if err != nil || after.Status == before.Status && after.Error == before.Error {
		return
	}
	f.bus.Publish(events.Event{Type: events.RunnerVersion, Data: after})
}

// EnsureLatest makes the runner bundle of version ready on each online
// server. The github package calls it for a new runner release. Servers use
// the version for new sandboxes as soon as it is ready there.
func (f *Fleet) EnsureLatest(ctx context.Context, version string) error {
	f.mu.Lock()
	if update.Compare(version, f.latest) > 0 {
		f.latest = version
	}
	var list []*node
	for _, n := range f.nodes {
		if n.status == store.ServerOnline {
			list = append(list, n)
		}
	}
	f.mu.Unlock()
	errs := make([]error, len(list))
	var wg sync.WaitGroup
	for i, n := range list {
		wg.Go(func() { errs[i] = f.ensureRunner(ctx, n, []string{version}) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (f *Fleet) Images() api.Images { return imageService{f} }

type imageService struct{ f *Fleet }

// Refresh pulls the image again on each connected server. The servers keep
// the image that they have until the new pull is ready, unless its ref
// changed.
func (s imageService) Refresh(ctx context.Context, imageID string) error {
	f := s.f
	img, err := f.st.GetImage(ctx, imageID)
	if err != nil {
		return err
	}
	var list []*node
	f.mu.Lock()
	f.images[img.ID] = img
	if img.IsDefault {
		f.defaultID = img.ID
	}
	for _, n := range f.nodes {
		if n.pulls[img.ID] != img.Ref {
			n.pulls[img.ID] = img.Ref
			list = append(list, n)
		}
	}
	f.mu.Unlock()
	for _, n := range list {
		f.goBg(func(ctx context.Context) { f.pull(ctx, n, img) })
	}
	return nil
}

// Remove stops the use of an image and removes its files from the
// connected servers. The API then deletes the image row.
func (s imageService) Remove(ctx context.Context, imageID string) error {
	f := s.f
	f.mu.Lock()
	f.dropped[imageID] = true
	delete(f.images, imageID)
	var nodes []*node
	for _, n := range f.nodes {
		delete(n.images, imageID)
		delete(n.failed, imageID)
		nodes = append(nodes, n)
	}
	f.mu.Unlock()
	for _, n := range nodes {
		f.gcImages(ctx, n, imageID)
	}
	return nil
}

// gcImages removes the images on the server of n that no image row and no
// live sandbox uses. skip is an image whose build does not count.
func (f *Fleet) gcImages(ctx context.Context, n *node, skip string) {
	builds, err := f.st.ListImageBuilds(ctx)
	if err != nil {
		f.log.Warn("list image builds", "err", err)
		return
	}
	live, err := f.st.ListSandboxes(ctx, store.SandboxFilter{ServerID: n.id, States: liveStates})
	if err != nil {
		f.log.Warn("list sandboxes", "err", err)
		return
	}
	var keep []string
	for _, b := range builds {
		if b.ServerID == n.id && b.ImageID != skip && b.Digest != "" {
			keep = append(keep, b.Digest)
		}
	}
	for _, sb := range live {
		if sb.ImageDigest != "" {
			keep = append(keep, sb.ImageDigest)
		}
	}
	if err := n.GCImages(ctx, keep); err != nil {
		f.log.Warn("remove unused images", "server_id", n.id, "err", err)
	}
}

// pruneRunners removes the old runner bundles of a server, except the
// versions of its live sandboxes. An agent of an older release has no prune
// route; it keeps its bundles.
func (f *Fleet) pruneRunners(ctx context.Context, n *node) {
	live, err := f.st.ListSandboxes(ctx, store.SandboxFilter{ServerID: n.id, States: liveStates})
	if err != nil {
		f.log.Warn("list sandboxes", "err", err)
		return
	}
	var keep []string
	for _, sb := range live {
		if sb.RunnerVersion != "" && !slices.Contains(keep, sb.RunnerVersion) {
			keep = append(keep, sb.RunnerVersion)
		}
	}
	if err := n.PruneRunners(ctx, keep); err != nil {
		f.log.Warn("remove old runner bundles", "server_id", n.id, "err", err)
	}
}
