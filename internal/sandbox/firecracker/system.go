package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/netrules"
)

// hostOps are the changes that the driver makes to the host network and
// to systemd.
type hostOps interface {
	setupNet(ctx context.Context, cfg netrules.Config) error
	// addTap makes the TAP of a lease, which only uid and gid can open,
	// and lets it through the firewall.
	addTap(l netrules.Lease, uid, gid int) error
	// keepTap lets the TAP of a VM from before a restart through again.
	keepTap(l netrules.Lease) error
	dropTap(l netrules.Lease) error
	killScope(unit string) error
	// stopJobs returns the VM scopes that systemd is about to stop, as at
	// a host shutdown.
	stopJobs() (map[string]bool, error)
	// probe fetches url with the network of a VM on lease l (Probe).
	probe(ctx context.Context, l netrules.Lease, dns netip.Addr, url string) error
	close() error
}

type systemHost struct{ net *netrules.Manager }

func (h systemHost) setupNet(ctx context.Context, cfg netrules.Config) error {
	return h.net.Setup(ctx, cfg)
}

func (h systemHost) addTap(l netrules.Lease, uid, gid int) error {
	if err := netrules.CreateTap(l.Tap, uid, gid, l.HostCIDR()); err != nil {
		return err
	}
	return h.net.AddTap(l.Tap, l.Guest, l.Host)
}

func (h systemHost) keepTap(l netrules.Lease) error { return h.net.AddTap(l.Tap, l.Guest, l.Host) }

func (h systemHost) dropTap(l netrules.Lease) error {
	return errors.Join(h.net.RemoveTap(l.Tap), netrules.DeleteTap(l.Tap))
}

func (h systemHost) killScope(unit string) error {
	out, err := exec.Command("systemctl", "kill", "--signal=SIGKILL", unit+".scope").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl kill %s: %w: %s", unit, err, bytes.TrimSpace(out))
	}
	return nil
}

func (h systemHost) stopJobs() (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "list-jobs", "--no-legend", unitName("*")+".scope").Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl list-jobs: %w", err)
	}
	return parseStopJobs(out), nil
}

// parseStopJobs reads the output of systemctl list-jobs: the lines
// "<job> <unit> <type> <state>".
func parseStopJobs(out []byte) map[string]bool {
	units := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		if f := strings.Fields(line); len(f) == 4 && f[2] == "stop" {
			units[f[1]] = true
		}
	}
	return units
}

func (h systemHost) close() error { return h.net.Close() }

// eventQueue delivers events in order without blocking the sender.
type eventQueue struct {
	out  chan sandbox.Event
	wake chan struct{}
	quit chan struct{}
	done chan struct{}

	mu   sync.Mutex
	list []sandbox.Event
}

func newEventQueue() *eventQueue {
	q := &eventQueue{
		out:  make(chan sandbox.Event),
		wake: make(chan struct{}, 1),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	go q.run()
	return q
}

func (q *eventQueue) push(ev sandbox.Event) {
	q.mu.Lock()
	q.list = append(q.list, ev)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *eventQueue) run() {
	defer close(q.done)
	defer close(q.out)
	for {
		q.mu.Lock()
		batch := q.list
		q.list = nil
		q.mu.Unlock()
		for _, ev := range batch {
			select {
			case q.out <- ev:
			case <-q.quit:
				return
			}
		}
		if len(batch) > 0 {
			continue
		}
		select {
		case <-q.wake:
		case <-q.quit:
			return
		}
	}
}

// close ends the delivery and closes the channel.
func (q *eventQueue) close() {
	close(q.quit)
	<-q.done
}
