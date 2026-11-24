package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/chuan99nd/home-driver/drive/internal/access"
)

// fixture builds, as admin@example.com (alice):
//
//	My Drive/
//	  A/            shared with carol as editor
//	    B/          shared with bob as viewer
//	      file.txt
//	    other.txt
//	  C/
//	    secret.txt
type fixture struct {
	e                            *env
	alice, bob, carol            *client
	a, b, c, file, other, secret string
}

func newFixture(t *testing.T) *fixture {
	e := newEnv(t)
	f := &fixture{e: e}
	f.alice = e.login("admin@example.com")
	f.a = f.alice.mkdir(f.alice.root, "A")
	f.b = f.alice.mkdir(f.a, "B")
	f.c = f.alice.mkdir(f.alice.root, "C")
	f.file = f.alice.upload(f.b, "file.txt", []byte("hello file")).ID
	f.other = f.alice.upload(f.a, "other.txt", []byte("other")).ID
	f.secret = f.alice.upload(f.c, "secret.txt", []byte("top secret")).ID
	f.alice.share(f.b, "Bob@Example.com", "viewer") // stored lower-cased
	f.alice.share(f.a, "carol@example.com", "editor")
	f.bob = e.login("bob@example.com")
	f.carol = e.login("CAROL@example.com")
	return f
}

func (f *fixture) userID(email string) int64 {
	var id int64
	if err := f.e.db.QueryRow(`SELECT id FROM users WHERE email = ?`, email).Scan(&id); err != nil {
		f.e.t.Fatalf("user %s: %v", email, err)
	}
	return id
}

func (f *fixture) level(email, node string) access.Level {
	f.e.t.Helper()
	res, err := access.Resolve(context.Background(), f.e.db, access.Principal{UserID: f.userID(email), Email: email}, node)
	if err != nil {
		f.e.t.Fatalf("resolve %s on %s: %v", email, node, err)
	}
	return res.Level
}

func TestAccessMatrix(t *testing.T) {
	f := newFixture(t)
	// dave is allowlisted but has nothing shared.
	f.alice.call("POST", "/api/admin/allowlist", map[string]string{"email": "dave@example.com"}, 200, nil)
	f.e.login("dave@example.com")

	cases := []struct {
		email string
		node  string
		name  string
		want  access.Level
	}{
		{"admin@example.com", f.a, "A", access.Owner},
		{"admin@example.com", f.file, "file", access.Owner},
		{"admin@example.com", f.secret, "secret", access.Owner},
		// bob: viewer on B and below, nothing else.
		{"bob@example.com", f.b, "B", access.Viewer},
		{"bob@example.com", f.file, "file (inherited)", access.Viewer},
		{"bob@example.com", f.a, "A (parent of share)", access.None},
		{"bob@example.com", f.other, "other (sibling)", access.None},
		{"bob@example.com", f.c, "C", access.None},
		{"bob@example.com", f.secret, "secret", access.None},
		{"bob@example.com", f.alice.root, "alice root", access.None},
		// carol: editor on A and everything below.
		{"carol@example.com", f.a, "A", access.Editor},
		{"carol@example.com", f.b, "B (inherited)", access.Editor},
		{"carol@example.com", f.file, "file (inherited)", access.Editor},
		{"carol@example.com", f.c, "C", access.None},
		{"carol@example.com", f.secret, "secret", access.None},
		// dave: nothing.
		{"dave@example.com", f.a, "A", access.None},
		{"dave@example.com", f.file, "file", access.None},
	}
	for _, c := range cases {
		if got := f.level(c.email, c.node); got != c.want {
			t.Errorf("%s on %s: got %v, want %v", c.email, c.name, got, c.want)
		}
	}

	// Unknown node and empty principal.
	if _, err := access.Resolve(context.Background(), f.e.db, access.Principal{UserID: 1, Email: "admin@example.com"}, "nope"); err != access.ErrNotFound {
		t.Errorf("unknown node: err = %v, want ErrNotFound", err)
	}
	if res, _ := access.Resolve(context.Background(), f.e.db, access.Principal{}, f.file); res.Level != access.None {
		t.Errorf("empty principal: got %v", res.Level)
	}
	// A principal with the right email but a different user id and no share.
	if res, _ := access.Resolve(context.Background(), f.e.db, access.Principal{UserID: 9999, Email: "nobody@example.com"}, f.file); res.Level != access.None {
		t.Errorf("stranger: got %v", res.Level)
	}

	// The strongest share on the chain wins: bob gets editor on A too.
	f.alice.share(f.a, "bob@example.com", "editor")
	if got := f.level("bob@example.com", f.file); got != access.Editor {
		t.Errorf("bob after editor share on A: %v", got)
	}
	// Downgrade by re-sharing with a different role.
	f.alice.share(f.a, "bob@example.com", "viewer")
	if got := f.level("bob@example.com", f.other); got != access.Viewer {
		t.Errorf("bob after downgrade: %v", got)
	}

	// Revoking carol's share removes everything she had.
	var shares struct {
		Shares []shareJSON `json:"shares"`
	}
	f.alice.get("/api/nodes/"+f.a+"/shares", 200, &shares)
	for _, sh := range shares.Shares {
		f.alice.call("DELETE", "/api/shares/"+itoa(sh.ID), nil, 200, nil)
	}
	for _, n := range []string{f.a, f.b, f.file, f.other} {
		if got := f.level("carol@example.com", n); got != access.None {
			t.Errorf("carol after revoke on %s: %v", n, got)
		}
	}
	// bob still has the direct viewer share on B.
	if got := f.level("bob@example.com", f.file); got != access.Viewer {
		t.Errorf("bob after A revoke: %v", got)
	}
	if got := f.level("bob@example.com", f.other); got != access.None {
		t.Errorf("bob on other after A revoke: %v", got)
	}

	// Trashed nodes are invisible to share recipients, even with a live share.
	f.alice.call("DELETE", "/api/nodes/"+f.b, nil, 200, nil)
	if got := f.level("bob@example.com", f.file); got != access.None {
		t.Errorf("bob on file in trashed folder: %v", got)
	}
	res, _ := access.Resolve(context.Background(), f.e.db, access.Principal{UserID: f.userID("admin@example.com"), Email: "admin@example.com"}, f.file)
	if res.Level != access.Owner || !res.Trashed {
		t.Errorf("owner on trashed: level %v trashed %v", res.Level, res.Trashed)
	}
}

func TestLinkPrincipalScope(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p := access.Principal{LinkNodeID: f.b}
	for _, c := range []struct {
		node string
		want access.Level
	}{{f.b, access.Viewer}, {f.file, access.Viewer}, {f.a, access.None}, {f.other, access.None}, {f.secret, access.None}, {f.alice.root, access.None}} {
		res, err := access.Resolve(ctx, f.e.db, p, c.node)
		if err != nil || res.Level != c.want {
			t.Errorf("link on %s: %v %v, want %v", c.node, res.Level, err, c.want)
		}
	}
	// A link principal never gains from a user identity.
	p = access.Principal{LinkNodeID: f.b, UserID: f.userID("admin@example.com"), Email: "admin@example.com"}
	if res, _ := access.Resolve(ctx, f.e.db, p, f.secret); res.Level != access.None {
		t.Errorf("link+user on secret: %v", res.Level)
	}
	// Breadcrumbs stop at the linked node.
	res, _ := access.Resolve(ctx, f.e.db, access.Principal{LinkNodeID: f.b}, f.file)
	if v := res.Visible(); len(v) != 2 || v[0].ID != f.b || v[1].ID != f.file {
		t.Errorf("visible chain = %v", v)
	}
}

// Attempts to reach outside a shared subtree through every endpoint that
// takes a node or parent id.
func TestSharedSubtreeEscape(t *testing.T) {
	f := newFixture(t)
	bob, carol := f.bob, f.carol

	// Reads outside the share.
	for _, id := range []string{f.a, f.c, f.secret, f.other, f.alice.root} {
		bob.get("/api/nodes/"+id, 404, nil)
		if resp, _ := bob.content("/api/nodes/"+id+"/content", nil); resp.StatusCode != 404 {
			t.Errorf("bob content %s: %d", id, resp.StatusCode)
		}
		bob.get("/api/nodes/"+id+"/zip", 404, nil)
		bob.get("/api/nodes/"+id+"/thumb", 404, nil)
		bob.get("/api/nodes/"+id+"/shares", 404, nil)
		bob.get("/api/nodes/"+id+"/links", 404, nil)
	}
	// Reads inside it.
	got := bob.list(f.b)
	if got.Role != "viewer" || len(got.Children) != 1 {
		t.Fatalf("bob list B: %+v", got)
	}
	// Breadcrumbs must not reveal ancestors above the share.
	fileView := bob.list(f.file)
	if len(fileView.Breadcrumbs) != 2 || fileView.Breadcrumbs[0].ID != f.b {
		t.Errorf("bob breadcrumbs: %+v", fileView.Breadcrumbs)
	}
	for _, cr := range carol.list(f.file).Breadcrumbs {
		if cr.ID == f.alice.root {
			t.Errorf("carol sees alice's root in breadcrumbs")
		}
	}

	// Viewer cannot write.
	bob.call("POST", "/api/nodes/"+f.b+"/children", map[string]string{"name": "x"}, 403, nil)
	bob.call("PATCH", "/api/nodes/"+f.file, map[string]string{"name": "renamed"}, 403, nil)
	bob.call("DELETE", "/api/nodes/"+f.file, nil, 403, nil)
	bob.call("POST", "/api/uploads", map[string]any{"parent_id": f.b, "name": "x.txt", "size": 1}, 403, nil)
	// Viewer cannot reshare or make links.
	bob.call("POST", "/api/nodes/"+f.b+"/shares", map[string]string{"email": "x@example.com", "role": "viewer"}, 403, nil)
	bob.call("POST", "/api/nodes/"+f.b+"/links", map[string]any{}, 403, nil)

	// Writes that target nodes outside the share look like "not found".
	bob.call("POST", "/api/nodes/"+f.c+"/children", map[string]string{"name": "x"}, 404, nil)
	bob.call("POST", "/api/uploads", map[string]any{"parent_id": f.c, "name": "x.txt", "size": 1}, 404, nil)
	bob.call("PATCH", "/api/nodes/"+f.secret, map[string]string{"name": "x"}, 404, nil)
	bob.call("DELETE", "/api/nodes/"+f.secret, nil, 404, nil)
	bob.call("POST", "/api/nodes/"+f.secret+"/copy", map[string]string{"parent_id": bob.root}, 404, nil)
	bob.call("POST", "/api/trash/"+f.secret+"/restore", nil, 404, nil)
	bob.call("DELETE", "/api/trash/"+f.secret, nil, 404, nil)

	// Editor (carol on A): can work inside...
	sub := carol.mkdir(f.a, "from-carol")
	up := carol.upload(sub, "c.txt", []byte("carol data"))
	carol.call("PATCH", "/api/nodes/"+up.ID, map[string]string{"name": "c2.txt"}, 200, nil)
	// ...and what she creates belongs to alice's tree.
	var owner int64
	if err := f.e.db.QueryRow(`SELECT owner_id FROM nodes WHERE id = ?`, up.ID).Scan(&owner); err != nil || owner != f.userID("admin@example.com") {
		t.Errorf("file created by editor: owner %d err %v", owner, err)
	}
	// ...but cannot move things out of the shared subtree:
	// to a folder of the owner she cannot see,
	carol.call("PATCH", "/api/nodes/"+up.ID, map[string]string{"parent_id": f.c}, 404, nil)
	carol.call("PATCH", "/api/nodes/"+up.ID, map[string]string{"parent_id": f.alice.root}, 404, nil)
	// to her own drive (would take the file away from its owner),
	carol.call("PATCH", "/api/nodes/"+up.ID, map[string]string{"parent_id": carol.root}, 403, nil)
	// nor pull outside things in,
	carol.call("PATCH", "/api/nodes/"+f.secret, map[string]string{"parent_id": f.a}, 404, nil)
	carol.call("POST", "/api/nodes/"+f.secret+"/copy", map[string]string{"parent_id": f.a}, 404, nil)
	// nor move her own files into a folder where she is not an editor.
	mine := carol.upload(carol.root, "mine.txt", []byte("mine"))
	carol.call("PATCH", "/api/nodes/"+mine.ID, map[string]string{"parent_id": f.c}, 404, nil)
	// Moving her own file into the shared folder would give it away: refused.
	carol.call("PATCH", "/api/nodes/"+mine.ID, map[string]string{"parent_id": f.a}, 403, nil)
	// She cannot rename, move or trash the shared folder itself.
	carol.call("PATCH", "/api/nodes/"+f.a, map[string]string{"name": "A2"}, 403, nil)
	carol.call("PATCH", "/api/nodes/"+f.a, map[string]string{"parent_id": f.b}, 403, nil)
	carol.call("DELETE", "/api/nodes/"+f.a, nil, 403, nil)
	// Only the owner reshares, lists shares, or deletes forever.
	carol.call("POST", "/api/nodes/"+f.a+"/shares", map[string]string{"email": "x@example.com", "role": "viewer"}, 403, nil)
	carol.get("/api/nodes/"+f.a+"/shares", 403, nil)
	carol.call("POST", "/api/nodes/"+up.ID+"/links", map[string]any{}, 403, nil)
	carol.call("DELETE", "/api/nodes/"+up.ID, nil, 200, nil) // trash: allowed for editors
	carol.call("DELETE", "/api/trash/"+up.ID, nil, 404, nil) // delete forever: owner only
	var trash struct {
		Items []nodeJSON `json:"items"`
	}
	carol.get("/api/trash", 200, &trash)
	if len(trash.Items) != 0 {
		t.Errorf("editor sees owner's trash: %+v", trash.Items)
	}
	// The editor who trashed it can undo; a different editor/viewer cannot.
	bob.call("POST", "/api/trash/"+up.ID+"/restore", nil, 404, nil)
	carol.call("POST", "/api/trash/"+up.ID+"/restore", nil, 200, nil)
	// Share ids of other people's nodes cannot be deleted.
	var bobShare int64
	if err := f.e.db.QueryRow(`SELECT id FROM shares WHERE email = 'bob@example.com'`).Scan(&bobShare); err != nil {
		t.Fatal(err)
	}
	carol.call("DELETE", "/api/shares/"+itoa(bobShare), nil, 403, nil)
	f.e.login("admin@example.com").call("POST", "/api/admin/allowlist", map[string]string{"email": "dave@example.com"}, 200, nil)
	f.e.login("dave@example.com").call("DELETE", "/api/shares/"+itoa(bobShare), nil, 404, nil)

	// Viewer may copy a shared file into their own drive; the copy is theirs.
	var cp nodeResp
	bob.call("POST", "/api/nodes/"+f.file+"/copy", map[string]string{"parent_id": bob.root}, 201, &cp)
	if err := f.e.db.QueryRow(`SELECT owner_id FROM nodes WHERE id = ?`, cp.Node.ID).Scan(&owner); err != nil || owner != f.userID("bob@example.com") {
		t.Errorf("copy owner %d err %v", owner, err)
	}
	// But not into the shared folder (viewer) or elsewhere in alice's drive.
	bob.call("POST", "/api/nodes/"+f.file+"/copy", map[string]string{"parent_id": f.b}, 403, nil)
	bob.call("POST", "/api/nodes/"+f.file+"/copy", map[string]string{"parent_id": f.c}, 404, nil)

	// Shared-with-me and search only show what is shared.
	var shared struct {
		Items []nodeJSON `json:"items"`
	}
	bob.get("/api/shared", 200, &shared)
	if len(shared.Items) != 1 || shared.Items[0].ID != f.b || shared.Items[0].Role != "viewer" || shared.Items[0].Owner != "admin@example.com" {
		t.Errorf("bob shared-with-me: %+v", shared.Items)
	}
	bob.get("/api/search?q=secret", 200, &shared)
	if len(shared.Items) != 0 {
		t.Errorf("bob found secret: %+v", shared.Items)
	}
	bob.get("/api/search?q=file", 200, &shared)
	if len(shared.Items) != 2 { // the shared file.txt and his own copy
		t.Errorf("bob search file: %+v", shared.Items)
	}
	bob.get("/api/search?q=other", 200, &shared)
	if len(shared.Items) != 0 {
		t.Errorf("bob found sibling outside share: %+v", shared.Items)
	}

	// Upload sessions are private to their creator.
	var init struct {
		ID string `json:"id"`
	}
	carol.call("POST", "/api/uploads", map[string]any{"parent_id": f.a, "name": "up.bin", "size": 4}, 201, &init)
	bob.get("/api/uploads/"+init.ID, 404, nil)
	if resp, _ := bob.raw("PUT", "/api/uploads/"+init.ID+"/chunk?offset=0", stringsReader("abcd"), nil); resp.StatusCode != 404 {
		t.Errorf("bob wrote to carol's upload: %d", resp.StatusCode)
	}
	// If the share is revoked mid-upload, completion fails.
	if resp, _ := carol.raw("PUT", "/api/uploads/"+init.ID+"/chunk?offset=0", stringsReader("abcd"), nil); resp.StatusCode != 200 {
		t.Fatalf("carol chunk: %d", resp.StatusCode)
	}
	var carolShare int64
	if err := f.e.db.QueryRow(`SELECT id FROM shares WHERE email = 'carol@example.com'`).Scan(&carolShare); err != nil {
		t.Fatal(err)
	}
	f.alice.call("DELETE", "/api/shares/"+itoa(carolShare), nil, 200, nil)
	// carol lost her only share: her session is gone entirely.
	carol.call("POST", "/api/uploads/"+init.ID+"/complete", nil, 401, nil)
	if contains(f.alice.names(f.a), "up.bin") {
		t.Errorf("upload completed after share was revoked")
	}
}

func TestUnauthenticatedAPI(t *testing.T) {
	f := newFixture(t)
	anon := f.e.anon()
	for _, p := range []string{"/api/nodes/" + f.file, "/api/nodes/" + f.file + "/content", "/api/trash", "/api/shared",
		"/api/search?q=a", "/api/usage", "/api/admin/allowlist", "/api/nodes/" + f.a + "/zip"} {
		if resp, _ := anon.raw("GET", p, nil, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anon GET %s: %d", p, resp.StatusCode)
		}
	}
	anon.call("DELETE", "/api/nodes/"+f.file, nil, 401, nil)
	anon.call("POST", "/api/uploads", map[string]any{"parent_id": f.a, "name": "x", "size": 1}, 401, nil)
	// A forged session cookie is just an unknown session.
	req, _ := http.NewRequest("GET", f.e.ts.URL+"/api/nodes/"+f.file, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "forged"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("forged cookie: %d", resp.StatusCode)
	}
}
