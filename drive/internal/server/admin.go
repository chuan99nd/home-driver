package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// ---- allowlist ----

func (s *Server) handleAllowlistGet(w http.ResponseWriter, r *http.Request, u *User) {
	rows, err := s.db.QueryContext(r.Context(), `SELECT email, added_by, created_at FROM allowlist ORDER BY email`)
	if err != nil {
		s.internal(w, r, fmt.Errorf("list allowlist: %w", err))
		return
	}
	defer rows.Close()
	type entry struct {
		Email     string `json:"email"`
		AddedBy   string `json:"added_by"`
		CreatedAt int64  `json:"created_at"`
	}
	items := []entry{}
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.Email, &e.AddedBy, &e.CreatedAt); err != nil {
			s.internal(w, r, fmt.Errorf("list allowlist: %w", err))
			return
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		s.internal(w, r, fmt.Errorf("list allowlist: %w", err))
		return
	}
	s.json(w, http.StatusOK, map[string]any{"items": items, "admins": s.cfg.AdminEmails})
}

func (s *Server) handleAllowlistAdd(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Email string `json:"email"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	email, ok := parseEmail(in.Email)
	if !ok {
		s.fail(w, http.StatusBadRequest, "bad_email", "not a valid email address")
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `INSERT INTO allowlist(email, added_by, created_at) VALUES (?, ?, ?) ON CONFLICT(email) DO NOTHING`,
		email, u.Email, s.now()); err != nil {
		s.internal(w, r, fmt.Errorf("add to allowlist: %w", err))
		return
	}
	s.log.Info("allowlist add", "email", email, "by", u.Email)
	s.json(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAllowlistRemove(w http.ResponseWriter, r *http.Request, u *User) {
	email, ok := parseEmail(r.URL.Query().Get("email"))
	if !ok {
		s.fail(w, http.StatusBadRequest, "bad_email", "not a valid email address")
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM allowlist WHERE email = ?`, email); err != nil {
		s.internal(w, r, fmt.Errorf("remove from allowlist: %w", err))
		return
	}
	s.log.Info("allowlist remove", "email", email, "by", u.Email)
	s.json(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- curator reverse proxy ----

type curatorEmailKey struct{}

// newCuratorProxy builds the reverse proxy to the curator. The curator has
// no auth of its own: it trusts X-Home-Driver-Email, so this proxy removes
// every client-supplied X-Home-Driver-* header and sets the real one.
func newCuratorProxy(target string, log *slog.Logger) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("parse CURATOR_URL: %w", err)
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			email, _ := pr.In.Context().Value(curatorEmailKey{}).(string)
			pr.SetURL(u)
			// Strip the /curator prefix.
			p := strings.TrimPrefix(pr.In.URL.Path, "/curator")
			if p == "" {
				p = "/"
			}
			pr.Out.URL.Path = strings.TrimRight(u.Path, "/") + p
			pr.Out.URL.RawPath = ""
			// The curator runs its own same-origin check on state-changing
			// requests: it compares Origin with Host / X-Forwarded-Host. So
			// the browser's Host and Origin are passed through unchanged.
			// (Rewrite already dropped any client-supplied X-Forwarded-*.)
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			for k := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(k), "x-home-driver-") {
					delete(pr.Out.Header, k)
				}
			}
			pr.Out.Header.Set("X-Home-Driver-Email", email)
			pr.Out.Header.Set("X-Forwarded-Prefix", "/curator")
			// The curator has no use for Drive's cookies.
			pr.Out.Header.Del("Cookie")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn("curator proxy", "err", err)
			http.Error(w, "The photo cleanup service is not running.", http.StatusBadGateway)
		},
	}, nil
}

func (s *Server) handleCurator(w http.ResponseWriter, r *http.Request) {
	if s.curator == nil {
		s.fail(w, http.StatusNotFound, "not_found", "photo cleanup is not configured")
		return
	}
	u, err := s.currentUser(r)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if u == nil {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		s.fail(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
		return
	}
	if !u.IsAdmin {
		s.fail(w, http.StatusForbidden, "forbidden", "admin only")
		return
	}
	if r.URL.Path == "/curator" {
		http.Redirect(w, r, "/curator/", http.StatusPermanentRedirect)
		return
	}
	s.curator.ServeHTTP(w, r.WithContext(contextWithEmail(r, u.Email)))
}
