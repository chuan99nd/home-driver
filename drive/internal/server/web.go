package server

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed web
var webFS embed.FS

type asset struct {
	body  []byte
	ctype string
	etag  string
}

func loadAssets() map[string]asset {
	out := map[string]asset{}
	err := fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := webFS.ReadFile(p)
		if err != nil {
			return err
		}
		ctype := ""
		switch path.Ext(p) {
		case ".html":
			ctype = "text/html; charset=utf-8"
		case ".js":
			ctype = "text/javascript; charset=utf-8"
		case ".css":
			ctype = "text/css; charset=utf-8"
		case ".svg":
			ctype = "image/svg+xml"
		case ".png":
			ctype = "image/png"
		case ".webmanifest":
			ctype = "application/manifest+json"
		default:
			ctype = mime.TypeByExtension(path.Ext(p))
		}
		sum := sha256.Sum256(b)
		out[strings.TrimPrefix(p, "web/")] = asset{body: b, ctype: ctype, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		return nil
	})
	if err != nil {
		panic("embedded web assets: " + err.Error())
	}
	return out
}

func serveAsset(w http.ResponseWriter, r *http.Request, a asset) {
	appHeaders(w)
	w.Header().Set("Content-Type", a.ctype)
	// Always revalidate: a new binary must never be mixed with old scripts.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", a.etag)
	if r.Header.Get("If-None-Match") == a.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(a.body)
}

// staticHandler serves the embedded UI files under /static/ and /sw.js.
func (s *Server) staticHandler() http.Handler {
	assets := loadAssets()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		if r.URL.Path == "/sw.js" {
			name = "sw.js"
		}
		a, ok := assets[name]
		if !ok || strings.HasSuffix(name, ".html") {
			http.NotFound(w, r)
			return
		}
		serveAsset(w, r, a)
	})
}

func (s *Server) page(name string) asset {
	b, err := webFS.ReadFile("web/" + name)
	if err != nil {
		panic("embedded page " + name + ": " + err.Error())
	}
	sum := sha256.Sum256(b)
	return asset{body: b, ctype: "text/html; charset=utf-8", etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	serveAsset(w, r, s.page("index.html"))
}

// handlePublicPage serves the shell for a public link. The page itself
// reveals nothing; its script asks the API with the token from the URL.
func (s *Server) handlePublicPage(w http.ResponseWriter, r *http.Request) {
	serveAsset(w, r, s.page("public.html"))
}

func contextWithEmail(r *http.Request, email string) context.Context {
	return context.WithValue(r.Context(), curatorEmailKey{}, email)
}
