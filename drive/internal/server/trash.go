package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"time"

	"github.com/chuan99nd/home-driver/drive/internal/access"
)

func (s *Server) handleListTrash(w http.ResponseWriter, r *http.Request, u *User) {
	// Owner-scoped query: the trash only ever shows the caller's own nodes.
	rows, err := s.db.QueryContext(r.Context(), `SELECT `+access.NodeColumns+` FROM nodes
		WHERE owner_id = ? AND trashed_at IS NOT NULL ORDER BY trashed_at DESC, name`, u.ID)
	if err != nil {
		s.internal(w, r, fmt.Errorf("list trash: %w", err))
		return
	}
	defer rows.Close()
	items := []nodeJSON{}
	for rows.Next() {
		n, err := access.ScanNode(rows)
		if err != nil {
			s.internal(w, r, fmt.Errorf("list trash: %w", err))
			return
		}
		items = append(items, toJSON(n))
	}
	if err := rows.Err(); err != nil {
		s.internal(w, r, fmt.Errorf("list trash: %w", err))
		return
	}
	s.json(w, http.StatusOK, map[string]any{"items": items, "trash_days": s.cfg.TrashDays})
}

// handleRestore takes a node out of the trash.
//
// The owner may restore anything of theirs; it goes back to its folder, or to
// the root of their drive when that folder is itself in the trash. The person
// who trashed an item inside a folder shared with them as editor may undo
// that (in place only), which is what the UI's "Undo" relies on.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request, u *User) {
	res, err := access.Resolve(r.Context(), s.db, u.principal(), r.PathValue("id"))
	if err != nil && !errors.Is(err, access.ErrNotFound) {
		s.internal(w, r, err)
		return
	}
	if err != nil || res.Node.TrashedAt == 0 || res.Node.ParentID == "" {
		s.fail(w, http.StatusNotFound, "not_found", "not found in trash")
		return
	}
	node := res.Node
	target := node.ParentID
	parent, err := access.Resolve(r.Context(), s.db, u.principal(), node.ParentID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	switch {
	case res.Level == access.Owner:
		if parent.Trashed {
			if err := s.db.QueryRowContext(r.Context(), `SELECT root_id FROM users WHERE id = ?`, node.OwnerID).Scan(&target); err != nil {
				s.internal(w, r, fmt.Errorf("load root: %w", err))
				return
			}
		}
	case node.TrashedBy == u.ID && !parent.Trashed && parent.Level >= access.Editor:
		// Undo by the editor who deleted it.
	default:
		s.fail(w, http.StatusNotFound, "not_found", "not found in trash")
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer tx.Rollback()
	name, err := freeName(r.Context(), tx, target, node.Name)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	result, err := tx.ExecContext(r.Context(),
		`UPDATE nodes SET trashed_at = NULL, trashed_by = NULL, parent_id = ?, name = ? WHERE id = ? AND trashed_at IS NOT NULL`,
		target, name, node.ID)
	if err != nil {
		s.internal(w, r, fmt.Errorf("restore: %w", err))
		return
	}
	if n, _ := result.RowsAffected(); n != 1 {
		s.fail(w, http.StatusNotFound, "not_found", "not found in trash")
		return
	}
	if err := tx.Commit(); err != nil {
		s.internal(w, r, err)
		return
	}
	node.Name, node.ParentID, node.TrashedAt, node.TrashedBy = name, target, 0, 0
	s.json(w, http.StatusOK, map[string]any{"node": toJSON(node), "parent_id": target, "moved_to_root": parent.Trashed})
}

func (s *Server) handlePurgeOne(w http.ResponseWriter, r *http.Request, u *User) {
	res, ok := s.authzTrashed(w, r, u.principal(), r.PathValue("id"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	if err := s.purge(ctx, res.Node.ID); err != nil {
		s.internal(w, r, err)
		return
	}
	s.log.Info("deleted forever", "node", res.Node.ID, "by", u.Email)
	s.json(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleEmptyTrash(w http.ResponseWriter, r *http.Request, u *User) {
	ids, err := s.queryIDs(r.Context(), `SELECT id FROM nodes WHERE owner_id = ? AND trashed_at IS NOT NULL`, u.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	n := 0
	for _, id := range ids {
		// Each one is authorized like a single delete-forever.
		res, err := access.Resolve(ctx, s.db, u.principal(), id)
		if errors.Is(err, access.ErrNotFound) {
			continue // already purged as part of a trashed parent folder
		}
		if err != nil {
			s.internal(w, r, err)
			return
		}
		if res.Level != access.Owner || res.Node.TrashedAt == 0 {
			continue
		}
		if err := s.purge(ctx, id); err != nil {
			s.internal(w, r, err)
			return
		}
		n++
	}
	s.log.Info("trash emptied", "by", u.Email, "items", n)
	s.json(w, http.StatusOK, map[string]any{"ok": true, "purged": n})
}

func (s *Server) queryIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query ids: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("query ids: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// errNotTrashed is returned by purge for a node that is not in the trash.
var errNotTrashed = errors.New("purge: node is not in the trash")

// purge permanently deletes a trashed node and everything under it. This is
// the ONLY code path that deletes file rows or blob files.
//
// It refuses nodes that are not directly in the trash. Blob refcounts are
// decremented in the same transaction as the row deletes; a blob file is
// removed only after commit, only when its refcount reached zero AND no node
// row references it any more. blobMu keeps a concurrent upload of identical
// content from racing with the file removal.
func (s *Server) purge(ctx context.Context, nodeID string) error {
	s.blobMu.Lock()
	defer s.blobMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	defer tx.Rollback()

	var trashed int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE id = ? AND trashed_at IS NOT NULL AND parent_id IS NOT NULL`, nodeID).Scan(&trashed)
	if err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	if trashed != 1 {
		return errNotTrashed
	}

	const subtree = `WITH RECURSIVE t(id) AS (
		SELECT ? UNION ALL SELECT n.id FROM nodes n JOIN t ON n.parent_id = t.id
	) `
	rows, err := tx.QueryContext(ctx, subtree+`SELECT n.blob_sha, COUNT(*) FROM nodes n JOIN t ON t.id = n.id
		WHERE n.blob_sha IS NOT NULL GROUP BY n.blob_sha`, nodeID)
	if err != nil {
		return fmt.Errorf("purge: list blobs: %w", err)
	}
	refs := map[string]int{}
	for rows.Next() {
		var sha string
		var n int
		if err := rows.Scan(&sha, &n); err != nil {
			rows.Close()
			return fmt.Errorf("purge: list blobs: %w", err)
		}
		refs[sha] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("purge: list blobs: %w", err)
	}

	// Foreign keys are checked at the end of the statement, so the whole
	// subtree can go in one DELETE. Shares, links and uploads cascade.
	if _, err := tx.ExecContext(ctx, subtree+`DELETE FROM nodes WHERE id IN (SELECT id FROM t)`, nodeID); err != nil {
		return fmt.Errorf("purge: delete nodes: %w", err)
	}

	var dead []string
	for sha, n := range refs {
		// The CHECK (refcount >= 0) aborts the whole purge on a bad count.
		if _, err := tx.ExecContext(ctx, `UPDATE blobs SET refcount = refcount - ? WHERE sha256 = ?`, n, sha); err != nil {
			return fmt.Errorf("purge: unreference blob: %w", err)
		}
		var refcount, live int
		if err := tx.QueryRowContext(ctx, `SELECT refcount, (SELECT COUNT(*) FROM nodes WHERE blob_sha = ?1) FROM blobs WHERE sha256 = ?1`, sha).
			Scan(&refcount, &live); err != nil {
			return fmt.Errorf("purge: check blob: %w", err)
		}
		if refcount != live {
			return fmt.Errorf("purge: blob %s refcount %d but %d references; refusing to continue", sha, refcount, live)
		}
		if refcount == 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE sha256 = ?`, sha); err != nil {
				return fmt.Errorf("purge: delete blob row: %w", err)
			}
			dead = append(dead, sha)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	for _, sha := range dead {
		if err := s.blobs.Remove(sha); err != nil {
			// The data is unreferenced either way; an orphan file is only wasted space.
			s.log.Error("remove blob file", "sha256", sha, "err", err)
		}
		if err := os.Remove(s.thumbPath(sha)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("remove thumbnail", "sha256", sha, "err", err)
		}
	}
	return nil
}

// purgeExpired deletes forever what has been in the trash longer than
// DRIVE_TRASH_DAYS. It returns the number of trashed items purged.
func (s *Server) purgeExpired(ctx context.Context) (int, error) {
	if s.cfg.TrashDays <= 0 {
		return 0, nil
	}
	cutoff := s.Now().Add(-time.Duration(s.cfg.TrashDays) * 24 * time.Hour).Unix()
	ids, err := s.queryIDs(ctx, `SELECT id FROM nodes WHERE trashed_at IS NOT NULL AND trashed_at < ? AND parent_id IS NOT NULL`, cutoff)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		err := s.purge(ctx, id)
		if errors.Is(err, errNotTrashed) {
			continue // went away with a parent, or was restored meanwhile
		}
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// RunJobs runs the periodic maintenance until ctx is cancelled: trash purge,
// stale upload cleanup, expired session cleanup.
func (s *Server) RunJobs(ctx context.Context) {
	run := func() {
		if n, err := s.purgeExpired(ctx); err != nil {
			s.log.Error("trash purge failed", "err", err)
		} else if n > 0 {
			s.log.Info("trash purge", "items", n, "older_than_days", s.cfg.TrashDays)
		}
		if err := s.cleanStaleUploads(ctx); err != nil {
			s.log.Error("upload cleanup failed", "err", err)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, s.now()); err != nil {
			s.log.Error("session cleanup failed", "err", err)
		}
	}
	// Not immediately at boot: give the owner a moment after a restart.
	first := time.NewTimer(2 * time.Minute)
	defer first.Stop()
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
			run()
		case <-tick.C:
			run()
		}
	}
}
