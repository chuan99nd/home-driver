package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/chuan99nd/home-driver/drive/internal/access"
)

// maxFolderDepth is the deepest folder nesting the API lets users create. It
// is well below access.MaxDepth, beyond which Resolve fails closed.
const maxFolderDepth = 100

type nodeJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDir     bool   `json:"is_dir"`
	Size      int64  `json:"size"`
	Kind      string `json:"kind"`
	Thumb     bool   `json:"thumb,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	TrashedAt int64  `json:"trashed_at,omitempty"`
	Role      string `json:"role,omitempty"`
	Owner     string `json:"owner,omitempty"`
	Shared    bool   `json:"shared,omitempty"`
}

func toJSON(n *access.Node) nodeJSON {
	j := nodeJSON{ID: n.ID, Name: n.Name, IsDir: n.IsDir, Size: n.Size, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, TrashedAt: n.TrashedAt}
	if n.IsDir {
		j.Kind = "folder"
	} else {
		j.Kind = kindOf(n.Name)
		j.Thumb = canThumb(n.Name)
	}
	return j
}

type crumb struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// listChildren returns the live children of a folder. The caller must have
// authorized the folder; children inherit at least the folder's level.
// withShared adds the "shared" badge flag; pass it only for owners.
func (s *Server) listChildren(ctx context.Context, parentID, sortBy, order string, withShared bool) ([]nodeJSON, error) {
	col := "name COLLATE NOCASE"
	switch sortBy {
	case "date":
		col = "updated_at"
	case "size":
		col = "size"
	}
	dir := "ASC"
	if order == "desc" {
		dir = "DESC"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+access.NodeColumns+`,
		EXISTS(SELECT 1 FROM shares WHERE node_id = nodes.id) OR EXISTS(SELECT 1 FROM links WHERE node_id = nodes.id)
		FROM nodes WHERE parent_id = ? AND trashed_at IS NULL ORDER BY is_dir DESC, `+col+` `+dir+`, name`, parentID)
	if err != nil {
		return nil, fmt.Errorf("list children: %w", err)
	}
	defer rows.Close()
	out := []nodeJSON{}
	for rows.Next() {
		var n access.Node
		var shared bool
		if err := rows.Scan(&n.ID, &n.OwnerID, &n.ParentID, &n.Name, &n.IsDir, &n.BlobSHA, &n.Size,
			&n.CreatedAt, &n.UpdatedAt, &n.TrashedAt, &n.TrashedBy, &shared); err != nil {
			return nil, fmt.Errorf("list children: %w", err)
		}
		j := toJSON(&n)
		j.Shared = shared && withShared
		out = append(out, j)
	}
	return out, rows.Err()
}

// describe writes the standard node view: the node, the caller's role, the
// visible breadcrumbs and, for folders, the children.
func (s *Server) describe(w http.ResponseWriter, r *http.Request, res access.Result) {
	out := map[string]any{"node": toJSON(res.Node), "role": res.Level.String()}
	crumbs := []crumb{}
	for _, n := range res.Visible() {
		crumbs = append(crumbs, crumb{ID: n.ID, Name: n.Name})
	}
	out["breadcrumbs"] = crumbs
	if res.Node.IsDir {
		children, err := s.listChildren(r.Context(), res.Node.ID, r.URL.Query().Get("sort"), r.URL.Query().Get("order"), res.Level == access.Owner)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		out["children"] = children
	}
	s.json(w, http.StatusOK, out)
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request, u *User) {
	res, ok := s.authz(w, r, u.principal(), r.PathValue("id"), access.Viewer)
	if !ok {
		return
	}
	s.describe(w, r, res)
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Name string `json:"name"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	name, ok := validName(in.Name)
	if !ok {
		s.fail(w, http.StatusBadRequest, "bad_name", "invalid folder name")
		return
	}
	res, ok := s.authz(w, r, u.principal(), r.PathValue("id"), access.Editor)
	if !ok {
		return
	}
	if !res.Node.IsDir {
		s.fail(w, http.StatusBadRequest, "not_a_folder", "parent is not a folder")
		return
	}
	if len(res.Chain) >= maxFolderDepth {
		s.fail(w, http.StatusBadRequest, "too_deep", "folders are nested too deeply")
		return
	}
	if reservedName(res.Node, name) {
		s.fail(w, http.StatusBadRequest, "reserved_name", "this name is reserved; choose another")
		return
	}
	now := s.now()
	n := &access.Node{ID: randomHex(16), OwnerID: res.Node.OwnerID, ParentID: res.Node.ID, Name: name, IsDir: true, CreatedAt: now, UpdatedAt: now}
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO nodes(id, owner_id, parent_id, name, is_dir, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?)`,
		n.ID, n.OwnerID, n.ParentID, n.Name, now, now)
	if isUniqueViolation(err) {
		s.fail(w, http.StatusConflict, "name_conflict", "an item with this name already exists here")
		return
	}
	if err != nil {
		s.internal(w, r, fmt.Errorf("mkdir: %w", err))
		return
	}
	s.json(w, http.StatusCreated, map[string]any{"node": toJSON(n)})
}

// authzMutate authorizes changing the node itself (rename, move, trash):
// Editor on the node and on its parent. A share recipient therefore cannot
// rename, move or delete the shared item itself, only things inside it, and
// nobody can touch a root.
func (s *Server) authzMutate(w http.ResponseWriter, r *http.Request, p access.Principal, id string) (access.Result, bool) {
	res, ok := s.authz(w, r, p, id, access.Editor)
	if !ok {
		return res, false
	}
	if res.Node.ParentID == "" {
		s.fail(w, http.StatusBadRequest, "is_root", "the root folder cannot be changed")
		return res, false
	}
	parent, err := access.Resolve(r.Context(), s.db, p, res.Node.ParentID)
	if err != nil {
		s.internal(w, r, err)
		return res, false
	}
	if parent.Level < access.Editor {
		s.fail(w, http.StatusForbidden, "forbidden", "only the owner can rename, move or delete a shared item itself")
		return res, false
	}
	return res, true
}

func (s *Server) handlePatchNode(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Name     *string `json:"name"`
		ParentID *string `json:"parent_id"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	res, ok := s.authzMutate(w, r, u.principal(), r.PathValue("id"))
	if !ok {
		return
	}
	node := res.Node
	name, parentID := node.Name, node.ParentID
	if in.Name != nil {
		if name, ok = validName(*in.Name); !ok {
			s.fail(w, http.StatusBadRequest, "bad_name", "invalid name")
			return
		}
	}
	moving := in.ParentID != nil && *in.ParentID != node.ParentID
	var destDepth int
	destNode := res.Chain[1]
	if moving {
		dest, ok := s.authz(w, r, u.principal(), *in.ParentID, access.Editor)
		if !ok {
			return
		}
		if !dest.Node.IsDir {
			s.fail(w, http.StatusBadRequest, "not_a_folder", "destination is not a folder")
			return
		}
		// Moving between owners would silently take a file away from its
		// owner; use copy for that.
		if dest.Node.OwnerID != node.OwnerID {
			s.fail(w, http.StatusForbidden, "cross_owner", "items cannot be moved to another person's drive; copy instead")
			return
		}
		parentID = dest.Node.ID
		destDepth = len(dest.Chain)
		destNode = dest.Node
	}
	if reservedName(destNode, name) {
		s.fail(w, http.StatusBadRequest, "reserved_name", "this name is reserved; choose another")
		return
	}
	if name == node.Name && !moving {
		s.json(w, http.StatusOK, map[string]any{"node": toJSON(node)})
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer tx.Rollback()
	if moving {
		// Checked inside the transaction so two concurrent moves cannot
		// build a cycle between them.
		inside, err := isInSubtree(r.Context(), tx, node.ID, parentID)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		if inside {
			s.fail(w, http.StatusBadRequest, "into_self", "a folder cannot be moved into itself or one of its subfolders")
			return
		}
		var height int
		if err := tx.QueryRowContext(r.Context(), `
			WITH RECURSIVE t(id, d) AS (
				SELECT ?, 1 UNION ALL
				SELECT n.id, t.d + 1 FROM nodes n JOIN t ON n.parent_id = t.id WHERE t.d < 400
			) SELECT MAX(d) FROM t`, node.ID).Scan(&height); err != nil {
			s.internal(w, r, fmt.Errorf("subtree height: %w", err))
			return
		}
		if destDepth+height > maxFolderDepth+1 {
			s.fail(w, http.StatusBadRequest, "too_deep", "folders would be nested too deeply")
			return
		}
	}
	now := s.now()
	result, err := tx.ExecContext(r.Context(),
		`UPDATE nodes SET name = ?, parent_id = ?, updated_at = ? WHERE id = ? AND trashed_at IS NULL`, name, parentID, now, node.ID)
	if isUniqueViolation(err) {
		s.fail(w, http.StatusConflict, "name_conflict", "an item with this name already exists there")
		return
	}
	if err != nil {
		s.internal(w, r, fmt.Errorf("update node: %w", err))
		return
	}
	if n, _ := result.RowsAffected(); n != 1 {
		s.fail(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if err := tx.Commit(); err != nil {
		s.internal(w, r, err)
		return
	}
	node.Name, node.ParentID, node.UpdatedAt = name, parentID, now
	s.json(w, http.StatusOK, map[string]any{"node": toJSON(node)})
}

// isInSubtree reports whether candidate is root or a descendant of root.
func isInSubtree(ctx context.Context, tx *sql.Tx, root, candidate string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `
		WITH RECURSIVE up(id, d) AS (
			SELECT ?, 0 UNION ALL
			SELECT n.parent_id, up.d + 1 FROM nodes n JOIN up ON n.id = up.id WHERE n.parent_id IS NOT NULL AND up.d < 400
		) SELECT COUNT(*) FROM up WHERE id = ?`, candidate, root).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("subtree check: %w", err)
	}
	return n > 0, nil
}

// reservedName reports whether name may not be used directly inside parent.
// The only reserved name is the WebDAV virtual folder at the top of a drive.
func reservedName(parent *access.Node, name string) bool {
	return parent.ParentID == "" && name == davSharedName
}

// freeName returns name, or "name (n).ext" for the first n that is not taken
// among the live children of parentID.
func freeName(ctx context.Context, tx *sql.Tx, parentID, name string) (string, error) {
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	if base == "" { // dotfiles: ".bashrc"
		base, ext = name, ""
	}
	for i := 0; i < 10000; i++ {
		cand := name
		if i > 0 {
			suffix := fmt.Sprintf(" (%d)%s", i, ext)
			b := base
			for len(b)+len(suffix) > 255 && len(b) > 0 {
				b = b[:len(b)-1]
			}
			cand = strings.ToValidUTF8(b, "") + suffix
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE parent_id = ? AND name = ? AND trashed_at IS NULL`, parentID, cand).Scan(&n); err != nil {
			return "", fmt.Errorf("check name: %w", err)
		}
		if n == 0 {
			return cand, nil
		}
	}
	return "", errors.New("could not find a free name")
}

// handleCopy copies one file. It needs Viewer on the source and Editor on
// the destination; the copy belongs to the destination's owner. A name clash
// is resolved by adding a " (n)" suffix.
func (s *Server) handleCopy(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		ParentID string `json:"parent_id"`
		Name     string `json:"name"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	src, ok := s.authz(w, r, u.principal(), r.PathValue("id"), access.Viewer)
	if !ok {
		return
	}
	if src.Node.IsDir {
		s.fail(w, http.StatusBadRequest, "is_folder", "only files can be copied")
		return
	}
	dest, ok := s.authz(w, r, u.principal(), in.ParentID, access.Editor)
	if !ok {
		return
	}
	if !dest.Node.IsDir {
		s.fail(w, http.StatusBadRequest, "not_a_folder", "destination is not a folder")
		return
	}
	name := src.Node.Name
	if in.Name != "" {
		if name, ok = validName(in.Name); !ok {
			s.fail(w, http.StatusBadRequest, "bad_name", "invalid name")
			return
		}
	}
	if reservedName(dest.Node, name) {
		s.fail(w, http.StatusBadRequest, "reserved_name", "this name is reserved; choose another")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer tx.Rollback()
	name, err = freeName(r.Context(), tx, dest.Node.ID, name)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	now := s.now()
	n := &access.Node{ID: randomHex(16), OwnerID: dest.Node.OwnerID, ParentID: dest.Node.ID, Name: name,
		BlobSHA: src.Node.BlobSHA, Size: src.Node.Size, CreatedAt: now, UpdatedAt: now}
	// The refcount bump and the new reference are one transaction. If the
	// source was purged in the meantime the blob row is gone and we abort.
	upd, err := tx.ExecContext(r.Context(), `UPDATE blobs SET refcount = refcount + 1 WHERE sha256 = ?`, n.BlobSHA)
	if err != nil {
		s.internal(w, r, fmt.Errorf("copy: %w", err))
		return
	}
	if c, _ := upd.RowsAffected(); c != 1 {
		s.fail(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO nodes(id, owner_id, parent_id, name, is_dir, blob_sha, size, created_at, updated_at) VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		n.ID, n.OwnerID, n.ParentID, n.Name, n.BlobSHA, n.Size, now, now); err != nil {
		s.internal(w, r, fmt.Errorf("copy: %w", err))
		return
	}
	if err := tx.Commit(); err != nil {
		s.internal(w, r, err)
		return
	}
	s.json(w, http.StatusCreated, map[string]any{"node": toJSON(n)})
}

// handleTrash moves a node to the trash. Nothing is removed from disk.
func (s *Server) handleTrash(w http.ResponseWriter, r *http.Request, u *User) {
	res, ok := s.authzMutate(w, r, u.principal(), r.PathValue("id"))
	if !ok {
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `UPDATE nodes SET trashed_at = ?, trashed_by = ? WHERE id = ? AND trashed_at IS NULL`, s.now(), u.ID, res.Node.ID); err != nil {
		s.internal(w, r, fmt.Errorf("trash: %w", err))
		return
	}
	s.log.Info("trashed", "node", res.Node.ID, "by", u.Email)
	s.json(w, http.StatusOK, map[string]bool{"ok": true})
}

// sharedRoots returns the nodes directly shared with the user that are still
// reachable, each checked through access.Resolve.
func (s *Server) sharedRoots(ctx context.Context, u *User) ([]access.Result, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id FROM shares WHERE email = ? ORDER BY created_at DESC`, u.Email)
	if err != nil {
		return nil, fmt.Errorf("list shares: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list shares: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list shares: %w", err)
	}
	var out []access.Result
	for _, id := range ids {
		res, err := access.Resolve(ctx, s.db, u.principal(), id)
		if errors.Is(err, access.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// Owner level means the user shared something with themselves.
		if res.Trashed || res.Level == access.None || res.Level == access.Owner {
			continue
		}
		out = append(out, res)
	}
	return out, nil
}

func (s *Server) handleSharedWithMe(w http.ResponseWriter, r *http.Request, u *User) {
	roots, err := s.sharedRoots(r.Context(), u)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	items := []nodeJSON{}
	for _, res := range roots {
		j := toJSON(res.Node)
		j.Role = res.Level.String()
		if err := s.db.QueryRowContext(r.Context(), `SELECT email FROM users WHERE id = ?`, res.Node.OwnerID).Scan(&j.Owner); err != nil {
			s.internal(w, r, fmt.Errorf("load owner: %w", err))
			return
		}
		items = append(items, j)
	}
	s.json(w, http.StatusOK, map[string]any{"items": items})
}

// handleSearch finds nodes by name inside the user's drive and inside
// everything shared with the user.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, u *User) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 200 {
		s.json(w, http.StatusOK, map[string]any{"items": []nodeJSON{}})
		return
	}
	seeds := []any{}
	if own, err := access.Resolve(r.Context(), s.db, u.principal(), u.RootID); err == nil && own.Level == access.Owner {
		seeds = append(seeds, u.RootID)
	}
	roots, err := s.sharedRoots(r.Context(), u)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	for _, res := range roots {
		seeds = append(seeds, res.Node.ID)
	}
	if len(seeds) == 0 {
		s.json(w, http.StatusOK, map[string]any{"items": []nodeJSON{}})
		return
	}
	// An empty query returns the most recently changed files instead.
	where, tail := `n.name LIKE ? ESCAPE '\'`, `ORDER BY n.is_dir DESC, n.name COLLATE NOCASE LIMIT 200`
	if q == "" {
		where, tail = `n.is_dir = 0 AND ? != ''`, `ORDER BY n.updated_at DESC LIMIT 24`
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	args := append(seeds, like)
	rows, err := s.db.QueryContext(r.Context(), `
		WITH RECURSIVE t(id) AS (
			SELECT id FROM nodes WHERE id IN (?`+strings.Repeat(",?", len(seeds)-1)+`) AND trashed_at IS NULL
			UNION
			SELECT n.id FROM nodes n JOIN t ON n.parent_id = t.id WHERE n.trashed_at IS NULL
		)
		SELECT `+access.Columns("n")+` FROM nodes n JOIN t ON t.id = n.id
		WHERE n.parent_id IS NOT NULL AND `+where+` `+tail, args...)
	if err != nil {
		s.internal(w, r, fmt.Errorf("search: %w", err))
		return
	}
	defer rows.Close()
	items := []nodeJSON{}
	for rows.Next() {
		n, err := access.ScanNode(rows)
		if err != nil {
			s.internal(w, r, fmt.Errorf("search: %w", err))
			return
		}
		items = append(items, toJSON(n))
	}
	if err := rows.Err(); err != nil {
		s.internal(w, r, fmt.Errorf("search: %w", err))
		return
	}
	s.json(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request, u *User) {
	var used, files int64
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(SUM(size), 0), COUNT(*) FROM nodes WHERE owner_id = ? AND is_dir = 0`, u.ID).Scan(&used, &files); err != nil {
		s.internal(w, r, fmt.Errorf("usage: %w", err))
		return
	}
	out := map[string]any{"used_bytes": used, "files": files}
	if total, free, ok := diskStats(s.cfg.DataDir); ok {
		out["disk_total_bytes"] = total
		out["disk_free_bytes"] = free
	}
	s.json(w, http.StatusOK, out)
}
