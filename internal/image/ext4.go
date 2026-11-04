package image

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Microsoft/hcsshim/ext4/tar2ext4"
)

// maxDiskSize is the largest ext4 file that runplane makes. The writer
// reserves 256 bytes of metadata for each GiB.
const maxDiskSize = 256 << 30

// writeExt4 writes the tar stream from fill as an ext4 file at dst, through
// a temp file and a rename. It returns the file size.
func writeExt4(dst string, fill func(w io.Writer) error) (int64, error) {
	f, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	pr, pw := io.Pipe()
	filled := make(chan error, 1)
	go func() {
		err := fill(pw)
		pw.CloseWithError(err)
		filled <- err
	}()
	err = tar2ext4.ConvertTarToExt4(pr, f, tar2ext4.MaximumDiskSize(maxDiskSize))
	if err == nil {
		// The ext4 writer stops at the end mark of the tar. Read the rest,
		// so that fill can return.
		_, err = io.Copy(io.Discard, pr)
	}
	pr.CloseWithError(err)
	if fillErr := <-filled; err == nil {
		err = fillErr
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		return 0, fmt.Errorf("write %s: %w", dst, err)
	}
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if err := f.Chmod(0o644); err != nil {
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(f.Name(), dst); err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// tarDir writes the files in dir as a tar stream. All files belong to root,
// as in the read-only bind mount of container mode.
func tarDir(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var link string
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
