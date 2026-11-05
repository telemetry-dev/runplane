package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/runplane/runplane/internal/atomicfile"
)

var (
	errTooLarge     = errors.New("upload is larger than the limit")
	errNoBlock      = errors.New("block list names a block that is not staged")
	errEmptyCommit  = errors.New("block list is empty")
	errNotCommitted = errors.New("upload has no committed blob")
)

// disk keeps blobs and staged upload blocks under one folder:
//
//	blobs/<id>               committed archive of an entry
//	uploads/<id>/<block>     staged blocks of an upload
type disk struct {
	root string
}

func (d disk) blob(id string) string    { return filepath.Join(d.root, "blobs", id) }
func (d disk) staging(id string) string { return filepath.Join(d.root, "uploads", id) }

func (d disk) init() error {
	for _, dir := range []string{d.root, filepath.Join(d.root, "blobs"), filepath.Join(d.root, "uploads")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("make cache folder: %w", err)
		}
	}
	return nil
}

// staged returns the count and the bytes of the staged blocks of an upload.
func (d disk) staged(id string) (int, int64) {
	files, _ := os.ReadDir(d.staging(id))
	var size int64
	for _, f := range files {
		if info, err := f.Info(); err == nil {
			size += info.Size()
		}
	}
	return len(files), size
}

// putBlock stages one block (Azure Put Block). A block with the same ID
// replaces the earlier one.
func (d disk) putBlock(id, blockID string, body io.Reader, limit int64) error {
	dir := d.staging(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("make staging folder: %w", err)
	}
	return writeFile(dir, filepath.Join(dir, blockName(blockID)), body, limit)
}

// commitBlocks joins staged blocks, in list order, into the blob (Azure Put
// Block List) and drops the staged blocks.
func (d disk) commitBlocks(id string, blockIDs []string) error {
	if len(blockIDs) == 0 {
		return errEmptyCommit
	}
	dir := d.staging(id)
	f, err := atomicfile.CreateIn(dir, d.blob(id), 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, blockID := range blockIDs {
		if err := appendFile(f, filepath.Join(dir, blockName(blockID))); err != nil {
			return err
		}
	}
	if err := f.Commit(); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// putBlob writes the whole blob in one request (Azure Put Blob) and drops
// staged blocks.
func (d disk) putBlob(id string, body io.Reader, limit int64) error {
	dir := d.staging(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("make staging folder: %w", err)
	}
	if err := writeFile(dir, d.blob(id), body, limit); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

func (d disk) size(id string) (int64, error) {
	info, err := os.Stat(d.blob(id))
	if errors.Is(err, os.ErrNotExist) {
		return 0, errNotCommitted
	}
	if err != nil {
		return 0, fmt.Errorf("stat blob: %w", err)
	}
	return info.Size(), nil
}

func (d disk) open(id string) (*os.File, error) {
	return os.Open(d.blob(id))
}

func (d disk) remove(id string) error {
	err := os.Remove(d.blob(id))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return errors.Join(err, os.RemoveAll(d.staging(id)))
}

// ids returns the entry IDs that have files in a subfolder ("blobs" or "uploads").
func (d disk) ids(sub string) []string {
	files, _ := os.ReadDir(filepath.Join(d.root, sub))
	ids := make([]string, len(files))
	for i, f := range files {
		ids[i] = f.Name()
	}
	return ids
}

// blockName maps a client block ID to a safe file name of fixed length.
func blockName(blockID string) string {
	sum := sha256.Sum256([]byte(blockID))
	return hex.EncodeToString(sum[:])
}

// writeFile writes body to path through a temporary file in the staging
// folder dir, so that a failed request leaves no partial file at path.
func writeFile(dir, path string, body io.Reader, limit int64) error {
	f, err := atomicfile.CreateIn(dir, path, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(body, limit+1))
	if err != nil {
		return fmt.Errorf("read upload: %w", err)
	}
	if n > limit {
		return errTooLarge
	}
	return f.Commit()
}

func appendFile(dst io.Writer, path string) error {
	src, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return errNoBlock
	}
	if err != nil {
		return fmt.Errorf("open block: %w", err)
	}
	defer src.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy block: %w", err)
	}
	return nil
}
