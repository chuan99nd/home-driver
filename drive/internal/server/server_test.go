package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chuan99nd/home-driver/drive/internal/config"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func stringsReader(s string) io.Reader { return strings.NewReader(s) }

// ---- login rules ----

func TestLoginAllowlist(t *testing.T) {
	e := newEnv(t)
	// Admin emails are matched case-insensitively.
	admin := e.login("ADMIN@Example.com")

	if _, ok := e.tryLogin("stranger@example.com"); ok {
		t.Fatal("stranger could sign in")
	}
	// A refused login leaves no account behind.
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM users WHERE email = 'stranger@example.com'`).Scan(&n)
	if n != 0 {
		t.Errorf("refused login created a user")
	}

	// Allowlist, managed by admins only.
	admin.call("POST", "/api/admin/allowlist", map[string]string{"email": " Friend@Example.COM "}, 200, nil)
	friend := e.login("friend@example.com")
	friend.get("/api/admin/allowlist", 403, nil)
	friend.call("POST", "/api/admin/allowlist", map[string]string{"email": "x@example.com"}, 403, nil)
	friend.get("/api/nodes/"+friend.root, 200, nil)

	// Removing the entry ends the existing session immediately.
	admin.call("DELETE", "/api/admin/allowlist?email=friend@example.com", nil, 200, nil)
	friend.get("/api/nodes/"+friend.root, 401, nil)
	if _, ok := e.tryLogin("friend@example.com"); ok {
		t.Error("removed account could sign in again")
	}

	// A share addressed to an email grants login; revoking it removes it.
	folder := admin.mkdir(admin.root, "Photos")
	admin.share(folder, "Guest@example.com", "viewer")
	guest := e.login("guest@EXAMPLE.com")
	guest.get("/api/nodes/"+folder, 200, nil)
	var shares struct {
		Shares []shareJSON `json:"shares"`
	}
	admin.get("/api/nodes/"+folder+"/shares", 200, &shares)
	if len(shares.Shares) != 1 || shares.Shares[0].Email != "guest@example.com" {
		t.Fatalf("shares: %+v", shares)
	}
	admin.call("DELETE", "/api/shares/"+itoa(shares.Shares[0].ID), nil, 200, nil)
	guest.get("/api/nodes/"+folder, 401, nil)
	if _, ok := e.tryLogin("guest@example.com"); ok {
		t.Error("guest could sign in after the share was revoked")
	}

	// Logout kills the session server-side.
	admin.call("POST", "/api/logout", nil, 200, nil)
	admin.get("/api/nodes/"+admin.root, 401, nil)

	// Bad input.
	admin = e.login("admin@example.com")
	admin.call("POST", "/api/admin/allowlist", map[string]string{"email": "not an email"}, 400, nil)
	admin.call("POST", "/api/nodes/"+folder+"/shares", map[string]string{"email": "a@b", "role": "owner"}, 400, nil)
	admin.call("POST", "/api/nodes/"+folder+"/shares", map[string]string{"email": "admin@example.com", "role": "viewer"}, 400, nil)
	admin.call("POST", "/api/nodes/"+admin.root+"/shares", map[string]string{"email": "x@example.com", "role": "viewer"}, 400, nil)
}

func TestSessionCookieFlags(t *testing.T) {
	e := newEnv(t)
	c := e.anon()
	resp, _ := c.raw("GET", "/dev/login?email=admin@example.com", nil, nil)
	var sc *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie {
			sc = ck
		}
	}
	if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode || sc.Secure {
		t.Fatalf("session cookie: %+v", sc)
	}
	// The raw token is not what is stored.
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, sc.Value).Scan(&n)
	if n != 0 {
		t.Error("session token stored in clear")
	}
	// Sessions expire.
	e.advance(sessionTTL + time.Hour)
	c.get("/api/nodes/x", 401, nil)

	https := config.Config{BaseURL: "https://drive.example.com"}
	if !https.Secure() {
		t.Error("https base URL must give Secure cookies")
	}
}

func TestDevLoginRouteAbsentByDefault(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.DevLogin = false
		c.GoogleClientID, c.GoogleClientSecret = "id", "secret"
	})
	resp, _ := e.anon().raw("GET", "/dev/login?email=admin@example.com", nil, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("dev login without DRIVE_DEV_LOGIN: %d", resp.StatusCode)
	}
}

// ---- CSRF ----

func TestCSRF(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	body := func() io.Reader { return strings.NewReader(`{"name":"x"}`) }
	path := "/api/nodes/" + c.root + "/children"

	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"no custom header", map[string]string{csrfHeader: "-"}, 403},
		{"foreign origin", map[string]string{"Origin": "https://evil.example"}, 403},
		{"null origin", map[string]string{"Origin": "null"}, 403},
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"same-site but other origin", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"form post style", map[string]string{csrfHeader: "-", "Content-Type": "application/x-www-form-urlencoded", "Origin": e.ts.URL}, 403},
		{"same origin", map[string]string{"Origin": e.ts.URL, "Sec-Fetch-Site": "same-origin"}, 201},
	}
	for _, tc := range cases {
		resp, b := c.raw("POST", path, body(), tc.hdr)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, resp.StatusCode, tc.want, b)
		}
	}
	if names := c.names(c.root); len(names) != 1 {
		t.Errorf("children after CSRF attempts: %v", names)
	}
	// Every state-changing method is covered.
	for _, m := range []string{"PATCH", "DELETE", "PUT"} {
		resp, _ := c.raw(m, "/api/nodes/"+c.root, nil, map[string]string{csrfHeader: "-"})
		if resp.StatusCode != 403 {
			t.Errorf("%s without header: %d", m, resp.StatusCode)
		}
	}
	resp, _ := c.raw("POST", "/api/logout", nil, map[string]string{csrfHeader: "-"})
	if resp.StatusCode != 403 {
		t.Errorf("logout without header: %d", resp.StatusCode)
	}
}

func TestJSONBodyLimit(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	big := `{"name":"` + strings.Repeat("a", maxJSONBody+10) + `"}`
	resp, _ := c.raw("POST", "/api/nodes/"+c.root+"/children", strings.NewReader(big), nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized JSON: %d", resp.StatusCode)
	}
	resp, _ = c.raw("POST", "/api/nodes/"+c.root+"/children", strings.NewReader(`{"name":"a","extra":1}`), nil)
	if resp.StatusCode != 400 {
		t.Errorf("unknown field: %d", resp.StatusCode)
	}
	resp, _ = c.raw("POST", "/api/nodes/"+c.root+"/children", strings.NewReader(`not json`), nil)
	if resp.StatusCode != 400 {
		t.Errorf("bad JSON: %d", resp.StatusCode)
	}
}

// ---- uploads ----

func TestChunkedUpload(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	data := pattern(3*testChunk+100, 1) // 4 chunks, the last one short

	var init struct {
		ID        string `json:"id"`
		ChunkSize int    `json:"chunk_size"`
	}
	c.call("POST", "/api/uploads", map[string]any{"parent_id": c.root, "name": "big.bin", "size": len(data), "sha256": sha(data)}, 201, &init)
	if init.ChunkSize != testChunk {
		t.Fatalf("chunk size %d", init.ChunkSize)
	}
	put := func(off int, body []byte) (int, map[string]any) {
		resp, b := c.raw("PUT", fmt.Sprintf("/api/uploads/%s/chunk?offset=%d", init.ID, off), bytes.NewReader(body), nil)
		var m map[string]any
		_ = jsonUnmarshal(b, &m)
		return resp.StatusCode, m
	}
	status := func() int {
		var s struct {
			Offset int `json:"offset"`
		}
		c.get("/api/uploads/"+init.ID, 200, &s)
		return s.Offset
	}

	// Chunk 0.
	if code, _ := put(0, data[:testChunk]); code != 200 {
		t.Fatalf("chunk 0: %d", code)
	}
	// Out of order: the server says where to continue.
	if code, m := put(2*testChunk, data[2*testChunk:3*testChunk]); code != 409 || int(m["offset"].(float64)) != testChunk {
		t.Fatalf("out-of-order chunk: %d %v", code, m)
	}
	// Replaying an old chunk is also out of order.
	if code, _ := put(0, data[:testChunk]); code != 409 {
		t.Fatalf("replayed chunk: %d", code)
	}
	// Wrong sizes: short, long, and beyond the 1-chunk limit.
	if code, _ := put(testChunk, data[testChunk:testChunk+10]); code != 400 {
		t.Fatalf("short chunk: %d", code)
	}
	if code, _ := put(testChunk, data[testChunk:2*testChunk+1]); code != 400 {
		t.Fatalf("long chunk: %d", code)
	}
	if code, _ := put(testChunk, data[testChunk:]); code != 400 {
		t.Fatalf("whole rest in one chunk: %d", code)
	}
	if got := status(); got != testChunk {
		t.Fatalf("offset after rejected chunks: %d", got)
	}
	// A chunk whose body is cut off mid-way (no Content-Length, short body).
	resp, _ := c.raw("PUT", fmt.Sprintf("/api/uploads/%s/chunk?offset=%d", init.ID, testChunk), io.MultiReader(bytes.NewReader(data[testChunk:testChunk+300])), nil)
	if resp.StatusCode != 400 {
		t.Fatalf("truncated chunk: %d", resp.StatusCode)
	}
	// Completing early is refused.
	c.call("POST", "/api/uploads/"+init.ID+"/complete", nil, 409, nil)

	// "Resume": a fresh client asks for the offset and sends the rest.
	off := status()
	if off != testChunk {
		t.Fatalf("resume offset %d", off)
	}
	for ; off < len(data); off += testChunk {
		if code, m := put(off, data[off:min(off+testChunk, len(data))]); code != 200 {
			t.Fatalf("chunk at %d: %d %v", off, code, m)
		}
	}
	// Nothing more is accepted.
	if code, _ := put(len(data), []byte("x")); code != 409 {
		t.Fatalf("chunk after the end: %d", code)
	}
	var done nodeResp
	c.call("POST", "/api/uploads/"+init.ID+"/complete", nil, 201, &done)
	if done.SHA256 != sha(data) || done.Node.Size != int64(len(data)) || done.Node.Name != "big.bin" {
		t.Fatalf("complete: %+v", done)
	}
	// Stored content-addressed, byte for byte, and the temp file is gone.
	got, err := os.ReadFile(e.blobPath(sha(data)))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("blob content mismatch: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dir, "tmp")); len(entries) != 0 {
		t.Errorf("tmp not empty: %v", entries)
	}
	c.get("/api/uploads/"+init.ID, 404, nil)
	resp, body := c.content("/api/nodes/"+done.Node.ID+"/content", nil)
	if resp.StatusCode != 200 || !bytes.Equal(body, data) {
		t.Fatalf("download mismatch: %d", resp.StatusCode)
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func TestUploadHashMismatch(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	data := []byte("the real content")
	var init struct {
		ID string `json:"id"`
	}
	c.call("POST", "/api/uploads", map[string]any{"parent_id": c.root, "name": "f.txt", "size": len(data), "sha256": sha([]byte("something else"))}, 201, &init)
	if resp, _ := c.raw("PUT", "/api/uploads/"+init.ID+"/chunk?offset=0", bytes.NewReader(data), nil); resp.StatusCode != 200 {
		t.Fatal("chunk failed")
	}
	c.call("POST", "/api/uploads/"+init.ID+"/complete", nil, 422, nil)
	if len(c.names(c.root)) != 0 {
		t.Error("corrupt upload created a file")
	}
	if e.blobExists(sha(data)) {
		t.Error("corrupt upload was stored as a blob")
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dir, "tmp")); len(entries) != 0 {
		t.Errorf("tmp not cleaned: %v", entries)
	}
	c.get("/api/uploads/"+init.ID, 404, nil)

	// Hash given only at completion.
	c.call("POST", "/api/uploads", map[string]any{"parent_id": c.root, "name": "g.txt", "size": len(data)}, 201, &init)
	c.raw("PUT", "/api/uploads/"+init.ID+"/chunk?offset=0", bytes.NewReader(data), nil)
	c.call("POST", "/api/uploads/"+init.ID+"/complete", map[string]string{"sha256": strings.Repeat("0", 64)}, 422, nil)

	// Bad init parameters.
	c.call("POST", "/api/uploads", map[string]any{"parent_id": c.root, "name": "a/b", "size": 1}, 400, nil)
	c.call("POST", "/api/uploads", map[string]any{"parent_id": c.root, "name": "a", "size": -1}, 400, nil)
	c.call("POST", "/api/uploads", map[string]any{"parent_id": c.root, "name": "a"}, 400, nil)
	c.call("POST", "/api/uploads", map[string]any{"parent_id": c.root, "name": "a", "size": 1, "sha256": "xyz"}, 400, nil)
	c.call("POST", "/api/uploads", map[string]any{"parent_id": "nope", "name": "a", "size": 1}, 404, nil)
	file := c.upload(c.root, "plain.txt", []byte("x"))
	c.call("POST", "/api/uploads", map[string]any{"parent_id": file.ID, "name": "a", "size": 1}, 400, nil)
}

func TestEmptyFileAndAbortAndStale(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	empty := c.upload(c.root, "empty.txt", nil)
	if empty.Size != 0 {
		t.Fatalf("empty: %+v", empty)
	}
	resp, body := c.content("/api/nodes/"+empty.ID+"/content", nil)
	if resp.StatusCode != 200 || len(body) != 0 {
		t.Fatalf("empty download: %d %q", resp.StatusCode, body)
	}

	var init struct {
		ID string `json:"id"`
	}
	c.call("POST", "/api/uploads", map[string]any{"parent_id": c.root, "name": "a.bin", "size": 10}, 201, &init)
	c.call("DELETE", "/api/uploads/"+init.ID, nil, 200, nil)
	if _, err := os.Stat(e.srv.partPath(init.ID)); !os.IsNotExist(err) {
		t.Error("abort left the temp file")
	}

	// Stale sessions are cleaned up; fresh ones are kept.
	var old, fresh struct {
		ID string `json:"id"`
	}
	c.call("POST", "/api/uploads", map[string]any{"parent_id": c.root, "name": "old.bin", "size": 10}, 201, &old)
	stray := filepath.Join(e.dir, "tmp", strings.Repeat("a", 32)+".part")
	os.WriteFile(stray, []byte("x"), 0o600)
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(stray, past, past)
	e.advance(uploadStaleAfter + time.Hour)
	c.call("POST", "/api/uploads", map[string]any{"parent_id": c.root, "name": "fresh.bin", "size": 10}, 201, &fresh)
	if err := e.srv.cleanStaleUploads(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.get("/api/uploads/"+old.ID, 404, nil)
	c.get("/api/uploads/"+fresh.ID, 200, nil)
	if _, err := os.Stat(e.srv.partPath(old.ID)); !os.IsNotExist(err) {
		t.Error("stale temp file kept")
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Error("stray temp file kept")
	}
	if _, err := os.Stat(e.srv.partPath(fresh.ID)); err != nil {
		t.Error("fresh temp file removed")
	}
}

// ---- dedup, refcounts, trash, purge ----

func TestDedupAndRefcount(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	data := pattern(2500, 9)
	sum := sha(data)

	a := c.upload(c.root, "a.bin", data)
	b := c.upload(c.root, "b.bin", data)
	if e.refcount(sum) != 2 {
		t.Fatalf("refcount after two uploads: %d", e.refcount(sum))
	}
	var files int
	filepath.WalkDir(filepath.Join(e.dir, "blobs"), func(_ string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			files++
		}
		return nil
	})
	if files != 1 {
		t.Fatalf("blob files on disk: %d, want 1", files)
	}
	var cp nodeResp
	c.call("POST", "/api/nodes/"+a.ID+"/copy", map[string]string{"parent_id": c.root}, 201, &cp)
	if cp.Node.Name != "a (1).bin" || e.refcount(sum) != 3 {
		t.Fatalf("copy: %+v refcount %d", cp.Node, e.refcount(sum))
	}

	// Trashing does not change references.
	c.call("DELETE", "/api/nodes/"+a.ID, nil, 200, nil)
	c.call("DELETE", "/api/nodes/"+b.ID, nil, 200, nil)
	c.call("DELETE", "/api/nodes/"+cp.Node.ID, nil, 200, nil)
	if e.refcount(sum) != 3 || !e.blobExists(sum) {
		t.Fatalf("after trash: refcount %d exists %v", e.refcount(sum), e.blobExists(sum))
	}
	// Purge two of three: the blob survives because a trashed row still references it.
	c.call("DELETE", "/api/trash/"+a.ID, nil, 200, nil)
	c.call("DELETE", "/api/trash/"+cp.Node.ID, nil, 200, nil)
	if e.refcount(sum) != 1 || !e.blobExists(sum) {
		t.Fatalf("after partial purge: refcount %d exists %v", e.refcount(sum), e.blobExists(sum))
	}
	// The remaining one is restorable and intact.
	c.call("POST", "/api/trash/"+b.ID+"/restore", nil, 200, nil)
	if _, body := c.content("/api/nodes/"+b.ID+"/content", nil); !bytes.Equal(body, data) {
		t.Fatal("restored file content differs")
	}
	// Purge the last reference: now, and only now, the file is removed.
	c.call("DELETE", "/api/nodes/"+b.ID, nil, 200, nil)
	c.call("DELETE", "/api/trash/"+b.ID, nil, 200, nil)
	if e.refcount(sum) != -1 || e.blobExists(sum) {
		t.Fatalf("after final purge: refcount %d exists %v", e.refcount(sum), e.blobExists(sum))
	}
	// Uploading the same content again works from scratch.
	again := c.upload(c.root, "again.bin", data)
	if _, body := c.content("/api/nodes/"+again.ID+"/content", nil); !bytes.Equal(body, data) || e.refcount(sum) != 1 {
		t.Fatal("re-upload after purge broken")
	}
}

func TestPurgeFolderWithSharedBlobs(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	shared, only := pattern(1500, 1), pattern(1500, 2)
	dir := c.mkdir(c.root, "dir")
	sub := c.mkdir(dir, "sub")
	c.upload(dir, "x.bin", shared)
	c.upload(sub, "y.bin", shared)
	c.upload(sub, "z.bin", only)
	keep := c.upload(c.root, "keep.bin", shared)

	// purge refuses anything that is not in the trash.
	if err := e.srv.purge(context.Background(), dir); !errors.Is(err, errNotTrashed) {
		t.Fatalf("purge of live node: %v", err)
	}
	if err := e.srv.purge(context.Background(), c.root); !errors.Is(err, errNotTrashed) {
		t.Fatalf("purge of root: %v", err)
	}
	c.call("DELETE", "/api/trash/"+dir, nil, 404, nil)

	c.call("DELETE", "/api/nodes/"+dir, nil, 200, nil)
	// A child of a trashed folder is not itself "in the trash".
	c.call("DELETE", "/api/trash/"+sub, nil, 404, nil)
	c.call("DELETE", "/api/trash/"+dir, nil, 200, nil)

	if e.refcount(sha(shared)) != 1 || !e.blobExists(sha(shared)) {
		t.Errorf("shared blob: refcount %d exists %v", e.refcount(sha(shared)), e.blobExists(sha(shared)))
	}
	if e.refcount(sha(only)) != -1 || e.blobExists(sha(only)) {
		t.Errorf("unique blob not removed")
	}
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&n)
	if n != 2 { // root + keep.bin
		t.Errorf("nodes left: %d", n)
	}
	if _, body := c.content("/api/nodes/"+keep.ID+"/content", nil); !bytes.Equal(body, shared) {
		t.Error("surviving file damaged")
	}
}

func TestTrashRestorePurge(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	dir := c.mkdir(c.root, "docs")
	f := c.upload(dir, "note.txt", []byte("note"))

	// Trash: gone from listings, direct access and search; present in trash.
	c.call("DELETE", "/api/nodes/"+f.ID, nil, 200, nil)
	if len(c.names(dir)) != 0 {
		t.Error("trashed file still listed")
	}
	c.get("/api/nodes/"+f.ID, 404, nil)
	if resp, _ := c.content("/api/nodes/"+f.ID+"/content", nil); resp.StatusCode != 404 {
		t.Error("trashed file still downloadable")
	}
	var items struct {
		Items []nodeJSON `json:"items"`
	}
	c.get("/api/search?q=note", 200, &items)
	if len(items.Items) != 0 {
		t.Error("trashed file found by search")
	}
	c.get("/api/trash", 200, &items)
	if len(items.Items) != 1 || items.Items[0].ID != f.ID {
		t.Fatalf("trash: %+v", items.Items)
	}
	// The name is free again while the old one sits in the trash.
	f2 := c.upload(dir, "note.txt", []byte("second"))
	if f2.Name != "note.txt" {
		t.Fatalf("name not reusable: %s", f2.Name)
	}
	// Restore to the original folder; the clash gets a suffix.
	c.call("POST", "/api/trash/"+f.ID+"/restore", nil, 200, nil)
	if names := c.names(dir); !contains(names, "note.txt") || !contains(names, "note (1).txt") {
		t.Fatalf("after restore: %v", names)
	}
	c.call("POST", "/api/trash/"+f.ID+"/restore", nil, 404, nil) // not in trash any more

	// Parent gone to the trash: the child is restored to the root.
	c.call("DELETE", "/api/nodes/"+f2.ID, nil, 200, nil)
	c.call("DELETE", "/api/nodes/"+dir, nil, 200, nil)
	var restored struct {
		ParentID    string `json:"parent_id"`
		MovedToRoot bool   `json:"moved_to_root"`
	}
	c.call("POST", "/api/trash/"+f2.ID+"/restore", nil, 200, &restored)
	if restored.ParentID != c.root || !restored.MovedToRoot || !contains(c.names(c.root), "note.txt") {
		t.Fatalf("restore to root: %+v %v", restored, c.names(c.root))
	}
	// Restoring the folder brings its remaining content back.
	c.call("POST", "/api/trash/"+dir+"/restore", nil, 200, nil)
	if names := c.names(dir); len(names) != 1 || names[0] != "note (1).txt" {
		t.Fatalf("folder after restore: %v", names)
	}
	// The root cannot be trashed.
	c.call("DELETE", "/api/nodes/"+c.root, nil, 400, nil)

	// Empty trash only touches the caller's trash.
	other := e.login("admin@example.com") // same user, second session
	_ = other
	c.call("POST", "/api/admin/allowlist", map[string]string{"email": "bob@example.com"}, 200, nil)
	bob := e.login("bob@example.com")
	bf := bob.upload(bob.root, "bob.txt", []byte("bob's"))
	bob.call("DELETE", "/api/nodes/"+bf.ID, nil, 200, nil)
	c.call("DELETE", "/api/nodes/"+dir, nil, 200, nil)
	c.call("DELETE", "/api/trash/"+bf.ID, nil, 404, nil) // not yours
	var emptied struct {
		Purged int `json:"purged"`
	}
	c.call("DELETE", "/api/trash", nil, 200, &emptied)
	if emptied.Purged != 1 {
		t.Errorf("purged %d", emptied.Purged)
	}
	bob.get("/api/trash", 200, &items)
	if len(items.Items) != 1 || !e.blobExists(sha([]byte("bob's"))) {
		t.Error("emptying one trash touched another user's")
	}
	if e.blobExists(sha([]byte("note"))) {
		t.Error("purged blob still on disk")
	}
}

func TestAutoPurge(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	old := c.upload(c.root, "old.txt", []byte("old data"))
	live := c.upload(c.root, "live.txt", []byte("live data"))
	c.call("DELETE", "/api/nodes/"+old.ID, nil, 200, nil)

	e.advance(29 * 24 * time.Hour)
	recent := c.upload(c.root, "recent.txt", []byte("recent data"))
	c.call("DELETE", "/api/nodes/"+recent.ID, nil, 200, nil)
	if n, err := e.srv.purgeExpired(context.Background()); err != nil || n != 0 {
		t.Fatalf("purge before the deadline: %d %v", n, err)
	}
	e.advance(2 * 24 * time.Hour)
	if n, err := e.srv.purgeExpired(context.Background()); err != nil || n != 1 {
		t.Fatalf("purge after the deadline: %d %v", n, err)
	}
	if e.blobExists(sha([]byte("old data"))) {
		t.Error("expired trash not purged")
	}
	if !e.blobExists(sha([]byte("recent data"))) || !e.blobExists(sha([]byte("live data"))) {
		t.Error("purge removed data that was not expired trash")
	}
	c = e.login("admin@example.com") // the 30-day session expired with the fake clock
	c.get("/api/nodes/"+live.ID, 200, nil)

	// DRIVE_TRASH_DAYS=0 disables the automatic purge.
	e2 := newEnv(t, func(c *config.Config) { c.TrashDays = 0 })
	c2 := e2.login("admin@example.com")
	f := c2.upload(c2.root, "f.txt", []byte("forever"))
	c2.call("DELETE", "/api/nodes/"+f.ID, nil, 200, nil)
	e2.advance(1000 * 24 * time.Hour)
	if n, _ := e2.srv.purgeExpired(context.Background()); n != 0 || !e2.blobExists(sha([]byte("forever"))) {
		t.Error("auto purge ran with TrashDays=0")
	}
}

// Uploads of identical content racing with purges must never leave a file
// row pointing at a missing blob.
func TestConcurrentUploadAndPurge(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	data := pattern(3000, 5)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cl := e.login("admin@example.com")
			for j := 0; j < 4; j++ {
				n := cl.upload(cl.root, fmt.Sprintf("f-%d-%d.bin", i, j), data)
				if i%2 == 0 {
					cl.call("DELETE", "/api/nodes/"+n.ID, nil, 200, nil)
					cl.call("DELETE", "/api/trash/"+n.ID, nil, 200, nil)
				}
			}
		}(i)
	}
	wg.Wait()
	list := c.list(c.root)
	if len(list.Children) != 12 {
		t.Fatalf("files left: %d", len(list.Children))
	}
	if e.refcount(sha(data)) != 12 {
		t.Fatalf("refcount %d", e.refcount(sha(data)))
	}
	for _, ch := range list.Children {
		if _, body := c.content("/api/nodes/"+ch.ID+"/content", nil); !bytes.Equal(body, data) {
			t.Fatalf("%s unreadable after concurrent purge", ch.Name)
		}
	}
}

// ---- downloads ----

func TestRangeDownload(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	data := pattern(5000, 3)
	n := c.upload(c.root, "Ảnh gia đình \"2024\".jpg", data)
	path := "/api/nodes/" + n.ID + "/content"

	resp, body := c.content(path, nil)
	if resp.StatusCode != 200 || !bytes.Equal(body, data) {
		t.Fatalf("full download: %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag != `"`+sha(data)+`"` || resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Errorf("headers: etag %q accept-ranges %q type %q", etag, resp.Header.Get("Accept-Ranges"), resp.Header.Get("Content-Type"))
	}
	cd := resp.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, `inline; filename="`) || !strings.Contains(cd, `filename*=UTF-8''%E1%BA%A2nh%20gia%20%C4%91%C3%ACnh%20%222024%22.jpg`) {
		t.Errorf("content-disposition: %s", cd)
	}
	if strings.Count(cd, `"`) != 2 {
		t.Errorf("quote not escaped in fallback filename: %s", cd)
	}

	for _, tc := range []struct {
		rng        string
		start, end int
	}{{"bytes=0-9", 0, 10}, {"bytes=100-199", 100, 200}, {"bytes=4990-", 4990, 5000}, {"bytes=-10", 4990, 5000}} {
		resp, body := c.content(path, map[string]string{"Range": tc.rng})
		if resp.StatusCode != 206 || !bytes.Equal(body, data[tc.start:tc.end]) {
			t.Errorf("range %s: status %d, %d bytes", tc.rng, resp.StatusCode, len(body))
		}
		if want := fmt.Sprintf("bytes %d-%d/5000", tc.start, tc.end-1); resp.Header.Get("Content-Range") != want {
			t.Errorf("range %s: Content-Range %q", tc.rng, resp.Header.Get("Content-Range"))
		}
	}
	if resp, _ := c.content(path, map[string]string{"Range": "bytes=9000-"}); resp.StatusCode != 416 {
		t.Errorf("unsatisfiable range: %d", resp.StatusCode)
	}
	if resp, _ := c.content(path, map[string]string{"If-None-Match": etag}); resp.StatusCode != 304 {
		t.Errorf("If-None-Match: %d", resp.StatusCode)
	}
	// If-Range with a stale validator falls back to the full body.
	if resp, body := c.content(path, map[string]string{"Range": "bytes=0-9", "If-Range": `"stale"`}); resp.StatusCode != 200 || len(body) != 5000 {
		t.Errorf("If-Range stale: %d %d", resp.StatusCode, len(body))
	}
	if resp, _ := c.content(path+"?download=1", nil); !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment;") {
		t.Errorf("download=1: %s", resp.Header.Get("Content-Disposition"))
	}
	// A folder has no content.
	dir := c.mkdir(c.root, "d")
	if resp, _ := c.content("/api/nodes/"+dir+"/content", nil); resp.StatusCode != 400 {
		t.Errorf("folder content: %d", resp.StatusCode)
	}
}

func TestActiveContentIsNeutralised(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	evil := []byte(`<html><script>alert(document.cookie)</script></html>`)
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><script>alert(1)</script></svg>`)

	cases := []struct {
		name string
		data []byte
	}{
		{"evil.html", evil}, {"evil.HTM", evil}, {"evil.xhtml", evil}, {"evil.svg", svg}, {"EVIL.SVG", svg},
		{"evil.xml", svg}, {"evil.js", []byte("alert(1)")}, {"evil.unknownext", evil}, {"noext", evil},
		{"evil.html.jpg", evil}, // sniffing must not turn this into HTML either
		{"evil.pdf", evil}, {"evil.mht", evil}, {"evil.swf", evil},
	}
	active := []string{"text/html", "image/svg", "xml", "javascript", "xhtml"}
	for _, tc := range cases {
		n := c.upload(c.root, tc.name, tc.data)
		for _, q := range []string{"", "?download=1"} {
			resp, _ := c.content("/api/nodes/"+n.ID+"/content"+q, nil)
			ct := resp.Header.Get("Content-Type")
			cd := resp.Header.Get("Content-Disposition")
			for _, bad := range active {
				if strings.Contains(strings.ToLower(ct), bad) {
					t.Errorf("%s%s served as active type %q", tc.name, q, ct)
				}
			}
			if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("%s%s: nosniff missing", tc.name, q)
			}
			csp := resp.Header.Get("Content-Security-Policy")
			if !strings.Contains(csp, "default-src 'none'") {
				t.Errorf("%s%s: CSP %q", tc.name, q, csp)
			}
			inline := strings.HasPrefix(cd, "inline")
			switch {
			case strings.HasPrefix(ct, "text/plain"), strings.HasPrefix(ct, "image/jpeg"):
				if !strings.Contains(csp, "sandbox") {
					t.Errorf("%s%s: inline content without CSP sandbox", tc.name, q)
				}
			case ct == "application/pdf":
				// PDFs render in the browser's viewer, never as script on our origin.
			default:
				if inline || ct != "application/octet-stream" {
					t.Errorf("%s%s: type %q disposition %q; want an octet-stream attachment", tc.name, q, ct, cd)
				}
			}
			if q != "" && inline {
				t.Errorf("%s: download=1 served inline", tc.name)
			}
		}
	}
	// The same guarantees hold through a public link.
	var link struct {
		Link linkJSON `json:"link"`
	}
	n := c.upload(c.root, "public.html", evil)
	c.call("POST", "/api/nodes/"+n.ID+"/links", map[string]any{}, 201, &link)
	token := link.Link.URL[strings.LastIndex(link.Link.URL, "/")+1:]
	resp, _ := e.anon().content("/api/public/"+token+"/nodes/"+n.ID+"/content", nil)
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != 200 || !strings.HasPrefix(ct, "text/plain") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("public html: %d %q", resp.StatusCode, ct)
	}
	// App pages carry a strict CSP without unsafe-inline for scripts.
	resp, _ = c.content("/", nil)
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("app CSP: %q", csp)
	}
}

func TestZipDownload(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	dir := c.mkdir(c.root, "album")
	sub := c.mkdir(dir, "sub")
	c.mkdir(dir, "empty")
	c.upload(dir, "a.txt", []byte("aaa"))
	c.upload(sub, "b.txt", []byte("bbbb"))
	gone := c.upload(sub, "trashed.txt", []byte("gone"))
	c.call("DELETE", "/api/nodes/"+gone.ID, nil, 200, nil)

	resp, body := c.content("/api/nodes/"+dir+"/zip", nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("zip: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(b)
	}
	want := map[string]string{"a.txt": "aaa", "sub/": "", "sub/b.txt": "bbbb", "empty/": ""}
	if len(got) != len(want) {
		t.Fatalf("zip entries: %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("zip entry %q = %q, want %q", k, got[k], v)
		}
	}
	file := c.upload(c.root, "f.txt", []byte("x"))
	c.get("/api/nodes/"+file.ID+"/zip", 400, nil)
}

func TestThumbnail(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	img := image.NewRGBA(image.Rect(0, 0, 800, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 800; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 100, 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	n := c.upload(c.root, "pic.png", buf.Bytes())
	if !n.Thumb || n.Kind != "image" {
		t.Fatalf("node: %+v", n)
	}
	for i := 0; i < 2; i++ { // second time from the cache
		resp, body := c.content("/api/nodes/"+n.ID+"/thumb", nil)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" {
			t.Fatalf("thumb: %d", resp.StatusCode)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
		if err != nil || cfg.Width != thumbSize || cfg.Height != thumbSize/2 {
			t.Fatalf("thumb size: %+v %v", cfg, err)
		}
	}
	// Not an image, or a broken one: 404, no crash.
	bad := c.upload(c.root, "broken.png", []byte("not a png"))
	c.get("/api/nodes/"+bad.ID+"/thumb", 404, nil)
	txt := c.upload(c.root, "a.txt", []byte("x"))
	c.get("/api/nodes/"+txt.ID+"/thumb", 404, nil)
}

// ---- names, moves ----

func TestNameConflicts(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	a := c.mkdir(c.root, "docs")
	c.call("POST", "/api/nodes/"+c.root+"/children", map[string]string{"name": "docs"}, 409, nil)
	b := c.mkdir(c.root, "other")
	c.call("PATCH", "/api/nodes/"+b, map[string]string{"name": "docs"}, 409, nil)

	f := c.upload(a, "report.pdf", []byte("1"))
	// Default policy for uploads is reject, at init...
	c.call("POST", "/api/uploads", map[string]any{"parent_id": a, "name": "report.pdf", "size": 1}, 409, nil)
	// ...and again at completion if the name got taken in between.
	var init struct {
		ID string `json:"id"`
	}
	c.call("POST", "/api/uploads", map[string]any{"parent_id": a, "name": "late.pdf", "size": 1}, 201, &init)
	c.raw("PUT", "/api/uploads/"+init.ID+"/chunk?offset=0", strings.NewReader("2"), nil)
	c.upload(a, "late.pdf", []byte("3"))
	c.call("POST", "/api/uploads/"+init.ID+"/complete", nil, 409, nil)
	// on_conflict=rename adds a counter before the extension.
	if n := c.upload(a, "report.pdf", []byte("4")); n.Name != "report (1).pdf" {
		t.Errorf("auto-suffix: %q", n.Name)
	}
	if n := c.upload(a, "report.pdf", []byte("5")); n.Name != "report (2).pdf" {
		t.Errorf("auto-suffix: %q", n.Name)
	}
	if n := c.upload(a, ".bashrc", []byte("6")); n.Name != ".bashrc" {
		t.Errorf("dotfile: %q", n.Name)
	}
	if n := c.upload(a, ".bashrc", []byte("7")); n.Name != ".bashrc (1)" {
		t.Errorf("dotfile suffix: %q", n.Name)
	}
	// Moving into a folder that has the name: rejected, nothing changes.
	g := c.upload(b, "report.pdf", []byte("8"))
	c.call("PATCH", "/api/nodes/"+g.ID, map[string]string{"parent_id": a}, 409, nil)
	if !contains(c.names(b), "report.pdf") {
		t.Error("rejected move changed something")
	}
	// Move + rename in one call resolves it.
	c.call("PATCH", "/api/nodes/"+g.ID, map[string]string{"parent_id": a, "name": "report-b.pdf"}, 200, nil)
	if !contains(c.names(a), "report-b.pdf") || len(c.names(b)) != 0 {
		t.Error("move+rename failed")
	}
	// Renaming to the same name is a no-op, not a conflict.
	c.call("PATCH", "/api/nodes/"+f.ID, map[string]string{"name": "report.pdf"}, 200, nil)

	for _, bad := range []string{"", "  ", ".", "..", "a/b", `a\b`, "a\x00b", "a\nb", strings.Repeat("x", 256)} {
		c.call("POST", "/api/nodes/"+c.root+"/children", map[string]string{"name": bad}, 400, nil)
		c.call("PATCH", "/api/nodes/"+f.ID, map[string]string{"name": bad}, 400, nil)
	}
	// Folders are not valid parents for files-as-parents.
	c.call("POST", "/api/nodes/"+f.ID+"/children", map[string]string{"name": "x"}, 400, nil)
	c.call("PATCH", "/api/nodes/"+g.ID, map[string]string{"parent_id": f.ID}, 400, nil)
}

func TestMoveIntoDescendant(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	a := c.mkdir(c.root, "a")
	b := c.mkdir(a, "b")
	d := c.mkdir(b, "d")
	c.call("PATCH", "/api/nodes/"+a, map[string]string{"parent_id": a}, 400, nil)
	c.call("PATCH", "/api/nodes/"+a, map[string]string{"parent_id": b}, 400, nil)
	c.call("PATCH", "/api/nodes/"+a, map[string]string{"parent_id": d}, 400, nil)
	// Legal moves still work, and the tree is intact.
	c.call("PATCH", "/api/nodes/"+d, map[string]string{"parent_id": c.root}, 200, nil)
	c.call("PATCH", "/api/nodes/"+a, map[string]string{"parent_id": d}, 200, nil)
	view := c.list(b)
	if len(view.Breadcrumbs) != 4 || view.Breadcrumbs[1].Name != "d" || view.Breadcrumbs[2].Name != "a" {
		t.Fatalf("breadcrumbs: %+v", view.Breadcrumbs)
	}
	c.call("PATCH", "/api/nodes/"+d, map[string]string{"parent_id": b}, 400, nil)
	// The root cannot be moved or renamed.
	c.call("PATCH", "/api/nodes/"+c.root, map[string]string{"name": "x"}, 400, nil)
	c.call("PATCH", "/api/nodes/"+c.root, map[string]string{"parent_id": a}, 400, nil)
}

func TestListSortSearchUsage(t *testing.T) {
	e := newEnv(t)
	c := e.login("admin@example.com")
	c.mkdir(c.root, "zeta")
	c.upload(c.root, "b.txt", []byte("12345"))
	e.advance(time.Minute)
	c.upload(c.root, "a.txt", []byte("1"))
	e.advance(time.Minute)
	c.upload(c.root, "C_100%.txt", []byte("123"))

	order := func(q string) string {
		var out nodeResp
		c.get("/api/nodes/"+c.root+q, 200, &out)
		var names []string
		for _, ch := range out.Children {
			names = append(names, ch.Name)
		}
		return strings.Join(names, ",")
	}
	if got := order(""); got != "zeta,a.txt,b.txt,C_100%.txt" {
		t.Errorf("by name: %s", got)
	}
	if got := order("?sort=size&order=desc"); got != "zeta,b.txt,C_100%.txt,a.txt" {
		t.Errorf("by size desc: %s", got)
	}
	if got := order("?sort=date&order=desc"); got != "zeta,C_100%.txt,a.txt,b.txt" {
		t.Errorf("by date desc: %s", got)
	}
	// A hostile sort parameter is ignored, not interpolated.
	if got := order("?sort=name;DROP%20TABLE%20nodes&order=x"); got != "zeta,a.txt,b.txt,C_100%.txt" {
		t.Errorf("bad sort: %s", got)
	}
	var items struct {
		Items []nodeJSON `json:"items"`
	}
	c.get("/api/search?q=TXT", 200, &items)
	if len(items.Items) != 3 {
		t.Errorf("search txt: %d", len(items.Items))
	}
	// LIKE wildcards in the query are literal.
	c.get("/api/search?q=100%25", 200, &items)
	if len(items.Items) != 1 {
		t.Errorf("search 100%%: %d", len(items.Items))
	}
	c.get("/api/search?q=_", 200, &items)
	if len(items.Items) != 1 {
		t.Errorf("search _: %d", len(items.Items))
	}
	// Empty query: recent files, newest first.
	c.get("/api/search?q=", 200, &items)
	if len(items.Items) != 3 || items.Items[0].Name != "C_100%.txt" {
		t.Errorf("recent: %+v", items.Items)
	}
	var usage struct {
		Used  int64 `json:"used_bytes"`
		Files int64 `json:"files"`
	}
	c.get("/api/usage", 200, &usage)
	if usage.Used != 9 || usage.Files != 3 {
		t.Errorf("usage: %+v", usage)
	}
}

// ---- public links ----

func TestPublicLinks(t *testing.T) {
	f := newFixture(t)
	e, alice := f.e, f.alice
	mk := func(node string, body map[string]any) (int64, string) {
		var out struct {
			Link linkJSON `json:"link"`
		}
		alice.call("POST", "/api/nodes/"+node+"/links", body, 201, &out)
		if !strings.HasPrefix(out.Link.URL, e.ts.URL+"/s/") {
			t.Fatalf("link url: %s", out.Link.URL)
		}
		return out.Link.ID, strings.TrimPrefix(out.Link.URL, e.ts.URL+"/s/")
	}
	id, token := mk(f.b, map[string]any{})
	if len(token) < 43 { // 32 random bytes, base64url
		t.Fatalf("token too short: %d chars", len(token))
	}
	var stored int
	e.db.QueryRow(`SELECT COUNT(*) FROM links WHERE token_hash = ?`, token).Scan(&stored)
	if stored != 0 {
		t.Error("token stored in clear")
	}
	anon := e.anon()
	pub := "/api/public/" + token

	// Page and info need no session.
	if resp, _ := anon.content("/s/"+token, nil); resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("public page: %d", resp.StatusCode)
	}
	var info struct {
		NeedsPassword bool     `json:"needs_password"`
		Node          nodeJSON `json:"node"`
	}
	anon.get(pub, 200, &info)
	if info.NeedsPassword || info.Node.ID != f.b {
		t.Fatalf("info: %+v", info)
	}
	// Browse and download inside the linked folder.
	var view nodeResp
	anon.get(pub+"/nodes/"+f.b, 200, &view)
	if view.Role != "viewer" || len(view.Children) != 1 || len(view.Breadcrumbs) != 1 {
		t.Fatalf("public listing: %+v", view)
	}
	if resp, body := anon.content(pub+"/nodes/"+f.file+"/content", map[string]string{"Range": "bytes=0-4"}); resp.StatusCode != 206 || string(body) != "hello" {
		t.Fatalf("public range download: %d %q", resp.StatusCode, body)
	}
	if resp, _ := anon.content(pub+"/nodes/"+f.b+"/zip", nil); resp.StatusCode != 200 {
		t.Fatalf("public zip: %d", resp.StatusCode)
	}
	// Nothing outside the linked subtree, by any id.
	for _, outside := range []string{f.a, f.other, f.c, f.secret, alice.root, f.bob.root, "nope"} {
		for _, suffix := range []string{"", "/content", "/zip", "/thumb"} {
			if resp, _ := anon.content(pub+"/nodes/"+outside+suffix, nil); resp.StatusCode != 404 {
				t.Errorf("public access to %s%s: %d", outside, suffix, resp.StatusCode)
			}
		}
	}
	// The link grants nothing on the signed-in API, and is read-only.
	anon.get("/api/nodes/"+f.b, 401, nil)
	for _, m := range []string{"POST", "PATCH", "DELETE", "PUT"} {
		if resp, _ := anon.raw(m, pub+"/nodes/"+f.file, strings.NewReader(`{"name":"x"}`), nil); resp.StatusCode != 404 && resp.StatusCode != 405 {
			t.Errorf("%s on public node: %d", m, resp.StatusCode)
		}
	}
	// A signed-in stranger using the link gets no more than the link gives.
	if resp, _ := f.bob.content(pub+"/nodes/"+f.secret+"/content", nil); resp.StatusCode != 404 {
		t.Errorf("bob through link reached secret: %d", resp.StatusCode)
	}
	// Wrong tokens.
	for _, bad := range []string{"x", strings.Repeat("A", 43), token[:len(token)-1] + "_"} {
		anon.get("/api/public/"+bad, 404, nil)
		anon.get("/api/public/"+bad+"/nodes/"+f.b, 404, nil)
	}

	// Listing shows metadata but never the token.
	var links struct {
		Links []linkJSON `json:"links"`
	}
	alice.get("/api/nodes/"+f.b+"/links", 200, &links)
	if len(links.Links) != 1 || links.Links[0].URL != "" {
		t.Fatalf("links: %+v", links.Links)
	}
	// Trashing the linked folder cuts the link off; restoring brings it back.
	alice.call("DELETE", "/api/nodes/"+f.b, nil, 200, nil)
	anon.get(pub+"/nodes/"+f.file, 404, nil)
	anon.get(pub, 404, nil)
	alice.call("POST", "/api/trash/"+f.b+"/restore", nil, 200, nil)
	anon.get(pub+"/nodes/"+f.file, 200, nil)
	// Revoke.
	f.carol.call("DELETE", "/api/links/"+itoa(id), nil, 403, nil) // editors cannot
	alice.call("DELETE", "/api/links/"+itoa(id), nil, 200, nil)
	anon.get(pub, 404, nil)
	anon.get(pub+"/nodes/"+f.b, 404, nil)
	if resp, _ := anon.content(pub+"/nodes/"+f.file+"/content", nil); resp.StatusCode != 404 {
		t.Errorf("revoked link still downloads: %d", resp.StatusCode)
	}

	// Expiry.
	_, token = mk(f.file, map[string]any{"expires_in_days": 2})
	pub = "/api/public/" + token
	anon.get(pub+"/nodes/"+f.file, 200, nil)
	// A single-file link does not open the folder it is in.
	anon.get(pub+"/nodes/"+f.b, 404, nil)
	e.advance(47 * time.Hour)
	anon.get(pub+"/nodes/"+f.file, 200, nil)
	e.advance(2 * time.Hour)
	anon.get(pub, 404, nil)
	anon.get(pub+"/nodes/"+f.file, 404, nil)
	if resp, _ := anon.content(pub+"/nodes/"+f.file+"/content", nil); resp.StatusCode != 404 {
		t.Errorf("expired link still downloads: %d", resp.StatusCode)
	}
	alice.call("POST", "/api/nodes/"+f.file+"/links", map[string]any{"expires_in_days": 0}, 400, nil)

	// Password.
	_, token = mk(f.b, map[string]any{"password": "s3cret-pw"})
	pub = "/api/public/" + token
	var hash string
	e.db.QueryRow(`SELECT password_hash FROM links ORDER BY id DESC LIMIT 1`).Scan(&hash)
	if !strings.HasPrefix(hash, "pbkdf2-sha256$") || strings.Contains(hash, "s3cret-pw") {
		t.Errorf("password hash: %q", hash)
	}
	visitor := e.anon()
	visitor.get(pub, 200, &info)
	if !info.NeedsPassword {
		t.Fatal("password link does not ask for a password")
	}
	visitor.get(pub+"/nodes/"+f.b, 401, nil)
	if resp, _ := visitor.content(pub+"/nodes/"+f.file+"/content", nil); resp.StatusCode != 401 {
		t.Errorf("locked link downloads: %d", resp.StatusCode)
	}
	visitor.call("POST", pub+"/unlock", map[string]string{"password": "wrong"}, 403, nil)
	visitor.get(pub+"/nodes/"+f.b, 401, nil)
	// A forged unlock cookie does not help.
	forged := e.anon()
	req, _ := http.NewRequest("GET", e.ts.URL+pub+"/nodes/"+f.b, nil)
	var linkID int64
	e.db.QueryRow(`SELECT MAX(id) FROM links`).Scan(&linkID)
	req.AddCookie(&http.Cookie{Name: unlockCookieName(linkID), Value: strings.Repeat("a", 64)})
	if resp, err := forged.http.Do(req); err != nil || resp.StatusCode != 401 {
		t.Errorf("forged unlock cookie: %v %v", resp.StatusCode, err)
	}
	visitor.call("POST", pub+"/unlock", map[string]string{"password": "s3cret-pw"}, 200, nil)
	visitor.get(pub+"/nodes/"+f.b, 200, nil)
	visitor.get(pub+"/nodes/"+f.secret, 404, nil)
	// Unlocking is CSRF-protected like every other POST.
	if resp, _ := e.anon().raw("POST", pub+"/unlock", strings.NewReader(`{"password":"s3cret-pw"}`), map[string]string{csrfHeader: "-"}); resp.StatusCode != 403 {
		t.Errorf("unlock without csrf header: %d", resp.StatusCode)
	}
	// Guessing is rate-limited.
	guesser := e.anon()
	limited := false
	for i := 0; i < 12; i++ {
		resp, _ := guesser.raw("POST", pub+"/unlock", strings.NewReader(`{"password":"x"}`), nil)
		if resp.StatusCode == 429 {
			limited = true
		}
	}
	if !limited {
		t.Error("password guessing not rate-limited")
	}
	// Root cannot be linked.
	alice.call("POST", "/api/nodes/"+alice.root+"/links", map[string]any{}, 400, nil)
}

// ---- curator proxy ----

func TestCuratorProxy(t *testing.T) {
	type seen struct {
		path, query, email, extra, cookie, method string
		emails                                    int
	}
	var mu sync.Mutex
	var last seen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = seen{path: r.URL.Path, query: r.URL.RawQuery, email: r.Header.Get("X-Home-Driver-Email"),
			extra: r.Header.Get("X-Home-Driver-Admin") + r.Header.Get("X-Home-Driver-Anything"), cookie: r.Header.Get("Cookie"),
			method: r.Method, emails: len(r.Header.Values("X-Home-Driver-Email"))}
		mu.Unlock()
		// Like the real curator: every route needs the identity header, and
		// state-changing requests must be same-origin (Origin vs Host or
		// X-Forwarded-Host).
		if r.Header.Get("X-Home-Driver-Email") == "" {
			http.Error(w, "missing X-Home-Driver-Email", 403)
			return
		}
		if o := r.Header.Get("Origin"); r.Method != "GET" && o != "" {
			ou, _ := url.Parse(o)
			if ou == nil || (ou.Host != r.Host && ou.Host != r.Header.Get("X-Forwarded-Host")) {
				http.Error(w, "cross-origin", 403)
				return
			}
		}
		w.Header().Set("X-Upstream", "yes")
		fmt.Fprint(w, "curator says hi")
	}))
	defer upstream.Close()

	e := newEnv(t, func(c *config.Config) { c.CuratorURL = upstream.URL })
	admin := e.login("admin@example.com")
	admin.call("POST", "/api/admin/allowlist", map[string]string{"email": "bob@example.com"}, 200, nil)
	bob := e.login("bob@example.com")

	spoof := map[string]string{
		"X-Home-Driver-Email":    "attacker@example.com",
		"X-Home-Driver-Admin":    "1",
		"x-home-driver-anything": "1",
	}
	resp, body := admin.raw("GET", "/curator/api/queue?page=2", nil, spoof)
	if resp.StatusCode != 200 || string(body) != "curator says hi" || resp.Header.Get("X-Upstream") != "yes" {
		t.Fatalf("admin through proxy: %d %q", resp.StatusCode, body)
	}
	mu.Lock()
	got := last
	mu.Unlock()
	if got.path != "/api/queue" || got.query != "page=2" {
		t.Errorf("prefix not stripped: %q ?%q", got.path, got.query)
	}
	if got.email != "admin@example.com" || got.emails != 1 || got.extra != "" {
		t.Errorf("identity headers: email %q (x%d) extra %q", got.email, got.emails, got.extra)
	}
	if got.cookie != "" {
		t.Errorf("session cookie leaked to curator: %q", got.cookie)
	}
	// Root of the prefix.
	admin.raw("GET", "/curator/", nil, nil)
	mu.Lock()
	if last.path != "/" {
		t.Errorf("/curator/ -> %q", last.path)
	}
	last = seen{}
	mu.Unlock()
	// The curator UI uses relative URLs only, so /curator must become /curator/.
	if resp, _ := admin.raw("GET", "/curator", nil, nil); resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "/curator/" {
		t.Errorf("/curator: %d Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// A spoofed X-Forwarded-Host from the client is replaced by the real Host.
	var fwd string
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fwd = r.Header.Get("X-Forwarded-Host") + "|" + r.Host }))
	defer probe.Close()
	e3 := newEnv(t, func(c *config.Config) { c.CuratorURL = probe.URL })
	a3 := e3.login("admin@example.com")
	a3.raw("GET", "/curator/x", nil, map[string]string{"X-Forwarded-Host": "evil.example"})
	if want := strings.TrimPrefix(e3.ts.URL, "http://"); fwd != want+"|"+want {
		t.Errorf("forwarded host/host = %q, want %q twice", fwd, want)
	}

	// Non-admins and anonymous visitors never reach the upstream.
	if resp, _ := bob.raw("GET", "/curator/api/queue", nil, spoof); resp.StatusCode != 403 {
		t.Errorf("non-admin: %d", resp.StatusCode)
	}
	if resp, _ := bob.raw("POST", "/curator/api/approve", strings.NewReader("{}"), spoof); resp.StatusCode != 403 {
		t.Errorf("non-admin POST: %d", resp.StatusCode)
	}
	anon := e.anon()
	if resp, _ := anon.raw("GET", "/curator/", nil, spoof); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("anonymous GET: %d", resp.StatusCode)
	}
	if resp, _ := anon.raw("POST", "/curator/api/approve", nil, spoof); resp.StatusCode != 401 {
		t.Errorf("anonymous POST: %d", resp.StatusCode)
	}
	mu.Lock()
	if last.path != "" {
		t.Errorf("upstream was reached by a non-admin: %+v", last)
	}
	mu.Unlock()

	// Admin POST works (the curator's own pages do not send our custom
	// header), but a cross-site POST is refused before proxying.
	// A POST exactly as the browser sends it from the curator page: Origin
	// and Sec-Fetch-Site set, no Drive-specific header. It must pass both
	// Drive's CSRF check and the curator's own same-origin check.
	browser := map[string]string{csrfHeader: "-", "Origin": e.ts.URL, "Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"}
	if resp, body := admin.raw("POST", "/curator/api/approve", strings.NewReader("{}"), browser); resp.StatusCode != 200 {
		t.Errorf("admin POST: %d %s", resp.StatusCode, body)
	}
	mu.Lock()
	if last.method != "POST" || last.path != "/api/approve" {
		t.Errorf("admin POST upstream: %+v", last)
	}
	last = seen{}
	mu.Unlock()
	if resp, _ := admin.raw("POST", "/curator/api/approve", strings.NewReader("{}"), map[string]string{"Origin": "https://evil.example"}); resp.StatusCode != 403 {
		t.Errorf("cross-site POST to curator: %d", resp.StatusCode)
	}
	mu.Lock()
	if last.path != "" {
		t.Error("cross-site POST reached the curator")
	}
	mu.Unlock()

	// Session info tells the UI whether to show the link.
	var s struct {
		Curator bool `json:"curator"`
	}
	admin.get("/api/session", 200, &s)
	if !s.Curator {
		t.Error("admin not offered the curator link")
	}
	bob.get("/api/session", 200, &s)
	if s.Curator {
		t.Error("non-admin offered the curator link")
	}

	// Disabled proxy.
	e2 := newEnv(t, func(c *config.Config) { c.CuratorURL = "" })
	a2 := e2.login("admin@example.com")
	if resp, _ := a2.raw("GET", "/curator/", nil, nil); resp.StatusCode != 404 {
		t.Errorf("disabled proxy: %d", resp.StatusCode)
	}
	// Upstream down: a clear 502, not a hang or a panic.
	upstream.Close()
	if resp, _ := admin.raw("GET", "/curator/", nil, nil); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("upstream down: %d", resp.StatusCode)
	}
}

// ---- misc ----

func TestHealthAndStatic(t *testing.T) {
	e := newEnv(t)
	anon := e.anon()
	if resp, body := anon.content("/healthz", nil); resp.StatusCode != 200 || strings.TrimSpace(string(body)) != "ok" {
		t.Fatalf("healthz: %d %q", resp.StatusCode, body)
	}
	for _, p := range []string{"/", "/static/app.js", "/static/app.css", "/static/tokens.css", "/static/i18n.js", "/sw.js",
		"/static/manifest.webmanifest", "/static/icons.svg", "/static/icon-192.png", "/static/icon-512.png"} {
		resp, body := anon.content(p, nil)
		if resp.StatusCode != 200 || len(body) == 0 {
			t.Errorf("GET %s: %d", p, resp.StatusCode)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: nosniff missing", p)
		}
	}
	if resp, _ := anon.content("/static/../server.go", nil); resp.StatusCode == 200 {
		t.Error("path traversal in static")
	}
	if resp, _ := anon.content("/static/index.html", nil); resp.StatusCode != 404 {
		t.Error("html shell served from /static")
	}
	anon.get("/api/nope", 404, nil)
	// The UI never uses inline script or style (the CSP forbids both).
	for _, page := range []string{"index.html", "public.html"} {
		b, err := webFS.ReadFile("web/" + page)
		if err != nil {
			t.Fatal(err)
		}
		html := string(b)
		if strings.Contains(html, "<script>") || strings.Contains(html, "style=\"") || strings.Contains(html, "<style") ||
			strings.Contains(html, "onclick=") || strings.Contains(html, "http://") || strings.Contains(html, "https://") {
			t.Errorf("%s contains inline script/style or an external URL", page)
		}
	}
}

func TestDataDirMustExist(t *testing.T) {
	e := newEnv(t)
	cfg := e.srv.cfg
	cfg.DataDir = filepath.Join(t.TempDir(), "not-mounted")
	if _, err := New(cfg, e.db, e.srv.log); err == nil {
		t.Fatal("server started with a missing data dir")
	}
}
