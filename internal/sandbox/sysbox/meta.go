package sysbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/runplane/runplane/internal/sandbox"
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
