package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chuan99nd/home-driver/drive/internal/access"
)

// Chunked upload protocol (see drive/CLAUDE.md):
//
//	POST   /api/uploads                 {parent_id, name, size, sha256?, on_conflict?} -> {id, chunk_size, offset}
//	PUT    /api/uploads/{id}/chunk?offset=N   body = exactly the next chunk
//	GET    /api/uploads/{id}            -> {offset, size, chunk_size}   (resume)
//	POST   /api/uploads/{id}/complete   {sha256?} -> {node}
//	DELETE /api/uploads/{id}
//
// Chunks arrive strictly in order. Every chunk is chunk_size bytes except the
// last one. Bytes go straight to DATA_DIR/tmp/<id>.part; the running sha256
// state is saved after every chunk so a resumed upload never re-reads data.

const (
	uploadStaleAfter = 24 * time.Hour
	maxFileSize      = int64(1) << 40 // 1 TiB sanity bound
)

var (
	uploadIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
	sha256Re   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type upload struct {
	ID         string
	UserID     int64
	ParentID   string
	Name       string
	Size       int64
	ChunkSize  int64
	Received   int64
	HashState  []byte
	ExpectSHA  string
	OnConflict string
}

func (s *Server) partPath(id string) string {
	return filepath.Join(s.tmpDir, id+".part")
}

// loadUpload returns the caller's own upload session.
func (s *Server) loadUpload(w http.ResponseWriter, r *http.Request, u *User) (*upload, bool) {
	id := r.PathValue("uid")
	if !uploadIDRe.MatchString(id) {
		s.fail(w, http.StatusNotFound, "not_found", "no such upload")
		return nil, false
	}
	up := &upload{}
	err := s.db.QueryRowContext(r.Context(), `SELECT id, user_id, parent_id, name, size, chunk_size, received, hash_state, expect_sha, on_conflict
		FROM uploads WHERE id = ? AND user_id = ?`, id, u.ID).Scan(&up.ID, &up.UserID, &up.ParentID, &up.Name, &up.Size,
		&up.ChunkSize, &up.Received, &up.HashState, &up.ExpectSHA, &up.OnConflict)
	if errors.Is(err, sql.ErrNoRows) {
		s.fail(w, http.StatusNotFound, "not_found", "no such upload")
		return nil, false
	}
	if err != nil {
		s.internal(w, r, fmt.Errorf("load upload: %w", err))
		return nil, false
	}
	return up, true
}

// lockUpload makes chunk and complete requests for one upload mutually
// exclusive. A second concurrent request gets 409.
func (s *Server) lockUpload(w http.ResponseWriter, id string) (unlock func(), ok bool) {
	v, _ := s.uploadLocks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		s.fail(w, http.StatusConflict, "busy", "another request for this upload is in progress")
		return nil, false
	}
	return mu.Unlock, true
}

func (s *Server) discardUpload(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete upload: %w", err)
	}
	s.uploadLocks.Delete(id)
	if err := os.Remove(s.partPath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove upload temp file: %w", err)
	}
	return nil
}

func (s *Server) handleUploadInit(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		ParentID   string `json:"parent_id"`
		Name       string `json:"name"`
		Size       *int64 `json:"size"`
		SHA256     string `json:"sha256"`
		OnConflict string `json:"on_conflict"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	name, ok := validName(in.Name)
	if !ok {
		s.fail(w, http.StatusBadRequest, "bad_name", "invalid file name")
		return
	}
	if in.Size == nil || *in.Size < 0 || *in.Size > maxFileSize {
		s.fail(w, http.StatusBadRequest, "bad_size", "size is required and must be between 0 and 1 TiB")
		return
	}
	expect := strings.ToLower(in.SHA256)
	if expect != "" && !sha256Re.MatchString(expect) {
		s.fail(w, http.StatusBadRequest, "bad_sha256", "sha256 must be 64 hex characters")
		return
	}
	if in.OnConflict == "" {
		in.OnConflict = "reject"
	}
	if in.OnConflict != "reject" && in.OnConflict != "rename" {
		s.fail(w, http.StatusBadRequest, "bad_on_conflict", `on_conflict must be "reject" or "rename"`)
		return
	}
	parent, ok := s.authz(w, r, u.principal(), in.ParentID, access.Editor)
	if !ok {
		return
	}
	if !parent.Node.IsDir {
		s.fail(w, http.StatusBadRequest, "not_a_folder", "parent is not a folder")
		return
	}
	if len(parent.Chain) > maxFolderDepth {
		s.fail(w, http.StatusBadRequest, "too_deep", "folders are nested too deeply")
		return
	}
	if reservedName(parent.Node, name) {
		s.fail(w, http.StatusBadRequest, "reserved_name", "this name is reserved; choose another")
		return
	}
	if in.OnConflict == "reject" {
		// Early answer; the unique index is the real guard at completion.
		var n int
		if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM nodes WHERE parent_id = ? AND name = ? AND trashed_at IS NULL`,
			parent.Node.ID, name).Scan(&n); err != nil {
			s.internal(w, r, fmt.Errorf("check name: %w", err))
			return
		}
		if n > 0 {
			s.fail(w, http.StatusConflict, "name_conflict", "an item with this name already exists here")
			return
		}
	}
	if _, free, ok := diskStats(s.cfg.DataDir); ok && uint64(*in.Size) > free {
		s.fail(w, http.StatusInsufficientStorage, "disk_full", "not enough free space on the server")
		return
	}
	id := randomHex(16)
	f, err := os.OpenFile(s.partPath(id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		s.internal(w, r, fmt.Errorf("create upload temp file: %w", err))
		return
	}
	f.Close()
	now := s.now()
	if _, err := s.db.ExecContext(r.Context(), `INSERT INTO uploads(id, user_id, parent_id, name, size, chunk_size, received, expect_sha, on_conflict, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`, id, u.ID, parent.Node.ID, name, *in.Size, s.cfg.ChunkSize, expect, in.OnConflict, now, now); err != nil {
		os.Remove(s.partPath(id))
		s.internal(w, r, fmt.Errorf("create upload: %w", err))
		return
	}
	s.json(w, http.StatusCreated, map[string]any{"id": id, "chunk_size": s.cfg.ChunkSize, "offset": 0, "size": *in.Size})
}

func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request, u *User) {
	up, ok := s.loadUpload(w, r, u)
	if !ok {
		return
	}
	s.json(w, http.StatusOK, map[string]any{"id": up.ID, "chunk_size": up.ChunkSize, "offset": up.Received, "size": up.Size})
}

func (s *Server) handleUploadAbort(w http.ResponseWriter, r *http.Request, u *User) {
	up, ok := s.loadUpload(w, r, u)
	if !ok {
		return
	}
	unlock, ok := s.lockUpload(w, up.ID)
	if !ok {
		return
	}
	defer unlock()
	if err := s.discardUpload(r.Context(), up.ID); err != nil {
		s.internal(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]bool{"ok": true})
}

func restoreHash(state []byte) (hash.Hash, error) {
	h := sha256.New()
	if len(state) > 0 {
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
			return nil, fmt.Errorf("restore hash state: %w", err)
		}
	}
	return h, nil
}

func (s *Server) handleUploadChunk(w http.ResponseWriter, r *http.Request, u *User) {
	id := r.PathValue("uid")
	if !uploadIDRe.MatchString(id) {
		s.fail(w, http.StatusNotFound, "not_found", "no such upload")
		return
	}
	unlock, ok := s.lockUpload(w, id)
	if !ok {
		return
	}
	defer unlock()
	// Loaded under the lock so Received is current.
	up, ok := s.loadUpload(w, r, u)
	if !ok {
		s.uploadLocks.Delete(id)
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "bad_offset", "offset query parameter is required")
		return
	}
	if offset != up.Received {
		// Tells a resuming client where to continue.
		s.json(w, http.StatusConflict, map[string]any{"error": "wrong_offset", "message": "chunk out of order", "offset": up.Received})
		return
	}
	want := up.Size - up.Received
	if want > up.ChunkSize {
		want = up.ChunkSize
	}
	if want <= 0 {
		s.json(w, http.StatusConflict, map[string]any{"error": "already_complete", "message": "all bytes received", "offset": up.Received})
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != want {
		s.fail(w, http.StatusBadRequest, "bad_chunk_size", fmt.Sprintf("this chunk must be exactly %d bytes", want))
		return
	}
	h, err := restoreHash(up.HashState)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	// A slow phone connection may need a while for 32 MiB; still bounded.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Minute))

	f, err := os.OpenFile(s.partPath(up.ID), os.O_WRONLY, 0)
	if err != nil {
		s.internal(w, r, fmt.Errorf("open upload temp file: %w", err))
		return
	}
	defer f.Close()
	// Drop whatever a previously failed chunk left behind.
	if err := f.Truncate(up.Received); err != nil {
		s.internal(w, r, fmt.Errorf("truncate upload: %w", err))
		return
	}
	if _, err := f.Seek(up.Received, io.SeekStart); err != nil {
		s.internal(w, r, fmt.Errorf("seek upload: %w", err))
		return
	}
	// Read at most want+1 bytes: the extra byte detects an oversized body
	// without ever buffering or storing more than one chunk.
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(r.Body, want+1))
	if copyErr != nil || n != want {
		if err := f.Truncate(up.Received); err != nil {
			s.log.Error("truncate after failed chunk", "upload", up.ID, "err", err)
		}
		if copyErr != nil && n <= want {
			var werr *fs.PathError
			if errors.As(copyErr, &werr) {
				s.internal(w, r, fmt.Errorf("write chunk: %w", copyErr))
				return
			}
			s.fail(w, http.StatusBadRequest, "chunk_interrupted", "chunk upload was interrupted; send it again")
			return
		}
		s.fail(w, http.StatusBadRequest, "bad_chunk_size", fmt.Sprintf("this chunk must be exactly %d bytes", want))
		return
	}
	if err := f.Sync(); err != nil {
		s.internal(w, r, fmt.Errorf("sync chunk: %w", err))
		return
	}
	state, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		s.internal(w, r, fmt.Errorf("save hash state: %w", err))
		return
	}
	received := up.Received + n
	// Not tied to the request context: the bytes are on disk, record them
	// even if the client just went away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `UPDATE uploads SET received = ?, hash_state = ?, updated_at = ? WHERE id = ?`,
		received, state, s.now(), up.ID); err != nil {
		s.internal(w, r, fmt.Errorf("record chunk: %w", err))
		return
	}
	s.json(w, http.StatusOK, map[string]any{"offset": received, "size": up.Size})
}

func (s *Server) handleUploadComplete(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		SHA256 string `json:"sha256"`
	}
	if r.ContentLength != 0 {
		if !s.readJSON(w, r, &in) {
			return
		}
	}
	id := r.PathValue("uid")
	if !uploadIDRe.MatchString(id) {
		s.fail(w, http.StatusNotFound, "not_found", "no such upload")
		return
	}
	unlock, ok := s.lockUpload(w, id)
	if !ok {
		return
	}
	defer unlock()
	up, ok := s.loadUpload(w, r, u)
	if !ok {
		s.uploadLocks.Delete(id)
		return
	}
	if up.Received != up.Size {
		s.json(w, http.StatusConflict, map[string]any{"error": "incomplete", "message": "not all bytes have been uploaded", "offset": up.Received})
		return
	}
	h, err := restoreHash(up.HashState)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))
	expect := up.ExpectSHA
	if in.SHA256 != "" {
		claimed := strings.ToLower(in.SHA256)
		if expect != "" && claimed != expect {
			s.fail(w, http.StatusBadRequest, "bad_sha256", "sha256 differs from the one given at init")
			return
		}
		expect = claimed
	}
	if expect != "" && expect != sum {
		// The received bytes are not what the client meant to send.
		if err := s.discardUpload(r.Context(), up.ID); err != nil {
			s.log.Error("discard corrupt upload", "upload", up.ID, "err", err)
		}
		s.fail(w, http.StatusUnprocessableEntity, "hash_mismatch", "the uploaded data is corrupt (sha256 mismatch); upload it again")
		return
	}
	// Access may have changed since init (share revoked, folder trashed).
	parent, ok := s.authz(w, r, u.principal(), up.ParentID, access.Editor)
	if !ok {
		return
	}

	// From here on the request must not be abandoned half-way.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()

	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer tx.Rollback()
	name := up.Name
	if up.OnConflict == "rename" {
		if name, err = freeName(ctx, tx, parent.Node.ID, name); err != nil {
			s.internal(w, r, err)
			return
		}
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO blobs(sha256, size, refcount, created_at) VALUES (?, ?, 1, ?)
		ON CONFLICT(sha256) DO UPDATE SET refcount = refcount + 1`, sum, up.Size, now); err != nil {
		s.internal(w, r, fmt.Errorf("reference blob: %w", err))
		return
	}
	n := &access.Node{ID: randomHex(16), OwnerID: parent.Node.OwnerID, ParentID: parent.Node.ID, Name: name,
		BlobSHA: sum, Size: up.Size, CreatedAt: now, UpdatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO nodes(id, owner_id, parent_id, name, is_dir, blob_sha, size, created_at, updated_at)
		VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?)`, n.ID, n.OwnerID, n.ParentID, n.Name, n.BlobSHA, n.Size, now, now)
	if isUniqueViolation(err) {
		s.fail(w, http.StatusConflict, "name_conflict", "an item with this name already exists here")
		return
	}
	if err != nil {
		s.internal(w, r, fmt.Errorf("create file: %w", err))
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, up.ID); err != nil {
		s.internal(w, r, fmt.Errorf("finish upload: %w", err))
		return
	}
	// The file is made durable in the blob store before the database says it
	// exists. If the commit then fails, an unreferenced blob file is left
	// behind, which is harmless and reused by the next identical upload.
	if err := s.blobs.Ingest(s.partPath(up.ID), sum, up.Size); err != nil {
		s.internal(w, r, err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.internal(w, r, fmt.Errorf("finish upload: %w", err))
		return
	}
	s.uploadLocks.Delete(up.ID)
	s.json(w, http.StatusCreated, map[string]any{"node": toJSON(n), "sha256": sum})
}

// cleanStaleUploads removes upload sessions nobody touched for a day, and
// stray temp files without a session.
func (s *Server) cleanStaleUploads(ctx context.Context) error {
	cutoff := s.Now().Add(-uploadStaleAfter)
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM uploads WHERE updated_at < ?`, cutoff.Unix())
	if err != nil {
		return fmt.Errorf("list stale uploads: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("list stale uploads: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list stale uploads: %w", err)
	}
	for _, id := range ids {
		if err := s.discardUpload(ctx, id); err != nil {
			return err
		}
		s.log.Info("removed stale upload", "upload", id)
	}
	entries, err := os.ReadDir(s.tmpDir)
	if err != nil {
		return fmt.Errorf("read tmp dir: %w", err)
	}
	for _, e := range entries {
		id, isPart := strings.CutSuffix(e.Name(), ".part")
		info, err := e.Info()
		if err != nil || !isPart || info.ModTime().After(cutoff) {
			continue
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM uploads WHERE id = ?`, id).Scan(&n); err != nil {
			return fmt.Errorf("check upload: %w", err)
		}
		if n == 0 {
			if err := os.Remove(filepath.Join(s.tmpDir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove stray temp file: %w", err)
			}
		}
	}
	return nil
}
