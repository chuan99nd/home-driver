package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chuan99nd/home-driver/drive/internal/config"
	"github.com/chuan99nd/home-driver/drive/internal/db"
)

const testChunk = 1024

type env struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
	db  *sql.DB
	dir string

	mu  sync.Mutex
	now time.Time
}

type envOpt func(*config.Config)

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0o750); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: data, now: time.Now()}
	var h http.Handler
	e.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(e.ts.Close)

	cfg := config.Config{
		Addr:          "127.0.0.1:0",
		DBPath:        filepath.Join(dir, "db", "drive.db"),
		DataDir:       data,
		BaseURL:       e.ts.URL,
		AdminEmails:   []string{"admin@example.com"},
		SessionSecret: strings.Repeat("s", 40),
		TrashDays:     30,
		DevLogin:      true,
		ChunkSize:     testChunk,
		OIDCIssuer:    "https://accounts.google.com",
	}
	for _, o := range opts {
		o(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	d, err := db.Open(context.Background(), cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	e.db = d
	srv, err := New(cfg, d, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.Now = func() time.Time {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.now
	}
	e.srv = srv
	h = srv.Handler()
	return e
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

// client is one browser: its own cookie jar.
type client struct {
	e    *env
	http *http.Client
	root string
}

func (e *env) anon() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, http: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

// tryLogin goes through /dev/login and reports whether a session resulted.
func (e *env) tryLogin(email string) (*client, bool) {
	e.t.Helper()
	c := e.anon()
	resp, _ := c.raw("GET", "/dev/login?email="+url.QueryEscape(email), nil, nil)
	if resp.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("dev login %s: status %d", email, resp.StatusCode)
	}
	var s struct {
		Authenticated bool `json:"authenticated"`
		User          struct {
			RootID string `json:"root_id"`
		} `json:"user"`
	}
	c.get("/api/session", 200, &s)
	c.root = s.User.RootID
	return c, s.Authenticated
}

func (e *env) login(email string) *client {
	e.t.Helper()
	c, ok := e.tryLogin(email)
	if !ok {
		e.t.Fatalf("login as %s was refused", email)
	}
	return c
}

// raw sends a request. State-changing requests get the CSRF header unless
// hdr sets it to "-".
func (c *client) raw(method, path string, body io.Reader, hdr map[string]string) (*http.Response, []byte) {
	c.e.t.Helper()
	req, err := http.NewRequest(method, c.e.ts.URL+path, body)
	if err != nil {
		c.e.t.Fatal(err)
	}
	if method != "GET" && method != "HEAD" {
		req.Header.Set(csrfHeader, "1")
	}
	for k, v := range hdr {
		if v == "-" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		c.e.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp, b
}

// call sends JSON and checks the status. out may be nil.
func (c *client) call(method, path string, in any, want int, out any) {
	c.e.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	resp, b := c.raw(method, path, body, nil)
	if resp.StatusCode != want {
		c.e.t.Fatalf("%s %s: status %d, want %d; body: %s", method, path, resp.StatusCode, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			c.e.t.Fatalf("%s %s: decode %q: %v", method, path, b, err)
		}
	}
}

func (c *client) get(path string, want int, out any) {
	c.e.t.Helper()
	c.call("GET", path, nil, want, out)
}

type nodeResp struct {
	Node        nodeJSON   `json:"node"`
	Role        string     `json:"role"`
	Breadcrumbs []crumb    `json:"breadcrumbs"`
	Children    []nodeJSON `json:"children"`
	SHA256      string     `json:"sha256"`
}

func (c *client) mkdir(parent, name string) string {
	c.e.t.Helper()
	var out nodeResp
	c.call("POST", "/api/nodes/"+parent+"/children", map[string]string{"name": name}, 201, &out)
	return out.Node.ID
}

func (c *client) list(id string) nodeResp {
	c.e.t.Helper()
	var out nodeResp
	c.get("/api/nodes/"+id, 200, &out)
	return out
}

func (c *client) names(id string) []string {
	c.e.t.Helper()
	var names []string
	for _, ch := range c.list(id).Children {
		names = append(names, ch.Name)
	}
	return names
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// upload runs the whole chunk protocol and returns the new node.
func (c *client) upload(parent, name string, data []byte) nodeJSON {
	c.e.t.Helper()
	var init struct {
		ID        string `json:"id"`
		ChunkSize int    `json:"chunk_size"`
	}
	c.call("POST", "/api/uploads", map[string]any{"parent_id": parent, "name": name, "size": len(data), "on_conflict": "rename"}, 201, &init)
	for off := 0; off < len(data); off += init.ChunkSize {
		end := min(off+init.ChunkSize, len(data))
		resp, b := c.raw("PUT", fmt.Sprintf("/api/uploads/%s/chunk?offset=%d", init.ID, off), bytes.NewReader(data[off:end]), nil)
		if resp.StatusCode != 200 {
			c.e.t.Fatalf("chunk at %d: %d %s", off, resp.StatusCode, b)
		}
	}
	var out nodeResp
	c.call("POST", "/api/uploads/"+init.ID+"/complete", map[string]string{"sha256": sha(data)}, 201, &out)
	return out.Node
}

func (c *client) share(node, email, role string) {
	c.e.t.Helper()
	c.call("POST", "/api/nodes/"+node+"/shares", map[string]string{"email": email, "role": role}, 200, nil)
}

func (c *client) content(path string, hdr map[string]string) (*http.Response, []byte) {
	c.e.t.Helper()
	return c.raw("GET", path, nil, hdr)
}

func (e *env) blobPath(sum string) string {
	return filepath.Join(e.dir, "blobs", sum[:2], sum[2:4], sum)
}

func (e *env) blobExists(sum string) bool {
	_, err := os.Stat(e.blobPath(sum))
	return err == nil
}

func (e *env) refcount(sum string) int {
	e.t.Helper()
	var n int
	err := e.db.QueryRow(`SELECT refcount FROM blobs WHERE sha256 = ?`, sum).Scan(&n)
	if err == sql.ErrNoRows {
		return -1
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) + seed
	}
	return b
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
