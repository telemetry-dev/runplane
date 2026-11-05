// Package fleet keeps the servers of the primary: its own server and the
// agents. It records their state and host stats, makes images and runner
// bundles ready on them, installs agents, and tells the scheduler which
// servers can take sandboxes.
//
// Fleet implements scheduler.Fleet, the api services Servers, InstallScript,
// HostChecker, and SettingsHook (Images through Fleet.Images), and the
// Bundles interface of the github package.
package fleet

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/agent"
	"github.com/runplane/runplane/internal/config"
	"github.com/runplane/runplane/internal/events"
	"github.com/runplane/runplane/internal/notify"
	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/store"
)

const (
	// An agent without a connection for this long is unreachable.
	unreachableAfter = 60 * time.Second
	syncEvery        = 5 * time.Second
	flushEvery       = 10 * time.Second
	factsEvery       = 10 * time.Minute
	provisionEvery   = time.Minute
	// A failed image pull or runner download waits this long before the
	// next try.
	retryAfter = 5 * time.Minute
	// A ready image with a tag is pulled again after this long, because the
	// builtin images get a new build each week.
	refreshEvery = 24 * time.Hour
	// callTimeout limits the short calls to a server: facts and settings.
	callTimeout = 30 * time.Second
	// updateTimeout limits an agent update: the agent downloads and checks
	// the release.
	updateTimeout = 10 * time.Minute
)

// Notifier sends an event to the notification channels that want it.
type Notifier interface {
	Notify(ctx context.Context, e notify.Event) error
}

type Config struct {
	Store  *store.Store
	Bus    *events.Bus
	Log    *slog.Logger
	Config config.Config
	// Local is the server of the primary itself. Required.
	Local agent.Node
	// AgentFingerprint is the fingerprint of the agent certificate. The
	// install commands and install.sh carry it.
	AgentFingerprint string
	// OnEvent gets each driver event of each server: the HandleEvent method
	// of the scheduler.
	OnEvent func(serverID string, ev sandbox.Event)
	// CapacityChanged runs when a server comes, goes, changes its status,
	// or gets its first image or runner: the CapacityChanged method of the
	// scheduler.
	CapacityChanged func()
	Notifier        Notifier         // optional
	Now             func() time.Time // nil: time.Now
}

// Fleet is the set of servers of the primary.
type Fleet struct {
	st       *store.Store
	bus      *events.Bus
	log      *slog.Logger
	cfg      config.Config
	local    agent.Node
	fp       string
	onEvent  func(string, sandbox.Event)
	changed  func()
	notifier Notifier
	now      func() time.Time

	// bg is the context of the work that outlives a call. Run cancels it.
	bg     context.Context
	stopBg context.CancelFunc
	bgMu   sync.Mutex
	wg     sync.WaitGroup
	wake   chan struct{} // asks Run to provision now

	keyMu sync.Mutex // the SSH key and known_hosts

	mu        sync.Mutex
	localID   string
	localAddr string               // IPv4 of the primary, for install commands
	nodes     map[string]*node     // connected servers by ID
	downSince map[string]time.Time // agents without a connection
	removed   map[string]bool      // servers that Remove disconnected
	installs  map[string]context.CancelFunc
	tokens    map[string]usedToken // recent token uses by hash
	images    map[string]store.Image
	defaultID string
	dropped   map[string]bool        // images that Images.Remove dropped
	updates   map[string]agentUpdate // the last update of each agent, by server ID
	latest    string                 // newest runner version
	minFree   float64                // min_free_disk_percent
	diskLow   map[string]bool
	samples   []store.ServerStat // host stats for the next flush
}

// node is a connected server.
type node struct {
	agent.Node
	id      string
	sess    *agent.Session // nil for the server of the primary
	arch    string
	addr    string // agents: the IP address that the primary sees
	version string // agents: the runplane version of the agent

	// Guarded by Fleet.mu.
	name         string
	status       string
	images       map[string]readyImage // by image ID
	pulls        map[string]string     // image ID: ref of the pull in progress
	failed       map[string]time.Time  // image ID: time of the last failed pull
	runner       string                // newest ready runner version
	runnerBusy   bool
	runnerFailed time.Time
}

type readyImage struct {
	ref   string // the ref of the image row at the pull
	image sandbox.ImageRef
	at    time.Time // when the pull ended
}

func New(c Config) *Fleet {
	bg, stop := context.WithCancel(context.Background())
	f := &Fleet{
		st:        c.Store,
		bus:       c.Bus,
		log:       cmp.Or(c.Log, slog.New(slog.DiscardHandler)),
		cfg:       c.Config,
		local:     c.Local,
		fp:        c.AgentFingerprint,
		onEvent:   c.OnEvent,
		changed:   c.CapacityChanged,
		notifier:  c.Notifier,
		now:       c.Now,
		bg:        bg,
		stopBg:    stop,
		wake:      make(chan struct{}, 1),
		nodes:     map[string]*node{},
		downSince: map[string]time.Time{},
		removed:   map[string]bool{},
		installs:  map[string]context.CancelFunc{},
		tokens:    map[string]usedToken{},
		images:    map[string]store.Image{},
		dropped:   map[string]bool{},
		updates:   map[string]agentUpdate{},
		diskLow:   map[string]bool{},
	}
	if f.now == nil {
		f.now = time.Now
	}
	return f
}

// Run records the server of the primary, then keeps the state of all
// servers until ctx ends: their status, facts, host stats, images, and
// runner bundles. Start it before the agent listener.
func (f *Fleet) Run(ctx context.Context) error {
	defer f.stop()
	st, err := f.st.GetSettings(ctx)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.minFree = st.MinFreeDiskPercent
	f.mu.Unlock()
	firstStart, err := f.seedImages(ctx)
	if err != nil {
		return err
	}
	if err := f.startLocal(ctx); err != nil {
		return err
	}
	if firstStart {
		if err := f.seedPool(ctx, st); err != nil {
			return err
		}
	}
	if err := f.startAgents(ctx); err != nil {
		return err
	}
	bus, cancel := f.bus.Subscribe(64)
	defer cancel()
	f.provision(ctx)

	check := time.NewTicker(syncEvery)
	defer check.Stop()
	flush := time.NewTicker(flushEvery)
	defer flush.Stop()
	provision := time.NewTicker(provisionEvery)
	defer provision.Stop()
	facts := time.NewTicker(factsEvery)
	defer facts.Stop()
	for {
		select {
		case <-ctx.Done():
			f.flushStats(context.WithoutCancel(ctx))
			return nil
		case e := <-bus:
			f.handleBus(e)
		case <-check.C:
			f.syncServers(ctx)
		case <-flush.C:
			f.flushStats(ctx)
		case <-provision.C:
			f.provision(ctx)
		case <-f.wake:
			f.provision(ctx)
		case <-facts.C:
			f.goBg(f.refreshFacts)
		}
	}
}

// stop ends the background work and waits for it.
func (f *Fleet) stop() {
	f.bgMu.Lock()
	f.stopBg()
	f.bgMu.Unlock()
	f.wg.Wait()
}

// goBg runs fn in the background until Run returns.
func (f *Fleet) goBg(fn func(ctx context.Context)) {
	f.bgMu.Lock()
	defer f.bgMu.Unlock()
	if f.bg.Err() != nil {
		return
	}
	f.wg.Go(func() { fn(f.bg) })
}

// kick asks Run to provision the servers soon.
func (f *Fleet) kick() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *Fleet) capacityChanged() {
	if f.changed != nil {
		f.changed()
	}
}

// Online returns the connected servers with the status online.
func (f *Fleet) Online() []sandbox.Host {
	f.mu.Lock()
	defer f.mu.Unlock()
	var hosts []sandbox.Host
	for _, n := range f.nodes {
		if n.status == store.ServerOnline {
			hosts = append(hosts, sandbox.Host{ServerID: n.id, Name: n.name, Arch: n.arch, Driver: n.Driver()})
		}
	}
	slices.SortFunc(hosts, func(a, b sandbox.Host) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ServerID, b.ServerID))
	})
	return hosts
}

// Driver returns the driver of a connected server, also a draining or
// disabled one, or nil.
func (f *Fleet) Driver(serverID string) sandbox.Driver {
	if n := f.node(serverID); n != nil {
		return n.Driver()
	}
	return nil
}

// Image returns the image for new sandboxes on the server if it is ready
// there. An empty imageID means the default image.
func (f *Fleet) Image(serverID, imageID string) (sandbox.ImageRef, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nodes[serverID]
	if n == nil {
		return sandbox.ImageRef{}, false
	}
	imageID = cmp.Or(imageID, f.defaultID)
	img, ok := f.images[imageID]
	r, ready := n.images[imageID]
	// A pull of an older ref of the image does not count.
	if !ok || !ready || r.ref != img.Ref {
		return sandbox.ImageRef{}, false
	}
	return r.image, true
}

// RunnerVersion returns the newest runner version that is ready on the server.
func (f *Fleet) RunnerVersion(serverID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := f.nodes[serverID]; n != nil && n.runner != "" {
		return n.runner, true
	}
	return "", false
}

func (f *Fleet) node(id string) *node {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nodes[id]
}

func (f *Fleet) connected() []*node {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := make([]*node, 0, len(f.nodes))
	for _, n := range f.nodes {
		list = append(list, n)
	}
	return list
}

// add makes n the connected node of its server and starts its pump.
func (f *Fleet) add(n *node) {
	n.images, n.pulls, n.failed = map[string]readyImage{}, map[string]string{}, map[string]time.Time{}
	f.mu.Lock()
	f.nodes[n.id] = n
	delete(f.downSince, n.id)
	f.mu.Unlock()
	f.goBg(func(ctx context.Context) { f.pump(ctx, n) })
}

// pump passes the driver events and the host stats of a node on. For an
// agent, it ends when the session ends.
func (f *Fleet) pump(ctx context.Context, n *node) {
	events, stats := n.Driver().Events(), n.Stats()
	for events != nil || stats != nil {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if f.onEvent != nil {
				f.onEvent(n.id, ev)
			}
		case s, ok := <-stats:
			if !ok {
				stats = nil
				continue
			}
			f.addStats(n, s)
		}
	}
	f.down(ctx, n)
}

// down drops the session of an agent that ended, unless a newer session
// of the server took its place.
func (f *Fleet) down(ctx context.Context, n *node) {
	now := f.now()
	f.mu.Lock()
	if f.nodes[n.id] != n {
		f.mu.Unlock()
		return
	}
	delete(f.nodes, n.id)
	f.downSince[n.id] = now
	name := n.name
	f.mu.Unlock()
	f.log.Info("server disconnected", "server_id", n.id, "name", name)
	if err := f.st.TouchServer(ctx, n.id, now); err != nil {
		f.log.Debug("record the last contact", "server_id", n.id, "err", err)
	}
	f.capacityChanged()
}

func (f *Fleet) publishServer(ctx context.Context, id string) {
	v, err := f.st.GetServer(ctx, id)
	if err != nil {
		f.log.Debug("read server for its event", "server_id", id, "err", err)
		return
	}
	f.bus.Publish(events.Event{Type: events.ServerState, Data: v})
}

// handleBus follows the changes that the API makes.
func (f *Fleet) handleBus(e events.Event) {
	switch e.Type {
	case events.ServerState:
		if v, ok := events.As[store.Server](e.Data); ok {
			f.serverChanged(v)
		}
	case events.ImageState:
		// The API sends the image row after a new image or a new default.
		if _, ok := events.As[store.Image](e.Data); ok {
			f.kick()
		}
	}
}

// serverChanged takes the name and the status of a server row for its
// connected node. A connected server is online, draining, or disabled; a
// row with another status is from before the connection.
func (f *Fleet) serverChanged(v store.Server) {
	f.mu.Lock()
	n := f.nodes[v.ID]
	changed := false
	if n != nil {
		n.name = v.Name
		if (v.Status == store.ServerOnline || adminStatus(v.Status)) && n.status != v.Status {
			n.status, changed = v.Status, true
		}
	}
	f.mu.Unlock()
	if changed {
		f.capacityChanged()
		f.kick()
	}
}

// notify sends an event to the notification channels in the background.
func (f *Fleet) notify(build func(dashboard string) notify.Event) {
	if f.notifier == nil {
		return
	}
	f.goBg(func(ctx context.Context) {
		st, err := f.st.GetSettings(ctx)
		if err != nil {
			f.log.Warn("read settings for a notification", "err", err)
		}
		e := build(st.PublicURL)
		if err := f.notifier.Notify(ctx, e); err != nil {
			f.log.Warn("send notification", "type", e.Type, "err", err)
		}
	})
}
