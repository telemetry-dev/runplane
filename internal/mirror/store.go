package mirror

import (
	"container/list"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/runplane/runplane/internal/atomicfile"
	"github.com/runplane/runplane/internal/datadisk"
)

// Folders of the store. A file in them has the hex part of its sha256 digest
// as its name.
const (
	blobsDir     = "blobs"
	manifestsDir = "manifests"
)

// store keeps blobs and manifests as files named by their digest. When the
// files use more than the limit of the mirror, or the disk is low, it
// removes the least recently used ones.
type store struct {
	dir   string
	space func() datadisk.Space

	mu    sync.Mutex
	lru   *list.List               // of *storeFile, most recently used first
	files map[string]*list.Element // by key, for example "blobs/<hex>"
	size  int64
}

type storeFile struct {
	key  string
	size int64
}

func openStore(dir string, space func() datadisk.Space) (*store, error) {
	s := &store{dir: dir, space: space, lru: list.New(), files: map[string]*list.Element{}}
	if err := os.RemoveAll(filepath.Join(dir, "tmp")); err != nil {
		return nil, fmt.Errorf("remove temp files: %w", err)
	}
	for _, sub := range []string{"tmp", blobsDir, manifestsDir} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("make mirror folder: %w", err)
		}
	}

	type found struct {
		key  string
		size int64
		used time.Time
	}
	var all []found
	for _, sub := range []string{blobsDir, manifestsDir} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			return nil, fmt.Errorf("read mirror folder: %w", err)
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			all = append(all, found{sub + "/" + e.Name(), info.Size(), info.ModTime()})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].used.Before(all[j].used) })
	for _, f := range all {
		s.files[f.key] = s.lru.PushFront(&storeFile{f.key, f.size})
		s.size += f.size
	}
	if err := s.evict(space()); err != nil {
		return nil, err
	}
	return s, nil
}

// open opens the file for key and marks it as used. It returns nil if the
// store does not have the file.
func (s *store) open(key string) *os.File {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.files[key]
	if !ok {
		return nil
	}
	path := s.path(key)
	f, err := os.Open(path)
	if err != nil {
		s.drop(el)
		return nil
	}
	s.lru.MoveToFront(el)
	// The modification time keeps the order of use after a restart. If it
	// fails, only that order is lost.
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return f
}

// temp makes the temp file for key on the file system of the store.
func (s *store) temp(key string) (*atomicfile.File, error) {
	return atomicfile.CreateIn(filepath.Join(s.dir, "tmp"), s.path(key), 0o600)
}

// add moves the complete temp file f of key into the store. Then it removes
// the least recently used files over the limit. It does not keep a file that
// is larger than the limit, or a file while the disk is low; that temp file
// goes at its Close.
func (s *store) add(key string, f *atomicfile.File, size int64) error {
	space := s.space()
	if space.Mirror > 0 && size > space.Mirror || space.Low() {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.evict(space)
	}
	// A sync before the lock, so that Commit has no data left to write
	// while it holds the lock.
	if err := f.Sync(); err != nil {
		return fmt.Errorf("store %s: %w", key, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := f.Commit(); err != nil {
		return fmt.Errorf("store %s: %w", key, err)
	}
	if el, ok := s.files[key]; ok {
		s.drop(el)
	}
	s.files[key] = s.lru.PushFront(&storeFile{key, size})
	s.size += size
	return s.evict(space)
}

// write stores the parts of data as one file under key.
func (s *store) write(key string, data ...[]byte) error {
	f, err := s.temp(key)
	if err != nil {
		return fmt.Errorf("store %s: %w", key, err)
	}
	defer f.Close()
	var size int64
	for _, d := range data {
		if _, err := f.Write(d); err != nil {
			return fmt.Errorf("store %s: %w", key, err)
		}
		size += int64(len(d))
	}
	return s.add(key, f, size)
}

func (s *store) bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

// evict removes the least recently used files until the files fit in the
// limit, and the disk has the free part that datadisk.Space.Reclaim asks
// for.
func (s *store) evict(space datadisk.Space) error {
	over := space.Reclaim()
	if space.Mirror > 0 {
		over = max(over, s.size-space.Mirror)
	}
	var errs []error
	for over > 0 && s.lru.Len() > 0 {
		el := s.lru.Back()
		f := el.Value.(*storeFile)
		if err := os.Remove(s.path(f.key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("evict: %w", err))
		}
		s.drop(el)
		over -= f.size
	}
	return errors.Join(errs...)
}

func (s *store) drop(el *list.Element) {
	f := s.lru.Remove(el).(*storeFile)
	delete(s.files, f.key)
	s.size -= f.size
}

func (s *store) path(key string) string {
	return filepath.Join(s.dir, filepath.FromSlash(key))
}
