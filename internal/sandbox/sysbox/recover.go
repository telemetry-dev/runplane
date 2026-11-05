package sysbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/control"
)

// meta is the file sandbox.json in the sandbox folder. A new driver reads it
// after a runplane restart. It never holds the JIT config.
type meta struct {
	Spec        sandbox.Spec  `json:"spec"`
	ContainerID string        `json:"container_id"`
	IP          string        `json:"ip"`
	State       sandbox.State `json:"state"`
	CreatedAt   time.Time     `json:"created_at"`
	HoldUntil   time.Time     `json:"hold_until,omitzero"`
	BusyAt      time.Time     `json:"busy_at,omitzero"`
}

const metaFile = "sandbox.json"

// recover takes over the sandbox containers that still operate after a
// runplane restart: it opens their control channels again, and the guests
// connect again within a second. It removes stopped containers and the
// folders, volumes, and probe containers that no sandbox uses.
func (d *Driver) recover(ctx context.Context) error {
	res, err := d.docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: d.owned(client.Filters{}.Add("label", sandbox.LabelSandbox)),
	})
	if err != nil {
		return fmt.Errorf("list sandbox containers: %w", err)
	}
	for _, c := range res.Items {
		id := c.Labels[sandbox.LabelSandbox]
		if c.State == container.StateRunning && idPattern.MatchString(id) {
			err := d.adopt(ctx, id, c.ID)
			if err == nil {
				continue
			}
			d.log.Warn("cannot take over sandbox, removing it", "sandbox", id, "err", err)
		}
		d.removeOrphan(ctx, c.ID, id)
	}

	entries, err := os.ReadDir(d.opts.Config.SandboxesDir())
	if err != nil {
		return fmt.Errorf("read sandboxes folder: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() && idPattern.MatchString(e.Name()) && d.get(e.Name()) == nil {
			d.removeFiles(ctx, e.Name())
		}
	}

	if d.opts.Scratch == ScratchVolume {
		vols, err := d.docker.VolumeList(ctx, client.VolumeListOptions{
			Filters: d.owned(client.Filters{}.Add("label", sandbox.LabelSandbox)),
		})
		if err != nil {
			return fmt.Errorf("list scratch volumes: %w", err)
		}
		for _, v := range vols.Items {
			if d.get(v.Labels[sandbox.LabelSandbox]) == nil {
				d.docker.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true})
			}
		}
	}

	probes, err := d.docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: d.owned(client.Filters{}.Add("label", labelProbe)),
	})
	if err != nil {
		return fmt.Errorf("list probe containers: %w", err)
	}
	for _, c := range probes.Items {
		d.docker.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true})
	}
	return nil
}

func (d *Driver) adopt(ctx context.Context, id, containerID string) error {
	dir := d.opts.Config.SandboxDir(id)
	m, err := readMeta(dir)
	if err != nil {
		return err
	}
	if m.ContainerID != containerID {
		return errors.New("the sandbox folder belongs to a different container")
	}
	b := &box{
		id:          id,
		dir:         dir,
		created:     m.CreatedAt,
		spec:        m.Spec,
		removed:     make(chan struct{}),
		containerID: containerID,
		ip:          m.IP,
		state:       m.State,
		holdUntil:   m.HoldUntil,
		busyAt:      m.BusyAt,
		// A running sandbox in the stopping state was in a Stop call.
		stopAsked: m.State == sandbox.StateStopping,
	}
	if b.logs, err = control.OpenLogFile(dir); err != nil {
		return err
	}
	if b.ctl, err = d.listen(b, nil); err != nil {
		b.logs.Close()
		return err
	}
	d.mu.Lock()
	d.boxes[id] = b
	d.mu.Unlock()
	d.log.Info("took over sandbox", "sandbox", id, "state", b.state)
	if b.state == sandbox.StateStopping {
		go d.stop(context.Background(), b)
	}

	// The bridge table usually has the binding of the port already.
	info, err := d.docker.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err == nil {
		err = d.bindPort(b, info.Container)
	}
	if err != nil {
		d.log.Warn("bind the bridge port of a sandbox", "sandbox", id, "err", err)
	}
	return nil
}

// save writes the meta file of b. Call it with d.mu held. Only a sandbox
// with a container needs one.
func (d *Driver) save(b *box) error {
	if b.containerID == "" || b.state.Done() {
		return nil
	}
	data, err := json.Marshal(meta{
		Spec:        b.spec,
		ContainerID: b.containerID,
		IP:          b.ip,
		State:       b.state,
		CreatedAt:   b.created,
		HoldUntil:   b.holdUntil,
		BusyAt:      b.busyAt,
	})
	if err != nil {
		return err
	}
	tmp := filepath.Join(b.dir, metaFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("save sandbox state: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(b.dir, metaFile)); err != nil {
		return fmt.Errorf("save sandbox state: %w", err)
	}
	return nil
}

func readMeta(dir string) (meta, error) {
	var m meta
	data, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		return m, fmt.Errorf("read sandbox state: %w", err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("read sandbox state: %w", err)
	}
	return m, nil
}
