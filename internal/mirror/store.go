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
)

// Folders of the store. A file in them has the hex part of its sha256 digest
// as its name.
const (
	blobsDir     = "blobs"
	manifestsDir = "manifests"
)

// store keeps blobs and manifests as files named by their digest. When the
// files use more than max bytes, it removes the least recently used ones.
type store struct {
	dir string
	max int64 // 0 = no limit

	mu    sync.Mutex
	lru   *list.List               // of *storeFile, most recently used first
	files map[string]*list.Element // by key, for example "blobs/<hex>"
	size  int64
}

type storeFile struct {
	key  string
	size int64
}

func openStore(dir string, max int64) (*store, error) {
	s := &store{dir: dir, max: max, lru: list.New(), files: map[string]*list.Element{}}
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
	if err := s.evict(); err != nil {
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

// temp makes a temp file on the same file system as the store.
func (s *store) temp() (*os.File, error) {
	return os.CreateTemp(filepath.Join(s.dir, "tmp"), "")
}

// add moves the complete temp file tmp into the store as key. Then it removes
// the least recently used files over the limit. It does not keep a file that
// is larger than the limit.
func (s *store) add(key, tmp string, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.max > 0 && size > s.max {
		return os.Remove(tmp)
	}
	if err := os.Rename(tmp, s.path(key)); err != nil {
		return errors.Join(fmt.Errorf("store %s: %w", key, err), os.Remove(tmp))
	}
	if el, ok := s.files[key]; ok {
		s.drop(el)
	}
	s.files[key] = s.lru.PushFront(&storeFile{key, size})
	s.size += size
	return s.evict()
}

// write stores the parts of data as one file under key.
func (s *store) write(key string, data ...[]byte) error {
	f, err := s.temp()
	if err != nil {
		return fmt.Errorf("store %s: %w", key, err)
	}
	var size int64
	for _, d := range data {
		if _, err = f.Write(d); err != nil {
			break
		}
		size += int64(len(d))
	}
	if err == nil {
		err = f.Sync()
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return errors.Join(fmt.Errorf("store %s: %w", key, err), os.Remove(f.Name()))
	}
	return s.add(key, f.Name(), size)
}

func (s *store) bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

func (s *store) evict() error {
	var errs []error
	for s.max > 0 && s.size > s.max {
		el := s.lru.Back()
		if err := os.Remove(s.path(el.Value.(*storeFile).key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("evict: %w", err))
		}
		s.drop(el)
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
