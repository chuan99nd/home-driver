package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/chuan99nd/home-driver/drive/internal/access"
	"github.com/chuan99nd/home-driver/drive/internal/config"
)

const (
	sessionCookie = "drive_session"
	oauthCookie   = "drive_oauth"
	sessionTTL    = 30 * 24 * time.Hour
)

// User is the signed-in account for a request.
type User struct {
	ID      int64
	Email   string
	Name    string
	RootID  string
	IsAdmin bool
}

func (u *User) principal() access.Principal {
	return access.Principal{UserID: u.ID, Email: u.Email}
}

func isValidUTF8(s string) bool { return utf8.ValidString(s) }

// mac returns hex(HMAC-SHA256(session secret, purpose || 0 || msg)).
func (s *Server) mac(purpose, msg string) string {
	h := hmac.New(sha256.New, []byte(s.cfg.SessionSecret))
	h.Write([]byte(purpose))
	h.Write([]byte{0})
	h.Write([]byte(msg))
	return hex.EncodeToString(h.Sum(nil))
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// parseEmail validates and normalizes a bare email address.
func parseEmail(in string) (string, bool) {
	e := config.NormalizeEmail(in)
	if len(e) > 254 || strings.ContainsAny(e, " <>,;\"") {
		return "", false
	}
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e {
		return "", false
	}
	return e, true
}

// loginAllowed implements the sign-in rule: admin, allowlisted, or the
// recipient of at least one share.
func (s *Server) loginAllowed(ctx context.Context, email string) (bool, error) {
	email = config.NormalizeEmail(email)
	if email == "" {
		return false, nil
	}
	if s.cfg.IsAdmin(email) {
		return true, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM allowlist WHERE email = ?1) + (SELECT COUNT(*) FROM shares WHERE email = ?1)`, email).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check login allowed: %w", err)
	}
	return n > 0, nil
}

// ensureUser returns the user for email, creating the account and its root
// folder on first sign-in.
func (s *Server) ensureUser(ctx context.Context, email, name string) (*User, error) {
	email = config.NormalizeEmail(email)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("ensure user: %w", err)
	}
	defer tx.Rollback()
	u := &User{Email: email, IsAdmin: s.cfg.IsAdmin(email)}
	var root sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id, name, root_id FROM users WHERE email = ?`, email).Scan(&u.ID, &u.Name, &root)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx, `INSERT INTO users(email, name, created_at) VALUES (?, ?, ?)`, email, name, s.now())
		if err != nil {
			return nil, fmt.Errorf("create user: %w", err)
		}
		u.ID, _ = res.LastInsertId()
		u.Name = name
	case err != nil:
		return nil, fmt.Errorf("load user: %w", err)
	default:
		if name != "" && name != u.Name {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET name = ? WHERE id = ?`, name, u.ID); err != nil {
				return nil, fmt.Errorf("update user: %w", err)
			}
			u.Name = name
		}
	}
	u.RootID = root.String
	if u.RootID == "" {
		u.RootID = randomHex(16)
		now := s.now()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nodes(id, owner_id, parent_id, name, is_dir, created_at, updated_at) VALUES (?, ?, NULL, 'My Drive', 1, ?, ?)`,
			u.RootID, u.ID, now, now); err != nil {
			return nil, fmt.Errorf("create root folder: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET root_id = ? WHERE id = ?`, u.RootID, u.ID); err != nil {
			return nil, fmt.Errorf("set root folder: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("ensure user: %w", err)
	}
	return u, nil
}

// startSession creates a server-side session and sets the cookie.
func (s *Server) startSession(ctx context.Context, w http.ResponseWriter, u *User) error {
	token := randomToken()
	now := s.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(id, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		s.mac("session", token), u.ID, now.Unix(), now.Add(sessionTTL).Unix())
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.cfg.Secure(), MaxAge: int(sessionTTL.Seconds()),
	})
	return nil
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.cfg.Secure(), MaxAge: -1,
	})
}

// currentUser returns the signed-in user or nil. A session whose account is
// no longer allowed to sign in (allowlist entry or last share removed) is
// deleted on the spot, so revocation takes effect immediately.
func (s *Server) currentUser(r *http.Request) (*User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	sid := s.mac("session", c.Value)
	u := &User{}
	var root sql.NullString
	err = s.db.QueryRowContext(r.Context(), `
		SELECT u.id, u.email, u.name, u.root_id FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id = ? AND s.expires_at > ?`, sid, s.now()).Scan(&u.ID, &u.Email, &u.Name, &root)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	u.RootID = root.String
	ok, err := s.loginAllowed(r.Context(), u.Email)
	if err != nil {
		return nil, err
	}
	if !ok {
		if _, err := s.db.ExecContext(r.Context(), `DELETE FROM sessions WHERE user_id = ?`, u.ID); err != nil {
			return nil, fmt.Errorf("drop sessions: %w", err)
		}
		return nil, nil
	}
	u.IsAdmin = s.cfg.IsAdmin(u.Email)
	if p, ok := r.Context().Value(logEmailKey).(*string); ok {
		*p = u.Email
	}
	return u, nil
}

type userHandler func(w http.ResponseWriter, r *http.Request, u *User)

// user wraps a handler that requires a signed-in user.
func (s *Server) user(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.currentUser(r)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		if u == nil {
			s.fail(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
			return
		}
		h(w, r, u)
	}
}

// admin wraps a handler that requires an admin user.
func (s *Server) admin(h userHandler) http.HandlerFunc {
	return s.user(func(w http.ResponseWriter, r *http.Request, u *User) {
		if !u.IsAdmin {
			s.fail(w, http.StatusForbidden, "forbidden", "admin only")
			return
		}
		h(w, r, u)
	})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	u, err := s.currentUser(r)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := map[string]any{
		"authenticated": u != nil,
		"google_login":  s.cfg.GoogleClientID != "",
		"dev_login":     s.cfg.DevLogin,
	}
	if u != nil {
		out["user"] = map[string]any{"email": u.Email, "name": u.Name, "is_admin": u.IsAdmin, "root_id": u.RootID}
		out["curator"] = u.IsAdmin && s.curator != nil
		out["trash_days"] = s.cfg.TrashDays
		out["chunk_size"] = s.cfg.ChunkSize
		out["dav_url"] = s.cfg.BaseURL + "/dav/"
	}
	s.json(w, http.StatusOK, out)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if _, err := s.db.ExecContext(r.Context(), `DELETE FROM sessions WHERE id = ?`, s.mac("session", c.Value)); err != nil {
			s.internal(w, r, err)
			return
		}
	}
	s.clearSessionCookie(w)
	s.json(w, http.StatusOK, map[string]bool{"ok": true})
}

// signIn is the common tail of Google and dev login.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request, email, name string) {
	ok, err := s.loginAllowed(r.Context(), email)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !ok {
		s.log.Warn("sign-in refused: not allowed", "email", config.NormalizeEmail(email))
		// The UI shows a friendly "ask the owner to share with this email" page.
		http.Redirect(w, r, "/?denied=1", http.StatusSeeOther)
		return
	}
	u, err := s.ensureUser(r.Context(), email, name)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if err := s.startSession(r.Context(), w, u); err != nil {
		s.internal(w, r, err)
		return
	}
	s.log.Info("sign-in", "email", u.Email)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) loginError(w http.ResponseWriter, status int, msg string) {
	appHeaders(w)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintln(w, msg)
}

// handleDevLogin is registered only when DRIVE_DEV_LOGIN=1, which config
// validation allows only on loopback. It applies the same allow rule as the
// real login.
func (s *Server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	email, ok := parseEmail(r.URL.Query().Get("email"))
	if !ok {
		s.loginError(w, http.StatusBadRequest, "usage: /dev/login?email=someone@example.com")
		return
	}
	s.signIn(w, r, email, "")
}

// ---- Google OIDC ----

type oidcState struct {
	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
}

// oidcClient discovers Google's endpoints on first use (not at startup, so
// the server can boot while the internet is down).
func (s *Server) oidcClient(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	s.oidc.mu.Lock()
	defer s.oidc.mu.Unlock()
	if s.oidc.oauth != nil {
		return s.oidc.oauth, s.oidc.verifier, nil
	}
	// Discovery must not be tied to the request context: the provider keeps
	// it for later key refreshes.
	dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = ctx
	provider, err := oidc.NewProvider(dctx, s.cfg.OIDCIssuer)
	if err != nil {
		return nil, nil, fmt.Errorf("oidc discovery: %w", err)
	}
	s.oidc.oauth = &oauth2.Config{
		ClientID:     s.cfg.GoogleClientID,
		ClientSecret: s.cfg.GoogleClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  s.cfg.BaseURL + "/auth/callback",
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
	s.oidc.verifier = provider.Verifier(&oidc.Config{ClientID: s.cfg.GoogleClientID})
	return s.oidc.oauth, s.oidc.verifier, nil
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.GoogleClientID == "" {
		s.loginError(w, http.StatusNotFound, "Google sign-in is not configured.")
		return
	}
	oc, _, err := s.oidcClient(r.Context())
	if err != nil {
		s.log.Error("oidc unavailable", "err", err)
		s.loginError(w, http.StatusBadGateway, "Could not reach Google to sign in. Try again in a moment.")
		return
	}
	state, nonce, verifier := randomToken(), randomToken(), oauth2.GenerateVerifier()
	// The cookie binds state, nonce and PKCE verifier to this browser.
	payload := state + "." + nonce + "." + verifier
	http.SetCookie(w, &http.Cookie{
		Name: oauthCookie, Value: payload + "." + s.mac("oauth", payload), Path: "/auth", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.cfg.Secure(), MaxAge: 600,
	})
	http.Redirect(w, r, oc.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if s.cfg.GoogleClientID == "" {
		s.loginError(w, http.StatusNotFound, "Google sign-in is not configured.")
		return
	}
	c, err := r.Cookie(oauthCookie)
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: "", Path: "/auth", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.cfg.Secure(), MaxAge: -1})
	if err != nil {
		s.loginError(w, http.StatusBadRequest, "Sign-in expired. Go back and try again.")
		return
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 4 || !hmac.Equal([]byte(parts[3]), []byte(s.mac("oauth", strings.Join(parts[:3], ".")))) ||
		!hmac.Equal([]byte(parts[0]), []byte(r.URL.Query().Get("state"))) {
		s.loginError(w, http.StatusBadRequest, "Sign-in state mismatch. Go back and try again.")
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		s.loginError(w, http.StatusForbidden, "Google sign-in was cancelled or failed.")
		return
	}
	oc, verifier, err := s.oidcClient(r.Context())
	if err != nil {
		s.loginError(w, http.StatusBadGateway, "Could not reach Google to sign in. Try again in a moment.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	tok, err := oc.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(parts[2]))
	if err != nil {
		s.log.Warn("oauth exchange failed", "err", err)
		s.loginError(w, http.StatusForbidden, "Google sign-in failed.")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := verifier.Verify(ctx, raw)
	if err != nil {
		s.log.Warn("id token rejected", "err", err)
		s.loginError(w, http.StatusForbidden, "Google sign-in failed.")
		return
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idt.Claims(&claims); err != nil || !hmac.Equal([]byte(idt.Nonce), []byte(parts[1])) {
		s.loginError(w, http.StatusForbidden, "Google sign-in failed.")
		return
	}
	if !claims.EmailVerified || claims.Email == "" {
		s.loginError(w, http.StatusForbidden, "Your Google account email is not verified.")
		return
	}
	s.signIn(w, r, claims.Email, claims.Name)
}
