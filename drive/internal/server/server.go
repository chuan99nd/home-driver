// Package server is the HTTP application: routing, middleware, and handlers.
package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chuan99nd/home-driver/drive/internal/access"
	"github.com/chuan99nd/home-driver/drive/internal/blob"
	"github.com/chuan99nd/home-driver/drive/internal/config"
)

// maxJSONBody bounds every JSON request body.
const maxJSONBody = 64 << 10

// csrfHeader must be present on every state-changing /api request. A
// cross-site page cannot set a custom header without a CORS preflight, which
// this server never approves.
const csrfHeader = "X-Drive-Csrf"

// Server holds the application state. Create it with New.
type Server struct {
	cfg     config.Config
	db      *sql.DB
	blobs   *blob.Store
	log     *slog.Logger
	tmpDir  string
	thumbs  string
	origin  string // scheme://host of BaseURL
	handler http.Handler
	curator http.Handler // nil when the proxy is disabled

	// blobMu serializes "make a blob referenced" (upload completion) against
	// "remove an unreferenced blob" (purge), so a blob file can never be
	// deleted while an upload of the same content is being committed.
	blobMu sync.Mutex

	uploadLocks sync.Map // upload id -> *sync.Mutex
	thumbSem    chan struct{}
	oidc        oidcState
	unlock      limiter
	davFails    failCounter
	davLocks    davLockSystems

	// Now is replaceable in tests.
	Now func() time.Time
}

// New wires up the server. dataDir must already exist: creating it here could
// silently write user data to the root filesystem when the data disk is not
// mounted.
func New(cfg config.Config, db *sql.DB, log *slog.Logger) (*Server, error) {
	st, err := os.Stat(cfg.DataDir)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("DRIVE_DATA_DIR %q must be an existing directory (is the data disk mounted?)", cfg.DataDir)
	}
	blobs, err := blob.New(filepath.Join(cfg.DataDir, "blobs"))
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:      cfg,
		db:       db,
		blobs:    blobs,
		log:      log,
		tmpDir:   filepath.Join(cfg.DataDir, "tmp"),
		thumbs:   filepath.Join(cfg.DataDir, "thumbs"),
		thumbSem: make(chan struct{}, 2),
		Now:      time.Now,
	}
	for _, d := range []string{s.tmpDir, s.thumbs} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	s.origin = u.Scheme + "://" + u.Host
	if cfg.CuratorURL != "" {
		s.curator, err = newCuratorProxy(cfg.CuratorURL, log)
		if err != nil {
			return nil, err
		}
	}
	s.handler = s.routes()
	return s, nil
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)

	// Auth.
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	if s.cfg.DevLogin {
		mux.HandleFunc("GET /dev/login", s.handleDevLogin)
	}
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("POST /api/logout", s.handleLogout)

	// Nodes.
	mux.HandleFunc("GET /api/nodes/{id}", s.user(s.handleGetNode))
	mux.HandleFunc("POST /api/nodes/{id}/children", s.user(s.handleMkdir))
	mux.HandleFunc("PATCH /api/nodes/{id}", s.user(s.handlePatchNode))
	mux.HandleFunc("POST /api/nodes/{id}/copy", s.user(s.handleCopy))
	mux.HandleFunc("DELETE /api/nodes/{id}", s.user(s.handleTrash))
	mux.HandleFunc("GET /api/nodes/{id}/content", s.user(s.handleContent))
	mux.HandleFunc("GET /api/nodes/{id}/thumb", s.user(s.handleThumb))
	mux.HandleFunc("GET /api/nodes/{id}/zip", s.user(s.handleZip))
	mux.HandleFunc("GET /api/search", s.user(s.handleSearch))
	mux.HandleFunc("GET /api/usage", s.user(s.handleUsage))
	mux.HandleFunc("GET /api/shared", s.user(s.handleSharedWithMe))

	// Sharing.
	mux.HandleFunc("GET /api/nodes/{id}/shares", s.user(s.handleListShares))
	mux.HandleFunc("POST /api/nodes/{id}/shares", s.user(s.handleAddShare))
	mux.HandleFunc("DELETE /api/shares/{sid}", s.user(s.handleDeleteShare))
	mux.HandleFunc("GET /api/nodes/{id}/links", s.user(s.handleListLinks))
	mux.HandleFunc("POST /api/nodes/{id}/links", s.user(s.handleCreateLink))
	mux.HandleFunc("DELETE /api/links/{lid}", s.user(s.handleDeleteLink))

	// Uploads.
	mux.HandleFunc("POST /api/uploads", s.user(s.handleUploadInit))
	mux.HandleFunc("GET /api/uploads/{uid}", s.user(s.handleUploadStatus))
	mux.HandleFunc("PUT /api/uploads/{uid}/chunk", s.user(s.handleUploadChunk))
	mux.HandleFunc("POST /api/uploads/{uid}/complete", s.user(s.handleUploadComplete))
	mux.HandleFunc("DELETE /api/uploads/{uid}", s.user(s.handleUploadAbort))

	// Trash.
	mux.HandleFunc("GET /api/trash", s.user(s.handleListTrash))
	mux.HandleFunc("POST /api/trash/{id}/restore", s.user(s.handleRestore))
	mux.HandleFunc("DELETE /api/trash/{id}", s.user(s.handlePurgeOne))
	mux.HandleFunc("DELETE /api/trash", s.user(s.handleEmptyTrash))

	// Admin.
	mux.HandleFunc("GET /api/admin/allowlist", s.admin(s.handleAllowlistGet))
	mux.HandleFunc("POST /api/admin/allowlist", s.admin(s.handleAllowlistAdd))
	mux.HandleFunc("DELETE /api/admin/allowlist", s.admin(s.handleAllowlistRemove))

	// Public links (no session).
	mux.HandleFunc("GET /s/{token}", s.handlePublicPage)
	mux.HandleFunc("GET /api/public/{token}", s.handlePublicInfo)
	mux.HandleFunc("POST /api/public/{token}/unlock", s.handlePublicUnlock)
	mux.HandleFunc("GET /api/public/{token}/nodes/{id}", s.handlePublicNode)
	mux.HandleFunc("GET /api/public/{token}/nodes/{id}/content", s.handlePublicContent)
	mux.HandleFunc("GET /api/public/{token}/nodes/{id}/thumb", s.handlePublicThumb)
	mux.HandleFunc("GET /api/public/{token}/nodes/{id}/zip", s.handlePublicZip)

	// App passwords (managed with a session) and WebDAV (HTTP Basic only).
	mux.HandleFunc("GET /api/app-passwords", s.user(s.handleAppPasswordList))
	mux.HandleFunc("POST /api/app-passwords", s.user(s.handleAppPasswordCreate))
	mux.HandleFunc("DELETE /api/app-passwords/{pid}", s.user(s.handleAppPasswordDelete))
	mux.HandleFunc("/dav", s.handleDAV)
	mux.HandleFunc("/dav/", s.handleDAV)

	// Curator reverse proxy (admin only).
	mux.HandleFunc("/curator", s.handleCurator)
	mux.HandleFunc("/curator/", s.handleCurator)

	// Unknown API paths answer JSON, not the UI shell.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, http.StatusNotFound, "not_found", "no such endpoint")
	})

	// Embedded UI.
	static := s.staticHandler()
	mux.Handle("GET /static/", static)
	mux.Handle("GET /sw.js", static) // must live at the root to control "/"
	mux.HandleFunc("GET /{$}", s.handleIndex)

	return s.recoverer(s.logging(s.csrf(mux)))
}

// ---- middleware ----

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// ReadFrom keeps sendfile working for downloads.
func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := io.Copy(w.ResponseWriter, r)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type ctxKey int

const logEmailKey ctxKey = 1

// logging writes one structured line per request. It logs the route pattern,
// never the raw path, so share-link tokens do not end up in logs.
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		var email string
		r = r.WithContext(context.WithValue(r.Context(), logEmailKey, &email))
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" {
			return
		}
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		s.log.Info("request", "method", r.Method, "route", route, "status", sw.status,
			"bytes", sw.bytes, "ms", time.Since(start).Milliseconds(), "user", email)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "value", fmt.Sprint(v), "method", r.Method)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// csrf rejects cross-site state-changing requests.
//
//   - Sec-Fetch-Site, when the browser sends it, must be same-origin (or
//     "none", a user-typed navigation).
//   - Origin, when present, must equal the configured base URL origin.
//   - /api requests must additionally carry the custom X-Drive-Csrf header.
//
// Safe methods are never state-changing in this application.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
			s.fail(w, http.StatusForbidden, "csrf", "cross-site request refused")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != s.origin {
			s.fail(w, http.StatusForbidden, "csrf", "cross-origin request refused")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Header.Get(csrfHeader) == "" {
			s.fail(w, http.StatusForbidden, "csrf", "missing "+csrfHeader+" header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// appHeaders sets the security headers for pages and JSON served by the app
// itself (not user content, not the curator proxy).
func appHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' blob: data:; media-src 'self' blob:; frame-src 'self'; "+
			"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'self'")
}

// ---- JSON helpers ----

func (s *Server) json(w http.ResponseWriter, status int, v any) {
	appHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Debug("write response", "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, status int, code, msg string) {
	s.json(w, status, map[string]string{"error": code, "message": msg})
}

// internal logs err and answers 500 without leaking details.
func (s *Server) internal(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.log.Error("internal error", "method", r.Method, "route", r.Pattern, "err", err)
	s.fail(w, http.StatusInternalServerError, "internal", "internal error")
}

// readJSON decodes a small JSON body into v. It answers the error itself.
func (s *Server) readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.fail(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
			return false
		}
		s.fail(w, http.StatusBadRequest, "bad_json", "invalid JSON body")
		return false
	}
	return true
}

// ---- authorization: the one way handlers reach a node ----

// authz resolves the principal's access to a node and enforces a minimum
// level. It answers 404 when the principal may not even see the node (so
// existence is not leaked) and 403 when it may see but not do. Nodes in the
// trash are 404 here; trash handlers use authzTrashed.
func (s *Server) authz(w http.ResponseWriter, r *http.Request, p access.Principal, id string, min access.Level) (access.Result, bool) {
	res, err := access.Resolve(r.Context(), s.db, p, id)
	if errors.Is(err, access.ErrNotFound) {
		s.fail(w, http.StatusNotFound, "not_found", "not found")
		return res, false
	}
	if err != nil {
		s.internal(w, r, err)
		return res, false
	}
	if res.Level == access.None || res.Trashed {
		s.fail(w, http.StatusNotFound, "not_found", "not found")
		return res, false
	}
	if res.Level < min {
		s.fail(w, http.StatusForbidden, "forbidden", "you do not have permission to do this")
		return res, false
	}
	return res, true
}

// authzTrashed is authz for the trash endpoints: owner only, and the node
// itself must be the one that was put in the trash.
func (s *Server) authzTrashed(w http.ResponseWriter, r *http.Request, p access.Principal, id string) (access.Result, bool) {
	res, err := access.Resolve(r.Context(), s.db, p, id)
	if err != nil && !errors.Is(err, access.ErrNotFound) {
		s.internal(w, r, err)
		return res, false
	}
	if err != nil || res.Level != access.Owner || res.Node.TrashedAt == 0 {
		s.fail(w, http.StatusNotFound, "not_found", "not found in trash")
		return res, false
	}
	return res, true
}

// ---- small helpers ----

func (s *Server) now() int64 { return s.Now().Unix() }

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// validName checks a file or folder name.
func validName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || len(name) > 255 || !isValidUTF8(name) {
		return "", false
	}
	for _, c := range name {
		if c == '/' || c == '\\' || c < 0x20 || c == 0x7f {
			return "", false
		}
	}
	return name, true
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "db unavailable")
		return
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "blobs")); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "data dir unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}
