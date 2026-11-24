package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chuan99nd/home-driver/drive/internal/config"
)

// App passwords let sync clients (WebDAV) authenticate with HTTP Basic,
// because they cannot do Google sign-in. A signed-in user creates one per
// device; it is shown once and only its sha256 is stored. The secret has 160
// random bits, so a fast hash is enough (there is nothing to brute-force).

const (
	appPasswordPrefix = "hd-"
	maxAppPasswords   = 30

	// Failed Basic-auth attempts allowed per window before answering 429.
	davFailWindow     = 5 * time.Minute
	davMaxFailsSource = 20
	davMaxFailsAcct   = 10
)

func newAppPassword() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return appPasswordPrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

func appPasswordHash(pw string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(pw))))
	return hex.EncodeToString(sum[:])
}

type appPasswordJSON struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
	Password   string `json:"password,omitempty"` // only in the creation response
}

func (s *Server) handleAppPasswordList(w http.ResponseWriter, r *http.Request, u *User) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, name, created_at, COALESCE(last_used_at, 0) FROM app_passwords WHERE user_id = ? ORDER BY created_at DESC, id DESC`, u.ID)
	if err != nil {
		s.internal(w, r, fmt.Errorf("list app passwords: %w", err))
		return
	}
	defer rows.Close()
	items := []appPasswordJSON{}
	for rows.Next() {
		var a appPasswordJSON
		if err := rows.Scan(&a.ID, &a.Name, &a.CreatedAt, &a.LastUsedAt); err != nil {
			s.internal(w, r, fmt.Errorf("list app passwords: %w", err))
			return
		}
		items = append(items, a)
	}
	if err := rows.Err(); err != nil {
		s.internal(w, r, fmt.Errorf("list app passwords: %w", err))
		return
	}
	s.json(w, http.StatusOK, map[string]any{"items": items, "dav_url": s.cfg.BaseURL + "/dav/", "username": u.Email})
}

func (s *Server) handleAppPasswordCreate(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Name string `json:"name"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 60 || !isValidUTF8(name) || strings.ContainsFunc(name, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
		s.fail(w, http.StatusBadRequest, "bad_name", "give the device a short name")
		return
	}
	var n int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM app_passwords WHERE user_id = ?`, u.ID).Scan(&n); err != nil {
		s.internal(w, r, fmt.Errorf("count app passwords: %w", err))
		return
	}
	if n >= maxAppPasswords {
		s.fail(w, http.StatusConflict, "too_many", "too many device passwords; remove one first")
		return
	}
	pw := newAppPassword()
	now := s.now()
	res, err := s.db.ExecContext(r.Context(), `INSERT INTO app_passwords(user_id, name, token_hash, created_at) VALUES (?, ?, ?, ?)`,
		u.ID, name, appPasswordHash(pw), now)
	if err != nil {
		s.internal(w, r, fmt.Errorf("create app password: %w", err))
		return
	}
	id, _ := res.LastInsertId()
	s.log.Info("app password created", "user", u.Email, "id", id)
	s.json(w, http.StatusCreated, map[string]any{"item": appPasswordJSON{ID: id, Name: name, CreatedAt: now, Password: pw}})
}

func (s *Server) handleAppPasswordDelete(w http.ResponseWriter, r *http.Request, u *User) {
	pid, _ := strconv.ParseInt(r.PathValue("pid"), 10, 64)
	res, err := s.db.ExecContext(r.Context(), `DELETE FROM app_passwords WHERE id = ? AND user_id = ?`, pid, u.ID)
	if err != nil {
		s.internal(w, r, fmt.Errorf("delete app password: %w", err))
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		s.fail(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	s.log.Info("app password revoked", "user", u.Email, "id", pid)
	s.json(w, http.StatusOK, map[string]bool{"ok": true})
}

// checkAppPassword returns the user for a valid email + app password, or nil.
// The login rule still applies: an account that may no longer sign in cannot
// use its app passwords either.
func (s *Server) checkAppPassword(ctx context.Context, email, password string) (*User, error) {
	email = config.NormalizeEmail(email)
	want := []byte(appPasswordHash(password))
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.token_hash, COALESCE(a.last_used_at, 0), u.id, u.email, u.name, COALESCE(u.root_id, '')
		FROM app_passwords a JOIN users u ON u.id = a.user_id WHERE u.email = ?`, email)
	if err != nil {
		return nil, fmt.Errorf("check app password: %w", err)
	}
	var found *User
	var foundID, lastUsed int64
	for rows.Next() {
		var id, used int64
		var hash string
		u := &User{}
		if err := rows.Scan(&id, &hash, &used, &u.ID, &u.Email, &u.Name, &u.RootID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("check app password: %w", err)
		}
		// Constant-time compare, and no early exit.
		if subtle.ConstantTimeCompare([]byte(hash), want) == 1 {
			found, foundID, lastUsed = u, id, used
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("check app password: %w", err)
	}
	if found == nil || found.RootID == "" {
		return nil, nil
	}
	ok, err := s.loginAllowed(ctx, found.Email)
	if err != nil || !ok {
		return nil, err
	}
	found.IsAdmin = s.cfg.IsAdmin(found.Email)
	if now := s.now(); now-lastUsed >= 60 {
		if _, err := s.db.ExecContext(ctx, `UPDATE app_passwords SET last_used_at = ? WHERE id = ?`, now, foundID); err != nil {
			return nil, fmt.Errorf("touch app password: %w", err)
		}
	}
	return found, nil
}

// clientIP returns the address to rate-limit on. Forwarding headers are
// trusted only when the connection itself comes from loopback, which is how
// every request arrives behind the Cloudflare tunnel (cloudflared runs on
// this machine). A direct connection from elsewhere is keyed by its own
// address, whatever headers it sends.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote := net.ParseIP(host)
	if remote == nil || !remote.IsLoopback() {
		return host
	}
	if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); ip != nil {
		return ip.String()
	}
	// Cloudflare appends the address it saw; the right-most entry is the
	// only one not under the client's control.
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
		return ip.String()
	}
	return host
}

// failCounter counts failures per key in a fixed window.
type failCounter struct {
	mu sync.Mutex
	m  map[string]*window
}

func (f *failCounter) entry(key string, now time.Time, per time.Duration) *window {
	if f.m == nil {
		f.m = map[string]*window{}
	}
	w := f.m[key]
	if w == nil || now.Sub(w.start) > per {
		if len(f.m) > 20000 { // bound memory under a flood of distinct keys
			f.m = map[string]*window{}
		}
		w = &window{start: now}
		f.m[key] = w
	}
	return w
}

// blocked reports whether key already has max failures in the window.
func (f *failCounter) blocked(key string, now time.Time, max int, per time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entry(key, now, per).count >= max
}

func (f *failCounter) fail(key string, now time.Time, per time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entry(key, now, per).count++
}

// davAuth authenticates a WebDAV request with HTTP Basic (email + app
// password). Session cookies are deliberately ignored here: DAV verbs must
// not be reachable with ambient browser credentials.
func (s *Server) davAuth(w http.ResponseWriter, r *http.Request) (*User, bool) {
	challenge := func() {
		w.Header().Set("WWW-Authenticate", `Basic realm="Drive", charset="UTF-8"`)
		http.Error(w, "Sign in with your email and a device password from Drive's \"Sync devices\" screen.", http.StatusUnauthorized)
	}
	email, password, ok := r.BasicAuth()
	if !ok {
		challenge()
		return nil, false
	}
	email = config.NormalizeEmail(email)
	now := s.Now()
	srcKey, acctKey := "ip:"+clientIP(r), "acct:"+email
	if s.davFails.blocked(srcKey, now, davMaxFailsSource, davFailWindow) || s.davFails.blocked(acctKey, now, davMaxFailsAcct, davFailWindow) {
		w.Header().Set("Retry-After", strconv.Itoa(int(davFailWindow.Seconds())))
		http.Error(w, "Too many failed sign-in attempts. Wait a few minutes.", http.StatusTooManyRequests)
		return nil, false
	}
	u, err := s.checkAppPassword(r.Context(), email, password)
	if err != nil {
		s.log.Error("dav auth", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}
	if u == nil {
		s.davFails.fail(srcKey, now, davFailWindow)
		s.davFails.fail(acctKey, now, davFailWindow)
		s.log.Warn("dav sign-in failed", "email", email, "source", clientIP(r))
		challenge()
		return nil, false
	}
	if p, ok := r.Context().Value(logEmailKey).(*string); ok {
		*p = u.Email
	}
	return u, true
}
