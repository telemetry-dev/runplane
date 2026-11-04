package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// tagIndex keeps the digest of each tag and the time of the last check with
// upstream. It writes each tag to a small file, so that a restart keeps the
// tags and a stale tag still works when upstream is down.
type tagIndex struct {
	dir string

	mu   sync.Mutex
	tags map[string]tagEntry // by "name:tag"
}

type tagEntry struct {
	Name    string    `json:"name"`
	Tag     string    `json:"tag"`
	Digest  string    `json:"digest"`
	Checked time.Time `json:"checked"`
}

func openTags(dir string) (*tagIndex, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("make tag folder: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read tag folder: %w", err)
	}
	t := &tagIndex{dir: dir, tags: map[string]tagEntry{}}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if !strings.HasSuffix(e.Name(), ".json") {
			os.Remove(path) // a temp file from a crash
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read tag: %w", err)
		}
		var te tagEntry
		// The next check with upstream writes a damaged file again.
		if json.Unmarshal(data, &te) == nil {
			t.tags[te.Name+":"+te.Tag] = te
		}
	}
	return t, nil
}

func (t *tagIndex) get(name, tag string) (tagEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	te, ok := t.tags[name+":"+tag]
	return te, ok
}

// put records that name:tag has the digest now.
func (t *tagIndex) put(name, tag, digest string) error {
	te := tagEntry{Name: name, Tag: tag, Digest: digest, Checked: time.Now().UTC()}
	data, err := json.Marshal(te)
	if err != nil {
		return err
	}
	key := name + ":" + tag
	t.mu.Lock()
	t.tags[key] = te
	t.mu.Unlock()

	// A hash for the file name, because a name can contain "/" and a file
	// for a name could collide with the folder for a longer name.
	sum := sha256.Sum256([]byte(key))
	f, err := os.CreateTemp(t.dir, "*.tmp")
	if err != nil {
		return fmt.Errorf("save tag: %w", err)
	}
	_, err = f.Write(data)
	if err := errors.Join(err, f.Close()); err != nil {
		return errors.Join(fmt.Errorf("save tag: %w", err), os.Remove(f.Name()))
	}
	if err := os.Rename(f.Name(), filepath.Join(t.dir, hex.EncodeToString(sum[:])+".json")); err != nil {
		return errors.Join(fmt.Errorf("save tag: %w", err), os.Remove(f.Name()))
	}
	return nil
}
