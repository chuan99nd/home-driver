package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"github.com/chuan99nd/home-driver/drive/internal/access"
)

// WebDAV at /dav/ for sync clients.
//
//	/dav/                      the user's My Drive
//	/dav/_Shared with me/      virtual, read-only folder: one entry per item
//	                           shared with the user (reserved name, see reservedName)
//
// Design rules (see drive/CLAUDE.md):
//   - Auth is HTTP Basic with an app password only; cookies are ignored.
//   - The file system is the nodes table, not a directory. Every path is
//     turned into a node id and then authorized by access.Resolve, exactly
//     like the JSON API. Paths are cleaned lexically first, so "..", "//" and
//     encoded slashes can only ever name something inside the user's own
//     tree or inside an item shared with them.
//   - DELETE moves to the trash. Overwriting a file keeps the previous
//     version as a trashed copy. Nothing here deletes data permanently.

const (
	davPrefix     = "/dav"
	davSharedName = "_Shared with me"
)

// davLockSystems holds one in-memory lock table per user. Paths are relative
// to each user's own tree, so a shared table would let users collide.
// Locks do not survive a restart.
type davLockSystems struct {
	mu sync.Mutex
	m  map[int64]webdav.LockSystem
}

func (d *davLockSystems) forUser(id int64) webdav.LockSystem {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.m == nil {
		d.m = map[int64]webdav.LockSystem{}
	}
	if d.m[id] == nil {
		d.m[id] = webdav.NewMemLS()
	}
	return d.m[id]
}

// davStatusWriter turns the 404/405/500 that x/net/webdav answers for a
// failed write into 403 when the reason was a permission check.
type davStatusWriter struct {
	http.ResponseWriter
	fs *davFS
}

func (w *davStatusWriter) WriteHeader(code int) {
	if w.fs.denied && (code == http.StatusNotFound || code == http.StatusMethodNotAllowed || code == http.StatusInternalServerError) {
		code = http.StatusForbidden
	}
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) handleDAV(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "private, no-cache")

	// Clients probe with an unauthenticated OPTIONS before asking for credentials.
	if r.Method == http.MethodOptions && r.Header.Get("Authorization") == "" {
		h.Set("DAV", "1, 2")
		h.Set("MS-Author-Via", "DAV")
		h.Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, COPY, MOVE, PROPFIND, PROPPATCH, LOCK, UNLOCK")
		w.WriteHeader(http.StatusOK)
		return
	}
	u, ok := s.davAuth(w, r)
	if !ok {
		return
	}
	dfs := &davFS{s: s, u: u, p: u.principal()}
	rel := strings.TrimPrefix(r.URL.Path, davPrefix)

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// Served by our own safe content path (attachment, sandbox CSP), never
		// by the generic handler, which would pick an active content type.
		n, err := dfs.resolve(r.Context(), rel)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if n.virtual || n.res.Node.IsDir {
			h.Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, "This is the Drive WebDAV endpoint. Open it with a WebDAV client; see \"Sync devices\" in Drive.")
			return
		}
		s.serveFileAs(w, r, n.res.Node, true)
		return
	case "PROPFIND":
		// Walking a whole drive in one request is expensive; RFC 4918 allows refusing it.
		if d := r.Header.Get("Depth"); d == "" || strings.EqualFold(d, "infinity") {
			h.Set("Content-Type", "application/xml; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?><D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>`)
			return
		}
	case "COPY", "MOVE":
		if status := dfs.checkDestination(r, rel); status != 0 {
			http.Error(w, http.StatusText(status), status)
			return
		}
	case http.MethodPut:
		if v := r.Header.Get("X-OC-Mtime"); v != "" {
			if t, err := strconv.ParseFloat(v, 64); err == nil && t > 0 && t < 1e11 {
				dfs.mtime = int64(t)
				h.Set("X-OC-Mtime", "accepted")
			}
		}
		if _, free, ok := diskStats(s.cfg.DataDir); ok && r.ContentLength > 0 && uint64(r.ContentLength) > free {
			http.Error(w, "not enough free space on the server", http.StatusInsufficientStorage)
			return
		}
	}
	if r.Method != http.MethodPut {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // XML bodies only
	}
	handler := &webdav.Handler{
		Prefix:     davPrefix,
		FileSystem: dfs,
		LockSystem: s.davLocks.forUser(u.ID),
		Logger: func(r *http.Request, err error) {
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				s.log.Debug("webdav", "method", r.Method, "err", err)
			}
		},
	}
	handler.ServeHTTP(&davStatusWriter{ResponseWriter: w, fs: dfs}, r)
}

// ---- the file system ----

// davFS is a webdav.FileSystem for one user and one request.
type davFS struct {
	s      *Server
	u      *User
	p      access.Principal
	mtime  int64 // client-supplied modification time for PUT, 0 if none
	denied bool  // a permission check failed during this request
}

// davNode is what a path names.
type davNode struct {
	virtual     bool          // the "_Shared with me" folder itself
	sharedEntry bool          // an item directly inside it
	name        string        // display name (last path element)
	res         access.Result // unset for virtual
}

func (d *davFS) deny() error {
	d.denied = true
	return os.ErrPermission
}

func davSegments(name string) []string {
	clean := path.Clean("/" + name)
	if clean == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(clean, "/"), "/")
}

type sharedEntry struct {
	name string
	res  access.Result
}

// sharedEntries lists what is shared with the user, with unique names.
func (d *davFS) sharedEntries(ctx context.Context) ([]sharedEntry, error) {
	roots, err := d.s.sharedRoots(ctx, d.u)
	if err != nil {
		return nil, err
	}
	count := map[string]int{}
	for _, r := range roots {
		count[r.Node.Name]++
	}
	out := make([]sharedEntry, 0, len(roots))
	for _, r := range roots {
		name := r.Node.Name
		if count[name] > 1 { // two people shared items with the same name
			ext := path.Ext(name)
			name = strings.TrimSuffix(name, ext) + " (" + r.Node.ID[:6] + ")" + ext
		}
		out = append(out, sharedEntry{name: name, res: r})
	}
	return out, nil
}

// resolve maps a path to a node the user may at least view. Anything else,
// including nodes that exist but are not accessible, is os.ErrNotExist.
func (d *davFS) resolve(ctx context.Context, name string) (davNode, error) {
	segs := davSegments(name)
	id, rest, out := d.u.RootID, segs, davNode{}
	if len(segs) > 0 {
		out.name = segs[len(segs)-1]
	}
	if len(segs) > 0 && segs[0] == davSharedName {
		if len(segs) == 1 {
			return davNode{virtual: true, name: davSharedName}, nil
		}
		entries, err := d.sharedEntries(ctx)
		if err != nil {
			return out, err
		}
		id = ""
		for _, e := range entries {
			if e.name == segs[1] {
				id = e.res.Node.ID
			}
		}
		if id == "" {
			return out, os.ErrNotExist
		}
		rest, out.sharedEntry = segs[2:], len(segs) == 2
	}
	for _, seg := range rest {
		// Exact, case-sensitive match among live children only.
		err := d.s.db.QueryRowContext(ctx, `SELECT id FROM nodes WHERE parent_id = ? AND name = ? AND trashed_at IS NULL`, id, seg).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return out, os.ErrNotExist
		}
		if err != nil {
			return out, fmt.Errorf("dav lookup: %w", err)
		}
	}
	// The one authorization decision, same as the JSON API.
	res, err := access.Resolve(ctx, d.s.db, d.p, id)
	if errors.Is(err, access.ErrNotFound) {
		return out, os.ErrNotExist
	}
	if err != nil {
		return out, err
	}
	if res.Level == access.None || res.Trashed {
		return out, os.ErrNotExist
	}
	out.res = res
	return out, nil
}

// resolveParent returns the folder a new entry would be created in, checked
// for Editor, plus the validated entry name.
func (d *davFS) resolveParent(ctx context.Context, name string) (davNode, string, error) {
	segs := davSegments(name)
	if len(segs) == 0 {
		return davNode{}, "", d.deny()
	}
	parent, err := d.resolve(ctx, "/"+strings.Join(segs[:len(segs)-1], "/"))
	if err != nil {
		return parent, "", err
	}
	if parent.virtual {
		return parent, "", d.deny() // nothing can be created in the virtual folder
	}
	if !parent.res.Node.IsDir {
		return parent, "", os.ErrNotExist
	}
	if parent.res.Level < access.Editor {
		return parent, "", d.deny()
	}
	base, ok := validName(segs[len(segs)-1])
	if !ok || base != segs[len(segs)-1] || reservedName(parent.res.Node, base) {
		return parent, "", d.deny()
	}
	return parent, base, nil
}

// mutable applies the same rule as authzMutate: Editor on the node and on
// its parent; never a root, the virtual folder, or a shared item itself.
func (d *davFS) mutable(ctx context.Context, n davNode) error {
	if n.virtual || n.sharedEntry || n.res.Node.ParentID == "" || n.res.Level < access.Editor {
		return d.deny()
	}
	parent, err := access.Resolve(ctx, d.s.db, d.p, n.res.Node.ParentID)
	if err != nil {
		return err
	}
	if parent.Level < access.Editor || parent.Trashed {
		return d.deny()
	}
	return nil
}

// checkDestination vets COPY/MOVE before the generic handler runs: the
// destination must be inside /dav, not inside the source, and owned by the
// same person as the source. It returns 0 to continue or an HTTP status.
func (d *davFS) checkDestination(r *http.Request, srcRel string) int {
	u, err := url.Parse(r.Header.Get("Destination"))
	if err != nil || r.Header.Get("Destination") == "" {
		return http.StatusBadRequest
	}
	if u.Host != "" && u.Host != r.Host {
		return http.StatusBadGateway
	}
	if u.Path != davPrefix && !strings.HasPrefix(u.Path, davPrefix+"/") {
		return http.StatusForbidden
	}
	src := path.Clean("/" + srcRel)
	dst := path.Clean("/" + strings.TrimPrefix(u.Path, davPrefix))
	if dst == src || strings.HasPrefix(dst, src+"/") || src == "/" {
		return http.StatusForbidden
	}
	sn, err := d.resolve(r.Context(), src)
	if err != nil {
		return http.StatusNotFound
	}
	if sn.virtual {
		return http.StatusForbidden
	}
	parent, _, err := d.resolveParent(r.Context(), dst)
	if errors.Is(err, os.ErrNotExist) {
		return http.StatusConflict
	}
	if err != nil {
		return http.StatusForbidden
	}
	// Neither MOVE nor COPY crosses owners over WebDAV.
	if parent.res.Node.OwnerID != sn.res.Node.OwnerID {
		return http.StatusForbidden
	}
	return 0
}

func (d *davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	n, err := d.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return d.info(n), nil
}

func (d *davFS) info(n davNode) *davInfo {
	if n.virtual {
		return &davInfo{name: davSharedName, dir: true, mod: time.Unix(0, 0)}
	}
	return nodeInfo(n.res.Node, n.name)
}

func nodeInfo(n *access.Node, name string) *davInfo {
	if name == "" {
		name = n.Name
	}
	return &davInfo{name: name, size: n.Size, dir: n.IsDir, mod: time.Unix(n.UpdatedAt, 0), sha: n.BlobSHA}
}

func (d *davFS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	parent, base, err := d.resolveParent(ctx, name)
	if err != nil {
		return err
	}
	if len(parent.res.Chain) >= maxFolderDepth {
		return d.deny()
	}
	now := d.s.now()
	_, err = d.s.db.ExecContext(ctx, `INSERT INTO nodes(id, owner_id, parent_id, name, is_dir, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?)`,
		randomHex(16), parent.res.Node.OwnerID, parent.res.Node.ID, base, now, now)
	if isUniqueViolation(err) {
		return os.ErrExist
	}
	if err != nil {
		return fmt.Errorf("dav mkdir: %w", err)
	}
	return nil
}

// RemoveAll moves the node to the trash. It never deletes anything.
func (d *davFS) RemoveAll(ctx context.Context, name string) error {
	n, err := d.resolve(ctx, name)
	if err != nil {
		return err
	}
	if err := d.mutable(ctx, n); err != nil {
		return err
	}
	if _, err := d.s.db.ExecContext(ctx, `UPDATE nodes SET trashed_at = ?, trashed_by = ? WHERE id = ? AND trashed_at IS NULL`,
		d.s.now(), d.u.ID, n.res.Node.ID); err != nil {
		return fmt.Errorf("dav trash: %w", err)
	}
	d.s.log.Info("trashed", "node", n.res.Node.ID, "by", d.u.Email, "via", "webdav")
	return nil
}

func (d *davFS) Rename(ctx context.Context, oldName, newName string) error {
	src, err := d.resolve(ctx, oldName)
	if err != nil {
		return err
	}
	if err := d.mutable(ctx, src); err != nil {
		return err
	}
	parent, base, err := d.resolveParent(ctx, newName)
	if err != nil {
		return err
	}
	node := src.res.Node
	if parent.res.Node.OwnerID != node.OwnerID {
		return d.deny() // a move never takes a file away from its owner
	}
	tx, err := d.s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dav rename: %w", err)
	}
	defer tx.Rollback()
	inside, err := isInSubtree(ctx, tx, node.ID, parent.res.Node.ID)
	if err != nil {
		return err
	}
	if inside {
		return d.deny()
	}
	var height int
	if err := tx.QueryRowContext(ctx, `
		WITH RECURSIVE t(id, d) AS (
			SELECT ?, 1 UNION ALL
			SELECT n.id, t.d + 1 FROM nodes n JOIN t ON n.parent_id = t.id WHERE t.d < 400
		) SELECT MAX(d) FROM t`, node.ID).Scan(&height); err != nil {
		return fmt.Errorf("dav rename: %w", err)
	}
	if len(parent.res.Chain)+height > maxFolderDepth+1 {
		return d.deny()
	}
	res, err := tx.ExecContext(ctx, `UPDATE nodes SET name = ?, parent_id = ? WHERE id = ? AND trashed_at IS NULL`, base, parent.res.Node.ID, node.ID)
	if isUniqueViolation(err) {
		return os.ErrExist
	}
	if err != nil {
		return fmt.Errorf("dav rename: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return os.ErrNotExist
	}
	return tx.Commit()
}

func (d *davFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) == 0 {
		n, err := d.resolve(ctx, name)
		if err != nil {
			return nil, err
		}
		return &davReadFile{fs: d, ctx: ctx, n: n}, nil
	}
	parent, base, err := d.resolveParent(ctx, name)
	if err != nil {
		return nil, err
	}
	existed := false
	if n, err := d.resolve(ctx, name); err == nil {
		if n.res.Node.IsDir {
			return nil, d.deny()
		}
		if n.res.Level < access.Editor {
			return nil, d.deny()
		}
		existed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else if flag&os.O_CREATE == 0 {
		return nil, os.ErrNotExist
	}
	if len(parent.res.Chain) > maxFolderDepth {
		return nil, d.deny()
	}
	tmp := filepath.Join(d.s.tmpDir, "dav-"+randomHex(16)+".part")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("dav create temp file: %w", err)
	}
	return &davWriteFile{
		fs: d, parentID: parent.res.Node.ID, name: base, tmp: tmp, f: f, h: sha256.New(),
		// Opened without O_TRUNC on an existing file (a LOCK probe): only
		// replace the content if something is actually written.
		replace: !existed || flag&os.O_TRUNC != 0,
	}, nil
}

// ---- FileInfo ----

type davInfo struct {
	name string
	size int64
	dir  bool
	mod  time.Time
	sha  string
}

func (i *davInfo) Name() string { return i.name }
func (i *davInfo) Size() int64  { return i.size }
func (i *davInfo) Mode() os.FileMode {
	if i.dir {
		return os.ModeDir | 0o755
	}
	return 0o644
}
func (i *davInfo) ModTime() time.Time { return i.mod }
func (i *davInfo) IsDir() bool        { return i.dir }
func (i *davInfo) Sys() any           { return nil }

// ETag makes the content hash the ETag, so clients detect changes exactly.
func (i *davInfo) ETag(context.Context) (string, error) {
	if i.dir || i.sha == "" {
		return "", webdav.ErrNotImplemented
	}
	return `"` + i.sha + `"`, nil
}

// ContentType answers without opening the file (and without sniffing).
func (i *davInfo) ContentType(context.Context) (string, error) {
	ext := extOf(i.name)
	if t, ok := inlineTypes[ext]; ok {
		return t.ctype, nil
	}
	if textExts[ext] {
		return "text/plain; charset=utf-8", nil
	}
	if t, ok := downloadTypes[ext]; ok {
		return t, nil
	}
	return "application/octet-stream", nil
}

// ---- reading ----

type davReadFile struct {
	fs   *davFS
	ctx  context.Context
	n    davNode
	f    *os.File
	list []os.FileInfo
	pos  int
}

func (r *davReadFile) Stat() (os.FileInfo, error) { return r.fs.info(r.n), nil }
func (r *davReadFile) Write([]byte) (int, error)  { return 0, os.ErrPermission }
func (r *davReadFile) isDir() bool                { return r.n.virtual || r.n.res.Node.IsDir }
func (r *davReadFile) open() error {
	if r.f != nil {
		return nil
	}
	if r.isDir() {
		return fs.ErrInvalid
	}
	f, err := r.fs.s.blobs.Open(r.n.res.Node.BlobSHA)
	if err != nil {
		return fmt.Errorf("dav open blob: %w", err)
	}
	r.f = f
	return nil
}

func (r *davReadFile) Read(p []byte) (int, error) {
	if err := r.open(); err != nil {
		return 0, err
	}
	return r.f.Read(p)
}

func (r *davReadFile) Seek(offset int64, whence int) (int64, error) {
	if err := r.open(); err != nil {
		return 0, err
	}
	return r.f.Seek(offset, whence)
}

func (r *davReadFile) Close() error {
	if r.f != nil {
		return r.f.Close()
	}
	return nil
}

// Readdir lists a folder. The folder itself was authorized by resolve;
// children inherit at least that access.
func (r *davReadFile) Readdir(count int) ([]os.FileInfo, error) {
	if !r.isDir() {
		return nil, fs.ErrInvalid
	}
	if r.list == nil {
		r.list = []os.FileInfo{}
		if r.n.virtual {
			entries, err := r.fs.sharedEntries(r.ctx)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				r.list = append(r.list, nodeInfo(e.res.Node, e.name))
			}
		} else {
			rows, err := r.fs.s.db.QueryContext(r.ctx, `SELECT `+access.NodeColumns+` FROM nodes WHERE parent_id = ? AND trashed_at IS NULL ORDER BY name`, r.n.res.Node.ID)
			if err != nil {
				return nil, fmt.Errorf("dav readdir: %w", err)
			}
			for rows.Next() {
				n, err := access.ScanNode(rows)
				if err != nil {
					rows.Close()
					return nil, fmt.Errorf("dav readdir: %w", err)
				}
				r.list = append(r.list, nodeInfo(n, ""))
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("dav readdir: %w", err)
			}
			if r.n.res.Node.ID == r.fs.u.RootID {
				r.list = append(r.list, &davInfo{name: davSharedName, dir: true, mod: time.Unix(0, 0)})
			}
		}
	}
	rest := r.list[r.pos:]
	if count <= 0 {
		r.pos = len(r.list)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	if count > len(rest) {
		count = len(rest)
	}
	r.pos += count
	return rest[:count], nil
}

// ---- writing ----

// davWriteFile receives a PUT (or the destination of a COPY) into a temp
// file while hashing it, and commits it as a node when closed. If the body
// was cut short nothing is committed and the temp file is removed.
type davWriteFile struct {
	fs       *davFS
	parentID string
	name     string
	tmp      string
	f        *os.File
	h        hash.Hash
	size     int64
	failed   bool
	replace  bool
	done     bool
}

func (w *davWriteFile) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	w.h.Write(p[:n])
	w.size += int64(n)
	w.replace = true
	if err != nil {
		w.failed = true
	}
	return n, err
}

// ReadFrom is what io.Copy uses for a PUT body: it lets us see a read error
// (client went away, body shorter than Content-Length) and refuse to commit.
func (w *davWriteFile) ReadFrom(r io.Reader) (int64, error) {
	n, err := io.Copy(io.MultiWriter(w.f, w.h), r)
	w.size += n
	if n > 0 {
		w.replace = true
	}
	if err != nil {
		w.failed = true
	}
	return n, err
}

func (w *davWriteFile) Read([]byte) (int, error)           { return 0, os.ErrPermission }
func (w *davWriteFile) Seek(int64, int) (int64, error)     { return 0, os.ErrPermission }
func (w *davWriteFile) Readdir(int) ([]os.FileInfo, error) { return nil, fs.ErrInvalid }
func (w *davWriteFile) Stat() (os.FileInfo, error) {
	mod := w.fs.s.Now()
	if w.fs.mtime != 0 {
		mod = time.Unix(w.fs.mtime, 0)
	}
	return &davInfo{name: w.name, size: w.size, mod: mod, sha: hex.EncodeToString(w.h.Sum(nil))}, nil
}

func (w *davWriteFile) Close() error {
	if w.done {
		return nil
	}
	w.done = true
	err := w.f.Close()
	if err != nil || w.failed {
		os.Remove(w.tmp)
		if err == nil {
			err = errors.New("dav: upload was interrupted; nothing was saved")
		}
		return err
	}
	if !w.replace {
		os.Remove(w.tmp)
		return nil
	}
	if err := w.fs.commit(w); err != nil {
		os.Remove(w.tmp)
		return err
	}
	return nil
}

// commit turns a finished temp file into a node. It is the WebDAV twin of
// upload completion and keeps the same invariants:
//   - the blob is referenced (+1) in the same transaction as the node row;
//   - an existing file keeps its node id (so shares and links survive) and
//     its previous content becomes a trashed sibling row that still
//     references the old blob, so an overwrite is always recoverable from
//     the trash (empty previous content is not kept: nothing to recover);
//   - identical content is a no-op.
func (d *davFS) commit(w *davWriteFile) error {
	s := d.s
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	parent, err := access.Resolve(ctx, s.db, d.p, w.parentID)
	if err != nil || parent.Level < access.Editor || parent.Trashed || !parent.Node.IsDir {
		return d.deny()
	}
	sum := hex.EncodeToString(w.h.Sum(nil))
	now := s.now()
	mtime := now
	if d.mtime != 0 {
		mtime = d.mtime
	}

	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dav commit: %w", err)
	}
	defer tx.Rollback()

	old, err := access.ScanNode(tx.QueryRowContext(ctx, `SELECT `+access.NodeColumns+` FROM nodes WHERE parent_id = ? AND name = ? AND trashed_at IS NULL`, parent.Node.ID, w.name))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("dav commit: %w", err)
	}
	if err == nil {
		if old.IsDir {
			return d.deny()
		}
		if old.BlobSHA == sum {
			if d.mtime != 0 && old.UpdatedAt != mtime {
				if _, err := tx.ExecContext(ctx, `UPDATE nodes SET updated_at = ? WHERE id = ?`, mtime, old.ID); err != nil {
					return fmt.Errorf("dav commit: %w", err)
				}
			}
			os.Remove(w.tmp)
			return tx.Commit()
		}
	} else {
		old = nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO blobs(sha256, size, refcount, created_at) VALUES (?, ?, 1, ?)
		ON CONFLICT(sha256) DO UPDATE SET refcount = refcount + 1`, sum, w.size, now); err != nil {
		return fmt.Errorf("dav commit: reference blob: %w", err)
	}
	if old != nil {
		if old.Size > 0 {
			// Previous version: a trashed row taking over the old blob reference.
			if _, err := tx.ExecContext(ctx, `INSERT INTO nodes(id, owner_id, parent_id, name, is_dir, blob_sha, size, created_at, updated_at, trashed_at, trashed_by)
				VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?)`, randomHex(16), old.OwnerID, old.ParentID, old.Name, old.BlobSHA, old.Size,
				old.CreatedAt, old.UpdatedAt, now, d.u.ID); err != nil {
				return fmt.Errorf("dav commit: keep previous version: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE blobs SET refcount = refcount - 1 WHERE sha256 = ?`, old.BlobSHA); err != nil {
			return fmt.Errorf("dav commit: unreference empty blob: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE nodes SET blob_sha = ?, size = ?, updated_at = ? WHERE id = ?`, sum, w.size, mtime, old.ID); err != nil {
			return fmt.Errorf("dav commit: %w", err)
		}
	} else {
		_, err := tx.ExecContext(ctx, `INSERT INTO nodes(id, owner_id, parent_id, name, is_dir, blob_sha, size, created_at, updated_at)
			VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?)`, randomHex(16), parent.Node.OwnerID, parent.Node.ID, w.name, sum, w.size, now, mtime)
		if err != nil {
			return fmt.Errorf("dav commit: %w", err)
		}
	}
	// Durable in the blob store before the database says it exists.
	if err := s.blobs.Ingest(w.tmp, sum, w.size); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dav commit: %w", err)
	}
	return nil
}
