// Package blob is the content-addressed file store: DATA_DIR/blobs/ab/cd/<sha256>.
//
// It only knows about files on disk. Reference counting lives in the database
// and is coordinated by the server (see server.purge and upload completion).
package blob

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Store is a directory of blobs.
type Store struct {
	dir string
}

// New returns a store rooted at dir (created if missing).
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create blob dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Path returns where the blob with this hash lives. It panics on a malformed
// hash: hashes are produced by this program, never taken from a client.
func (s *Store) Path(sha string) string {
	if !shaRe.MatchString(sha) {
		panic("blob: malformed sha256 " + sha)
	}
	return filepath.Join(s.dir, sha[0:2], sha[2:4], sha)
}

// Open opens a blob for reading.
func (s *Store) Open(sha string) (*os.File, error) {
	return os.Open(s.Path(sha))
}

// Ingest moves the finished temp file into the store under its hash. The
// temp file is fsynced, renamed atomically, and the directory fsynced. If a
// blob with the same hash and size already exists the temp file is dropped.
func (s *Store) Ingest(tmpPath, sha string, size int64) error {
	dst := s.Path(sha)
	if st, err := os.Stat(dst); err == nil && st.Size() == size {
		if err := os.Remove(tmpPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("drop duplicate upload: %w", err)
		}
		return nil
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("open upload: %w", err)
	}
	st, err := f.Stat()
	if err == nil && st.Size() != size {
		err = fmt.Errorf("upload file is %d bytes, expected %d", st.Size(), size)
	}
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil {
		return fmt.Errorf("sync upload: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("create blob dir: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("store blob: %w", err)
	}
	if err := os.Chmod(dst, 0o440); err != nil {
		return fmt.Errorf("store blob: %w", err)
	}
	return syncDir(filepath.Dir(dst))
}

// Remove deletes a blob file. Only server.purge may call this.
func (s *Store) Remove(sha string) error {
	err := os.Remove(s.Path(sha))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove blob: %w", err)
	}
	return nil
}

// Exists reports whether the blob file is present.
func (s *Store) Exists(sha string) bool {
	_, err := os.Stat(s.Path(sha))
	return err == nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("sync dir: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync dir: %w", err)
	}
	return nil
}
