package server

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// davClient is a sync client: HTTP Basic, no cookies, no redirects.
type davClient struct {
	e        *env
	email    string
	password string
}

// device creates an app password for a signed-in user and returns a client.
func (c *client) device(email, name string) *davClient {
	c.e.t.Helper()
	var out struct {
		Item appPasswordJSON `json:"item"`
	}
	c.call("POST", "/api/app-passwords", map[string]string{"name": name}, 201, &out)
	if !strings.HasPrefix(out.Item.Password, appPasswordPrefix) || len(out.Item.Password) < 30 {
		c.e.t.Fatalf("app password looks wrong: %q", out.Item.Password)
	}
	return &davClient{e: c.e, email: email, password: out.Item.Password}
}

func davPath(p string) string {
	u := url.URL{Path: p}
	return u.EscapedPath()
}

// do sends a WebDAV request. rawPath is used verbatim (already escaped).
func (d *davClient) do(method, rawPath string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	d.e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, d.e.ts.URL+rawPath, rd)
	if err != nil {
		d.e.t.Fatal(err)
	}
	if d.email != "" {
		req.SetBasicAuth(d.email, d.password)
	}
	for k, v := range hdr {
		if k == "Destination" && strings.HasPrefix(v, "/") {
			v = d.e.ts.URL + davPath(v)
		}
		req.Header.Set(k, v)
	}
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		d.e.t.Fatalf("%s %s: %v", method, rawPath, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// want sends a request to a plain (unescaped) path and checks the status.
func (d *davClient) want(method, p string, body []byte, hdr map[string]string, statuses ...int) (*http.Response, []byte) {
	d.e.t.Helper()
	resp, b := d.do(method, davPath(p), body, hdr)
	for _, s := range statuses {
		if resp.StatusCode == s {
			return resp, b
		}
	}
	d.e.t.Fatalf("%s %s: status %d, want %v; body: %.300s", method, p, resp.StatusCode, statuses, b)
	return nil, nil
}

func (d *davClient) get(p string) string {
	d.e.t.Helper()
	_, b := d.want("GET", p, nil, nil, 200)
	return string(b)
}

// invariants checks, over the whole database, that every blob refcount
// equals the number of node rows referencing it and that every referenced
// blob file exists on disk.
func (e *env) invariants() {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT b.sha256, b.refcount, (SELECT COUNT(*) FROM nodes n WHERE n.blob_sha = b.sha256) FROM blobs b`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sum string
		var ref, live int
		if err := rows.Scan(&sum, &ref, &live); err != nil {
			e.t.Fatal(err)
		}
		if ref != live {
			e.t.Errorf("blob %s: refcount %d but %d rows reference it", sum[:8], ref, live)
		}
		if live > 0 && !e.blobExists(sum) {
			e.t.Errorf("blob %s is referenced but missing on disk", sum[:8])
		}
	}
	var dangling int
	e.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE is_dir = 0 AND blob_sha NOT IN (SELECT sha256 FROM blobs)`).Scan(&dangling)
	if dangling != 0 {
		e.t.Errorf("%d file rows without a blob row", dangling)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dir, "tmp")); len(entries) != 0 {
		e.t.Errorf("tmp not empty: %d leftover files", len(entries))
	}
}

func trashNames(c *client) []string {
	c.e.t.Helper()
	var out struct {
		Items []nodeJSON `json:"items"`
	}
	c.get("/api/trash", 200, &out)
	var names []string
	for _, it := range out.Items {
		names = append(names, fmt.Sprintf("%s:%d", it.Name, it.Size))
	}
	return names
}

const propfindBody = `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`

func TestDAVAuth(t *testing.T) {
	e := newEnv(t)
	admin := e.login("admin@example.com")
	dev := admin.device("admin@example.com", "Điện thoại của bố")
	depth0 := map[string]string{"Depth": "0"}

	// Only the hash is stored.
	var stored int
	e.db.QueryRow(`SELECT COUNT(*) FROM app_passwords WHERE token_hash = ? OR name = ?`, dev.password, dev.password).Scan(&stored)
	if stored != 0 {
		t.Error("app password stored in clear")
	}

	// Unauthenticated OPTIONS is a capability probe; everything else needs Basic.
	anon := &davClient{e: e}
	if resp, _ := anon.do("OPTIONS", "/dav/", nil, nil); resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("DAV"), "2") {
		t.Errorf("OPTIONS: %d DAV=%q", resp.StatusCode, resp.Header.Get("DAV"))
	}
	for _, m := range []string{"PROPFIND", "GET", "PUT", "DELETE", "MKCOL", "MOVE", "COPY", "LOCK", "PROPPATCH"} {
		resp, _ := anon.do(m, "/dav/x", nil, depth0)
		if resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic") {
			t.Errorf("anonymous %s: %d", m, resp.StatusCode)
		}
	}
	// A session cookie alone is never accepted on /dav/, with or without the CSRF header.
	for _, m := range []string{"PROPFIND", "GET", "PUT", "DELETE", "MKCOL"} {
		if resp, _ := admin.raw(m, "/dav/x", strings.NewReader("x"), depth0); resp.StatusCode != 401 {
			t.Errorf("cookie-only %s: %d", m, resp.StatusCode)
		}
	}
	if len(admin.names(admin.root)) != 0 {
		t.Error("a cookie-authenticated DAV request changed something")
	}

	// The real thing works; the email is case-insensitive; no CSRF header needed.
	dev.want("PROPFIND", "/dav/", []byte(propfindBody), depth0, 207)
	(&davClient{e: e, email: "ADMIN@Example.com", password: dev.password}).want("PROPFIND", "/dav/", nil, depth0, 207)
	dev.want("PUT", "/dav/a.txt", []byte("hello"), nil, 201)
	// A browser that somehow holds Basic credentials still cannot be driven cross-site.
	if resp, _ := dev.do("PUT", "/dav/evil.txt", []byte("x"), map[string]string{"Origin": "https://evil.example"}); resp.StatusCode != 403 {
		t.Errorf("cross-origin DAV PUT: %d", resp.StatusCode)
	}
	if resp, _ := dev.do("DELETE", "/dav/a.txt", nil, map[string]string{"Sec-Fetch-Site": "cross-site"}); resp.StatusCode != 403 {
		t.Errorf("cross-site DAV DELETE: %d", resp.StatusCode)
	}

	// Wrong secrets: wrong password, right password with someone else's email,
	// the session token, an empty password.
	admin.call("POST", "/api/admin/allowlist", map[string]string{"email": "bob@example.com"}, 200, nil)
	bob := e.login("bob@example.com")
	bobDev := bob.device("bob@example.com", "laptop")
	for _, c := range []*davClient{
		{e: e, email: "admin@example.com", password: "hd-" + strings.Repeat("a", 32)},
		{e: e, email: "bob@example.com", password: dev.password},
		{e: e, email: "admin@example.com", password: bobDev.password},
		{e: e, email: "admin@example.com", password: ""},
		{e: e, email: "nobody@example.com", password: dev.password},
	} {
		if resp, _ := c.do("PROPFIND", "/dav/", nil, depth0); resp.StatusCode != 401 {
			t.Errorf("bad credentials %s: %d", c.email, resp.StatusCode)
		}
	}

	// Listing shows name, created, last used; never the secret.
	var list struct {
		Items  []appPasswordJSON `json:"items"`
		DavURL string            `json:"dav_url"`
	}
	admin.get("/api/app-passwords", 200, &list)
	if len(list.Items) != 1 || list.Items[0].Name != "Điện thoại của bố" || list.Items[0].LastUsedAt == 0 || list.Items[0].Password != "" || list.DavURL != e.ts.URL+"/dav/" {
		t.Fatalf("list: %+v", list)
	}
	// Another user cannot revoke it; the owner can, and it stops working at once.
	bob.call("DELETE", "/api/app-passwords/"+itoa(list.Items[0].ID), nil, 404, nil)
	second := admin.device("admin@example.com", "second")
	admin.call("DELETE", "/api/app-passwords/"+itoa(list.Items[0].ID), nil, 200, nil)
	if resp, _ := dev.do("PROPFIND", "/dav/", nil, depth0); resp.StatusCode != 401 {
		t.Errorf("revoked app password: %d", resp.StatusCode)
	}
	second.want("PROPFIND", "/dav/", nil, depth0, 207)
	admin.call("POST", "/api/app-passwords", map[string]string{"name": "  "}, 400, nil)
	e.anon().call("POST", "/api/app-passwords", map[string]string{"name": "x"}, 401, nil)

	// The login rule still applies: off the allowlist, the app password is dead.
	bobDev.want("PROPFIND", "/dav/", nil, depth0, 207)
	admin.call("DELETE", "/api/admin/allowlist?email=bob@example.com", nil, 200, nil)
	if resp, _ := bobDev.do("PROPFIND", "/dav/", nil, depth0); resp.StatusCode != 401 {
		t.Errorf("de-allowlisted account: %d", resp.StatusCode)
	}
	if resp, _ := bobDev.do("PUT", "/dav/x.txt", []byte("x"), nil); resp.StatusCode != 401 {
		t.Errorf("de-allowlisted account PUT: %d", resp.StatusCode)
	}
}

func TestDAVRateLimit(t *testing.T) {
	e := newEnv(t)
	admin := e.login("admin@example.com")
	dev := admin.device("admin@example.com", "phone")
	admin.call("POST", "/api/admin/allowlist", map[string]string{"email": "bob@example.com"}, 200, nil)
	bobDev := e.login("bob@example.com").device("bob@example.com", "phone")
	depth0 := map[string]string{"Depth": "0"}
	bad := &davClient{e: e, email: "admin@example.com", password: "wrong"}

	// Per account: after davMaxFailsAcct failures even the right password waits.
	// Each attempt comes "from" a different address so only the account limit counts.
	for i := 0; i < davMaxFailsAcct; i++ {
		resp, _ := bad.do("PROPFIND", "/dav/", nil, map[string]string{"Depth": "0", "CF-Connecting-IP": fmt.Sprintf("203.0.113.%d", i+1)})
		if resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	other := map[string]string{"Depth": "0", "CF-Connecting-IP": "198.51.100.7"}
	if resp, _ := bad.do("PROPFIND", "/dav/", nil, other); resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("account limit: %d", resp.StatusCode)
	}
	if resp, _ := dev.do("PROPFIND", "/dav/", nil, other); resp.StatusCode != 429 {
		t.Fatalf("right password during lockout: %d", resp.StatusCode)
	}
	// Another account from another address is unaffected.
	if resp, _ := bobDev.do("PROPFIND", "/dav/", nil, other); resp.StatusCode != 207 {
		t.Fatalf("other account: %d", resp.StatusCode)
	}
	// The window passes.
	e.advance(davFailWindow + time.Second)
	dev.want("PROPFIND", "/dav/", nil, depth0, 207)

	// Per source: many different accounts guessed from one address.
	src := map[string]string{"Depth": "0", "CF-Connecting-IP": "203.0.113.200"}
	for i := 0; i < davMaxFailsSource; i++ {
		c := &davClient{e: e, email: fmt.Sprintf("guess%d@example.com", i), password: "x"}
		if resp, _ := c.do("PROPFIND", "/dav/", nil, src); resp.StatusCode != 401 {
			t.Fatalf("guess %d: %d", i, resp.StatusCode)
		}
	}
	if resp, _ := bobDev.do("PROPFIND", "/dav/", nil, src); resp.StatusCode != 429 {
		t.Fatalf("source limit: %d", resp.StatusCode)
	}
	if resp, _ := bobDev.do("PROPFIND", "/dav/", nil, other); resp.StatusCode != 207 {
		t.Fatalf("same account from a clean source: %d", resp.StatusCode)
	}
	// Successful requests never count as failures.
	e.advance(davFailWindow + time.Second)
	for i := 0; i < davMaxFailsSource+5; i++ {
		dev.want("PROPFIND", "/dav/", nil, depth0, 207)
	}
}

func TestClientIP(t *testing.T) {
	mk := func(remote string, hdr map[string]string) *http.Request {
		r, _ := http.NewRequest("GET", "http://x/", nil)
		r.RemoteAddr = remote
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	cases := []struct {
		remote string
		hdr    map[string]string
		want   string
	}{
		// Behind the tunnel (loopback): the proxy's headers are used.
		{"127.0.0.1:5000", map[string]string{"CF-Connecting-IP": "203.0.113.9"}, "203.0.113.9"},
		{"[::1]:5000", map[string]string{"X-Forwarded-For": "10.0.0.1, 203.0.113.9"}, "203.0.113.9"},
		{"127.0.0.1:5000", map[string]string{"CF-Connecting-IP": "garbage"}, "127.0.0.1"},
		{"127.0.0.1:5000", nil, "127.0.0.1"},
		// Not from loopback: headers are attacker-controlled and ignored.
		{"192.168.1.50:5000", map[string]string{"CF-Connecting-IP": "203.0.113.9", "X-Forwarded-For": "1.2.3.4"}, "192.168.1.50"},
	}
	for _, c := range cases {
		if got := clientIP(mk(c.remote, c.hdr)); got != c.want {
			t.Errorf("clientIP(%s, %v) = %q, want %q", c.remote, c.hdr, got, c.want)
		}
	}
}

func TestDAVFiles(t *testing.T) {
	e := newEnv(t)
	admin := e.login("admin@example.com")
	dev := admin.device("admin@example.com", "laptop")
	v1, v2 := pattern(3000, 1), pattern(4000, 2)

	// PROPFIND depth 0 and 1; infinite depth is refused.
	_, body := dev.want("PROPFIND", "/dav/", []byte(propfindBody), map[string]string{"Depth": "0"}, 207)
	if strings.Count(string(body), "<D:response>") != 1 {
		t.Errorf("depth 0: %s", body)
	}
	dev.want("MKCOL", "/dav/Tài liệu", nil, nil, 201)
	dev.want("MKCOL", "/dav/Tài liệu", nil, nil, 405) // exists
	dev.want("MKCOL", "/dav/no/such/parent", nil, nil, 409)
	dev.want("PUT", "/dav/Tài liệu/báo cáo.bin", v1, nil, 201)
	_, body = dev.want("PROPFIND", "/dav/", []byte(propfindBody), map[string]string{"Depth": "1"}, 207)
	for _, wantHref := range []string{"/dav/T%C3%A0i%20li%E1%BB%87u/", "/dav/_Shared%20with%20me/"} {
		if !strings.Contains(string(body), wantHref) {
			t.Errorf("depth 1 listing lacks %s: %s", wantHref, body)
		}
	}
	_, body = dev.want("PROPFIND", "/dav/Tài liệu/báo cáo.bin", []byte(propfindBody), map[string]string{"Depth": "0"}, 207)
	if !strings.Contains(string(body), "<D:getcontentlength>3000<") || !strings.Contains(string(body), sha(v1)) {
		t.Errorf("file props: %s", body)
	}
	dev.want("PROPFIND", "/dav/", nil, map[string]string{"Depth": "infinity"}, 403)
	dev.want("PROPFIND", "/dav/", nil, nil, 403)
	dev.want("PROPFIND", "/dav/missing", nil, map[string]string{"Depth": "0"}, 404)

	// The web API sees the same thing.
	if names := admin.names(admin.root); len(names) != 1 || names[0] != "Tài liệu" {
		t.Fatalf("API listing: %v", names)
	}

	// GET, with Range, as a safe attachment.
	resp, got := dev.want("GET", "/dav/Tài liệu/báo cáo.bin", nil, map[string]string{"Range": "bytes=100-199"}, 206)
	if !bytes.Equal(got, v1[100:200]) || resp.Header.Get("ETag") != `"`+sha(v1)+`"` {
		t.Error("range GET mismatch")
	}
	dev.want("PUT", "/dav/x.html", []byte("<script>alert(1)</script>"), nil, 201)
	resp, _ = dev.want("GET", "/dav/x.html", nil, nil, 200)
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "html") || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("html over DAV: type %q disposition %q", ct, resp.Header.Get("Content-Disposition"))
	}

	// Overwrite: the previous version goes to the trash, the node keeps its id.
	before := admin.list(admin.list(admin.root).Children[0].ID).Children[0]
	admin.share(before.ID, "friend@example.com", "viewer")
	dev.want("PUT", "/dav/Tài liệu/báo cáo.bin", v2, map[string]string{"X-OC-Mtime": "1700000000"}, 201, 204)
	after := admin.list(admin.list(admin.root).Children[0].ID).Children[0]
	if after.ID != before.ID || after.Size != 4000 || after.UpdatedAt != 1700000000 {
		t.Fatalf("after overwrite: %+v (before %+v)", after, before)
	}
	if dev.get("/dav/Tài liệu/báo cáo.bin") != string(v2) {
		t.Fatal("new content not served")
	}
	if tr := trashNames(admin); len(tr) != 1 || tr[0] != "báo cáo.bin:3000" {
		t.Fatalf("trash after overwrite: %v", tr)
	}
	if e.refcount(sha(v1)) != 1 || e.refcount(sha(v2)) != 1 || !e.blobExists(sha(v1)) {
		t.Fatalf("refcounts after overwrite: old %d new %d", e.refcount(sha(v1)), e.refcount(sha(v2)))
	}
	var shares int
	e.db.QueryRow(`SELECT COUNT(*) FROM shares WHERE node_id = ?`, before.ID).Scan(&shares)
	if shares != 1 {
		t.Error("overwrite dropped the file's share")
	}
	// The old version can be restored next to the new one, byte for byte.
	var trash struct {
		Items []nodeJSON `json:"items"`
	}
	admin.get("/api/trash", 200, &trash)
	admin.call("POST", "/api/trash/"+trash.Items[0].ID+"/restore", nil, 200, nil)
	if dev.get("/dav/Tài liệu/báo cáo (1).bin") != string(v1) {
		t.Fatal("restored previous version differs")
	}
	// Re-sending identical content is a no-op: no new trash entry.
	dev.want("PUT", "/dav/Tài liệu/báo cáo.bin", v2, nil, 201, 204)
	if tr := trashNames(admin); len(tr) != 0 {
		t.Fatalf("identical PUT created a version: %v", tr)
	}
	// The two-step save of Finder/Windows (empty file, then content) leaves no junk.
	dev.want("PUT", "/dav/new.txt", nil, nil, 201)
	dev.want("PUT", "/dav/new.txt", []byte("content"), nil, 201, 204)
	if tr := trashNames(admin); len(tr) != 0 {
		t.Fatalf("empty placeholder kept as a version: %v", tr)
	}
	e.invariants()

	// An upload cut off mid-way saves nothing and leaves the old content alone.
	conn, err := net.Dial("tcp", strings.TrimPrefix(e.ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("PUT", e.ts.URL+"/dav/new.txt", nil)
	req.SetBasicAuth(dev.email, dev.password)
	fmt.Fprintf(conn, "PUT /dav/new.txt HTTP/1.1\r\nHost: x\r\nAuthorization: %s\r\nContent-Length: 100000\r\n\r\n%s", req.Header.Get("Authorization"), strings.Repeat("z", 5000))
	conn.Close()
	fmt.Fprint(io.Discard, "")
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, _ := os.ReadDir(filepath.Join(e.dir, "tmp"))
		if len(entries) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dev.get("/dav/new.txt") != "content" {
		t.Fatal("aborted PUT damaged the file")
	}
	if tr := trashNames(admin); len(tr) != 0 {
		t.Fatalf("aborted PUT created a version: %v", tr)
	}
	e.invariants()

	// DELETE goes to the trash; nothing is removed from disk.
	dev.want("DELETE", "/dav/new.txt", nil, nil, 204)
	dev.want("GET", "/dav/new.txt", nil, nil, 404)
	dev.want("DELETE", "/dav/new.txt", nil, nil, 404)
	if tr := trashNames(admin); len(tr) != 1 || tr[0] != "new.txt:7" || !e.blobExists(sha([]byte("content"))) {
		t.Fatalf("after DELETE: %v", tr)
	}
	dev.want("DELETE", "/dav/", nil, nil, 403)                // the root
	dev.want("DELETE", "/dav/_Shared with me", nil, nil, 403) // the virtual folder
	dev.want("MOVE", "/dav/_Shared with me", nil, map[string]string{"Destination": "/dav/x"}, 403)

	// MOVE: rename, into a folder, over an existing target.
	dev.want("MOVE", "/dav/x.html", nil, map[string]string{"Destination": "/dav/Tài liệu/y.html"}, 201)
	dev.want("GET", "/dav/x.html", nil, nil, 404)
	dev.want("PUT", "/dav/target.txt", []byte("old target"), nil, 201)
	dev.want("PUT", "/dav/source.txt", []byte("the source"), nil, 201)
	dev.want("MOVE", "/dav/source.txt", nil, map[string]string{"Destination": "/dav/target.txt", "Overwrite": "F"}, 412)
	dev.want("MOVE", "/dav/source.txt", nil, map[string]string{"Destination": "/dav/target.txt", "Overwrite": "T"}, 204)
	if dev.get("/dav/target.txt") != "the source" {
		t.Fatal("MOVE over target")
	}
	if tr := trashNames(admin); !contains(tr, "target.txt:10") {
		t.Fatalf("overwritten MOVE target not in trash: %v", tr)
	}
	// A folder cannot be moved or copied into itself.
	dev.want("MKCOL", "/dav/Tài liệu/sub", nil, nil, 201)
	dev.want("MOVE", "/dav/Tài liệu", nil, map[string]string{"Destination": "/dav/Tài liệu/sub/inside"}, 403)
	dev.want("COPY", "/dav/Tài liệu", nil, map[string]string{"Destination": "/dav/Tài liệu/sub/inside"}, 403)
	dev.want("MOVE", "/dav/Tài liệu", nil, map[string]string{"Destination": "/dav/Tài liệu"}, 403)

	// COPY: a file, then a whole folder.
	dev.want("COPY", "/dav/target.txt", nil, map[string]string{"Destination": "/dav/copy.txt"}, 201)
	if dev.get("/dav/copy.txt") != "the source" || e.refcount(sha([]byte("the source"))) != 2 {
		t.Fatalf("COPY file: refcount %d", e.refcount(sha([]byte("the source"))))
	}
	dev.want("COPY", "/dav/Tài liệu", nil, map[string]string{"Destination": "/dav/Bản sao"}, 201)
	if dev.get("/dav/Bản sao/báo cáo.bin") != string(v2) || dev.get("/dav/Bản sao/y.html") == "" {
		t.Fatal("COPY folder")
	}
	if e.refcount(sha(v2)) != 2 {
		t.Fatalf("refcount after folder copy: %d", e.refcount(sha(v2)))
	}
	// Destinations outside /dav or on another host are refused.
	dev.want("MOVE", "/dav/copy.txt", nil, map[string]string{"Destination": e.ts.URL + "/api/nodes/x"}, 403)
	dev.want("COPY", "/dav/copy.txt", nil, map[string]string{"Destination": "http://other.example/dav/copy2.txt"}, 502)
	dev.want("MOVE", "/dav/copy.txt", nil, nil, 400)

	// Names the web UI refuses are refused here too.
	dev.want("MKCOL", "/dav/_Shared with me", nil, nil, 403, 405)
	dev.want("PUT", "/dav/_Shared with me", []byte("x"), nil, 403)
	dev.want("PUT", "/dav/_Shared with me/file.txt", []byte("x"), nil, 403)
	admin.call("POST", "/api/nodes/"+admin.root+"/children", map[string]string{"name": davSharedName}, 400, nil)
	sub := admin.mkdir(admin.root, "ok")
	admin.mkdir(sub, davSharedName) // only reserved at the top of a drive
	admin.call("PATCH", "/api/nodes/"+sub, map[string]string{"name": davSharedName}, 400, nil)
	e.invariants()

	// Purging the trash keeps every invariant (and is still the only delete).
	admin.call("DELETE", "/api/trash", nil, 200, nil)
	e.invariants()
	if dev.get("/dav/Tài liệu/báo cáo.bin") != string(v2) {
		t.Fatal("file damaged by purge of its previous versions")
	}
}

func TestDAVLock(t *testing.T) {
	e := newEnv(t)
	dev := e.login("admin@example.com").device("admin@example.com", "mac")
	dev.want("PUT", "/dav/doc.txt", []byte("v1"), nil, 201)
	lock := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner>mac</D:owner></D:lockinfo>`
	resp, body := dev.want("LOCK", "/dav/doc.txt", []byte(lock), map[string]string{"Timeout": "Second-600"}, 200)
	token := resp.Header.Get("Lock-Token")
	if token == "" || !strings.Contains(string(body), "opaquelocktoken") && !strings.Contains(string(body), "locktoken") {
		t.Fatalf("LOCK: token %q body %s", token, body)
	}
	// Without the token the file is locked; with it the write goes through.
	dev.want("PUT", "/dav/doc.txt", []byte("v2"), nil, 423)
	dev.want("DELETE", "/dav/doc.txt", nil, nil, 423)
	dev.want("PUT", "/dav/doc.txt", []byte("v2"), map[string]string{"If": "(" + token + ")"}, 201, 204)
	dev.want("UNLOCK", "/dav/doc.txt", nil, map[string]string{"Lock-Token": token}, 204)
	dev.want("PUT", "/dav/doc.txt", []byte("v3"), nil, 201, 204)
	if dev.get("/dav/doc.txt") != "v3" {
		t.Fatal("content after unlock")
	}
	// LOCK on a new name creates an empty file (class 2 behaviour Finder relies on).
	resp, _ = dev.want("LOCK", "/dav/fresh.txt", []byte(lock), nil, 201)
	dev.want("PUT", "/dav/fresh.txt", []byte("x"), map[string]string{"If": "(" + resp.Header.Get("Lock-Token") + ")"}, 201, 204)
	// Locks are per user: another user's lock table is separate.
	admin := e.login("admin@example.com")
	admin.call("POST", "/api/admin/allowlist", map[string]string{"email": "bob@example.com"}, 200, nil)
	bobDev := e.login("bob@example.com").device("bob@example.com", "pc")
	bobDev.want("PUT", "/dav/fresh.txt", []byte("bob's own file"), nil, 201)
	e.invariants()
}

// The fixture tree, seen through WebDAV by people it is shared with.
func TestDAVSharingAndEscapes(t *testing.T) {
	f := newFixture(t)
	e := f.e
	alice := f.alice.device("admin@example.com", "a")
	bob := f.bob.device("bob@example.com", "b")       // viewer of A/B
	carol := f.carol.device("carol@example.com", "c") // editor of A
	shared := "/dav/" + davSharedName
	d1 := map[string]string{"Depth": "1"}

	// What each of them sees.
	_, body := bob.want("PROPFIND", shared+"/", nil, d1, 207)
	if !strings.Contains(string(body), "/B/") || strings.Contains(string(body), "/A/") || strings.Contains(string(body), "secret") {
		t.Fatalf("bob's shared folder: %s", body)
	}
	if bob.get(shared+"/B/file.txt") != "hello file" {
		t.Fatal("viewer cannot read")
	}
	_, body = carol.want("PROPFIND", shared+"/A/", nil, d1, 207)
	if !strings.Contains(string(body), "other.txt") || !strings.Contains(string(body), "/B/") {
		t.Fatalf("carol's view of A: %s", body)
	}

	// Nothing outside a share is reachable by name, by "..", by encoding or by case.
	for _, p := range []string{
		"/dav/C/secret.txt", "/dav/A/other.txt", "/dav/A/B/file.txt",
		shared + "/A/other.txt", shared + "/C/secret.txt", shared + "/B/../C/secret.txt",
		"/dav/_shared with me/B/file.txt", shared + "/b/file.txt", shared + "/B/FILE.TXT",
	} {
		if resp, b := bob.do("GET", davPath(p), nil, nil); resp.StatusCode == 200 || strings.Contains(string(b), "top secret") || string(b) == "other" {
			t.Errorf("bob GET %s: %d %q", p, resp.StatusCode, b)
		}
	}
	for _, raw := range []string{
		"/dav/_Shared%20with%20me/B/%2e%2e/%2e%2e/C/secret.txt",
		"/dav/_Shared%20with%20me/B/..%2f..%2fC/secret.txt",
		"/dav/_Shared%20with%20me/B/%2e%2e/other.txt",
		"/dav/_Shared%20with%20me/B%2f..%2fother.txt",
		"/dav/%2e%2e/api/session",
		"/dav/_Shared%20with%20me/B/..%5cother.txt",
		"/dav//C/secret.txt",
	} {
		for _, m := range []string{"GET", "PROPFIND", "DELETE"} {
			resp, b := bob.do(m, raw, nil, map[string]string{"Depth": "0"})
			if resp.StatusCode/100 == 3 { // the mux normalised the path: follow it once
				loc, _ := url.Parse(resp.Header.Get("Location"))
				resp, b = bob.do(m, loc.EscapedPath(), nil, map[string]string{"Depth": "0"})
			}
			if resp.StatusCode < 400 || strings.Contains(string(b), "top secret") || strings.Contains(string(b), "other") {
				t.Errorf("bob %s %s: %d %q", m, raw, resp.StatusCode, b)
			}
		}
	}

	// Viewer: every write is refused, and nothing changed.
	for _, c := range []struct {
		method, path string
		hdr          map[string]string
	}{
		{"PUT", shared + "/B/new.txt", nil},
		{"PUT", shared + "/B/file.txt", nil},
		{"MKCOL", shared + "/B/dir", nil},
		{"DELETE", shared + "/B/file.txt", nil},
		{"DELETE", shared + "/B", nil},
		{"MOVE", shared + "/B/file.txt", map[string]string{"Destination": shared + "/B/renamed.txt"}},
		{"MOVE", shared + "/B/file.txt", map[string]string{"Destination": "/dav/stolen.txt"}},
		{"COPY", shared + "/B/file.txt", map[string]string{"Destination": shared + "/B/copy.txt"}},
		{"COPY", shared + "/B/file.txt", map[string]string{"Destination": "/dav/mine.txt"}}, // across owners
		{"LOCK", shared + "/B/newlock.txt", nil},
	} {
		resp, _ := bob.do(c.method, davPath(c.path), writeBody(c.method), mergeDest(e, c.hdr))
		if resp.StatusCode != 403 {
			t.Errorf("viewer %s %s: %d, want 403", c.method, c.path, resp.StatusCode)
		}
	}
	if names := f.alice.names(f.b); len(names) != 1 || names[0] != "file.txt" || alice.get("/dav/A/B/file.txt") != "hello file" {
		t.Fatalf("viewer changed the shared folder: %v", names)
	}
	if len(f.bob.names(f.bob.root)) != 0 {
		t.Fatal("viewer got a copy across owners")
	}

	// Editor: works inside the shared folder; what she creates is alice's.
	carol.want("PUT", shared+"/A/from-carol.txt", []byte("carol wrote this"), nil, 201)
	carol.want("MKCOL", shared+"/A/carol-dir", nil, nil, 201)
	carol.want("MOVE", shared+"/A/from-carol.txt", nil, map[string]string{"Destination": shared + "/A/carol-dir/moved.txt"}, 201)
	carol.want("COPY", shared+"/A/other.txt", nil, map[string]string{"Destination": shared + "/A/carol-dir/other-copy.txt"}, 201)
	if alice.get("/dav/A/carol-dir/moved.txt") != "carol wrote this" {
		t.Fatal("alice does not see the editor's file")
	}
	var owner int64
	e.db.QueryRow(`SELECT owner_id FROM nodes WHERE name = 'moved.txt'`).Scan(&owner)
	if owner != f.userID("admin@example.com") {
		t.Errorf("editor's file owner = %d", owner)
	}
	// Editor overwrites alice's file: the previous version lands in alice's trash.
	carol.want("PUT", shared+"/A/other.txt", []byte("changed by carol"), nil, 201, 204)
	if tr := trashNames(f.alice); !contains(tr, "other.txt:5") {
		t.Fatalf("owner's trash after editor overwrite: %v", tr)
	}
	if len(trashNames(f.carol)) != 0 {
		t.Fatal("editor sees the owner's trash")
	}
	// Editor deletes: to alice's trash, not gone.
	carol.want("DELETE", shared+"/A/carol-dir/moved.txt", nil, nil, 204)
	if tr := trashNames(f.alice); !contains(tr, "moved.txt:16") || !e.blobExists(sha([]byte("carol wrote this"))) {
		t.Fatalf("editor delete: %v", tr)
	}
	// Editor cannot touch the shared folder itself, move things out of it,
	// pull her own files into it, or reach alice's other folders.
	carol.want("PUT", "/dav/my-own.txt", []byte("mine"), nil, 201)
	for _, c := range []struct {
		method, path string
		hdr          map[string]string
	}{
		{"DELETE", shared + "/A", nil},
		{"MOVE", shared + "/A", map[string]string{"Destination": shared + "/A2"}},
		{"MOVE", shared + "/A", map[string]string{"Destination": "/dav/A"}},
		{"MOVE", shared + "/A/other.txt", map[string]string{"Destination": "/dav/taken.txt"}},
		{"COPY", shared + "/A/other.txt", map[string]string{"Destination": "/dav/taken.txt"}},
		{"MOVE", "/dav/my-own.txt", map[string]string{"Destination": shared + "/A/given.txt"}},
		{"COPY", "/dav/my-own.txt", map[string]string{"Destination": shared + "/A/given.txt"}},
		{"MOVE", shared + "/A/other.txt", map[string]string{"Destination": shared + "/x.txt"}},
		{"MKCOL", shared + "/new-top", nil},
		{"PUT", shared + "/new-top.txt", nil},
	} {
		resp, _ := carol.do(c.method, davPath(c.path), writeBody(c.method), mergeDest(e, c.hdr))
		if resp.StatusCode != 403 {
			t.Errorf("editor %s %s -> %v: %d, want 403", c.method, c.path, c.hdr, resp.StatusCode)
		}
	}
	for _, p := range []string{"/dav/C/x.txt", shared + "/C/x.txt", shared + "/A/../C/x.txt"} {
		if resp, _ := carol.do("PUT", davPath(p), []byte("x"), nil); resp.StatusCode/100 == 2 {
			t.Errorf("editor PUT %s: %d", p, resp.StatusCode)
		}
	}
	if names := f.alice.names(f.c); len(names) != 1 || names[0] != "secret.txt" {
		t.Fatalf("alice's private folder changed: %v", names)
	}
	if alice.get("/dav/A/other.txt") != "changed by carol" || carol.get("/dav/my-own.txt") != "mine" {
		t.Fatal("a refused move changed something")
	}

	// Revoking the share cuts WebDAV access at once (carol had only this share).
	var sid int64
	e.db.QueryRow(`SELECT id FROM shares WHERE email = 'carol@example.com'`).Scan(&sid)
	f.alice.call("DELETE", "/api/shares/"+itoa(sid), nil, 200, nil)
	if resp, _ := carol.do("GET", davPath(shared+"/A/other.txt"), nil, nil); resp.StatusCode != 401 {
		t.Errorf("after revoke: %d", resp.StatusCode)
	}
	// Trashing a shared folder hides it from the viewer.
	alice.want("DELETE", "/dav/A/B", nil, nil, 204)
	bob.want("GET", shared+"/B/file.txt", nil, nil, 404)
	_, body = bob.want("PROPFIND", shared+"/", nil, d1, 207)
	if strings.Contains(string(body), "/B/") {
		t.Error("trashed share still listed")
	}
	e.invariants()
}

const lockBody = `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`

// writeBody is a valid request body for a write method.
func writeBody(method string) []byte {
	switch method {
	case "PUT":
		return []byte("x")
	case "LOCK":
		return []byte(lockBody)
	}
	return nil
}

func mergeDest(e *env, hdr map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range hdr {
		out[k] = v
	}
	return out
}

// Two people sharing items with the same name get distinct entries.
func TestDAVSharedNameCollision(t *testing.T) {
	e := newEnv(t)
	admin := e.login("admin@example.com")
	admin.call("POST", "/api/admin/allowlist", map[string]string{"email": "second@example.com"}, 200, nil)
	second := e.login("second@example.com")
	a := admin.mkdir(admin.root, "Photos")
	b := second.mkdir(second.root, "Photos")
	admin.upload(a, "from-admin.txt", []byte("1"))
	second.upload(b, "from-second.txt", []byte("2"))
	admin.share(a, "guest@example.com", "viewer")
	second.share(b, "guest@example.com", "viewer")
	guest := e.login("guest@example.com").device("guest@example.com", "g")
	_, body := guest.want("PROPFIND", "/dav/"+davSharedName+"/", nil, map[string]string{"Depth": "1"}, 207)
	if strings.Count(string(body), "<D:response>") != 3 {
		t.Fatalf("listing: %s", body)
	}
	n1, n2 := "Photos ("+a[:6]+")", "Photos ("+b[:6]+")"
	if guest.get("/dav/"+davSharedName+"/"+n1+"/from-admin.txt") != "1" || guest.get("/dav/"+davSharedName+"/"+n2+"/from-second.txt") != "2" {
		t.Fatal("colliding shared names not separated")
	}
	guest.want("GET", "/dav/"+davSharedName+"/Photos/from-admin.txt", nil, nil, 404)
}
