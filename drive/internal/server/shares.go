package server

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chuan99nd/home-driver/drive/internal/access"
)

// ---- shares with people (owner only) ----

type shareJSON struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"created_at"`
}

// authzShareable authorizes managing shares and links: owner only, never a root.
func (s *Server) authzShareable(w http.ResponseWriter, r *http.Request, u *User, id string) (access.Result, bool) {
	res, ok := s.authz(w, r, u.principal(), id, access.Owner)
	if !ok {
		return res, false
	}
	if res.Node.ParentID == "" {
		s.fail(w, http.StatusBadRequest, "is_root", "the whole drive cannot be shared; share a folder instead")
		return res, false
	}
	return res, true
}

func (s *Server) handleListShares(w http.ResponseWriter, r *http.Request, u *User) {
	res, ok := s.authz(w, r, u.principal(), r.PathValue("id"), access.Owner)
	if !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT id, email, role, created_at FROM shares WHERE node_id = ? ORDER BY email`, res.Node.ID)
	if err != nil {
		s.internal(w, r, fmt.Errorf("list shares: %w", err))
		return
	}
	defer rows.Close()
	out := []shareJSON{}
	for rows.Next() {
		var sh shareJSON
		if err := rows.Scan(&sh.ID, &sh.Email, &sh.Role, &sh.CreatedAt); err != nil {
			s.internal(w, r, fmt.Errorf("list shares: %w", err))
			return
		}
		out = append(out, sh)
	}
	if err := rows.Err(); err != nil {
		s.internal(w, r, fmt.Errorf("list shares: %w", err))
		return
	}
	s.json(w, http.StatusOK, map[string]any{"shares": out})
}

// handleAddShare shares a node with one or more emails, or changes the role
// of an existing share. Sharing with an email also lets that account sign in.
func (s *Server) handleAddShare(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Email  string   `json:"email"`
		Emails []string `json:"emails"`
		Role   string   `json:"role"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	if in.Email != "" {
		in.Emails = append(in.Emails, in.Email)
	}
	if in.Role != "viewer" && in.Role != "editor" {
		s.fail(w, http.StatusBadRequest, "bad_role", `role must be "viewer" or "editor"`)
		return
	}
	if len(in.Emails) == 0 || len(in.Emails) > 50 {
		s.fail(w, http.StatusBadRequest, "bad_email", "give between 1 and 50 email addresses")
		return
	}
	var emails []string
	for _, e := range in.Emails {
		email, ok := parseEmail(e)
		if !ok {
			s.fail(w, http.StatusBadRequest, "bad_email", "not a valid email address: "+strings.TrimSpace(e))
			return
		}
		if email == u.Email {
			s.fail(w, http.StatusBadRequest, "self_share", "you already own this")
			return
		}
		emails = append(emails, email)
	}
	res, ok := s.authzShareable(w, r, u, r.PathValue("id"))
	if !ok {
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer tx.Rollback()
	for _, email := range emails {
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO shares(node_id, email, role, created_by, created_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(node_id, email) DO UPDATE SET role = excluded.role`, res.Node.ID, email, in.Role, u.ID, s.now()); err != nil {
			s.internal(w, r, fmt.Errorf("add share: %w", err))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		s.internal(w, r, err)
		return
	}
	s.log.Info("shared", "node", res.Node.ID, "by", u.Email, "with", strings.Join(emails, ","), "role", in.Role)
	s.json(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteShare(w http.ResponseWriter, r *http.Request, u *User) {
	sid, _ := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	var nodeID string
	err := s.db.QueryRowContext(r.Context(), `SELECT node_id FROM shares WHERE id = ?`, sid).Scan(&nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		s.fail(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if err != nil {
		s.internal(w, r, fmt.Errorf("load share: %w", err))
		return
	}
	if _, ok := s.authz(w, r, u.principal(), nodeID, access.Owner); !ok {
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM shares WHERE id = ? AND node_id = ?`, sid, nodeID); err != nil {
		s.internal(w, r, fmt.Errorf("delete share: %w", err))
		return
	}
	s.json(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- public links (owner only to manage) ----

const pbkdf2Iterations = 600_000

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations, hex.EncodeToString(salt), hex.EncodeToString(key)), nil
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	salt, err2 := hex.DecodeString(parts[2])
	want, err3 := hex.DecodeString(parts[3])
	if err != nil || err2 != nil || err3 != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && hmac.Equal(got, want)
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type linkJSON struct {
	ID          int64  `json:"id"`
	CreatedAt   int64  `json:"created_at"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
	HasPassword bool   `json:"has_password"`
	Expired     bool   `json:"expired,omitempty"`
	URL         string `json:"url,omitempty"` // only in the creation response
}

func (s *Server) handleListLinks(w http.ResponseWriter, r *http.Request, u *User) {
	res, ok := s.authz(w, r, u.principal(), r.PathValue("id"), access.Owner)
	if !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, created_at, COALESCE(expires_at, 0), password_hash IS NOT NULL FROM links WHERE node_id = ? ORDER BY id`, res.Node.ID)
	if err != nil {
		s.internal(w, r, fmt.Errorf("list links: %w", err))
		return
	}
	defer rows.Close()
	out := []linkJSON{}
	now := s.now()
	for rows.Next() {
		var l linkJSON
		if err := rows.Scan(&l.ID, &l.CreatedAt, &l.ExpiresAt, &l.HasPassword); err != nil {
			s.internal(w, r, fmt.Errorf("list links: %w", err))
			return
		}
		l.Expired = l.ExpiresAt != 0 && l.ExpiresAt <= now
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		s.internal(w, r, fmt.Errorf("list links: %w", err))
		return
	}
	s.json(w, http.StatusOK, map[string]any{"links": out})
}

// handleCreateLink creates a read-only public link. The token (256 random
// bits) is returned once; only its hash is stored.
func (s *Server) handleCreateLink(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		ExpiresInDays *int   `json:"expires_in_days"`
		ExpiresAt     *int64 `json:"expires_at"`
		Password      string `json:"password"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	var expires sql.NullInt64
	switch {
	case in.ExpiresAt != nil:
		expires = sql.NullInt64{Int64: *in.ExpiresAt, Valid: true}
	case in.ExpiresInDays != nil:
		if *in.ExpiresInDays < 1 || *in.ExpiresInDays > 3650 {
			s.fail(w, http.StatusBadRequest, "bad_expiry", "expires_in_days must be between 1 and 3650")
			return
		}
		expires = sql.NullInt64{Int64: s.Now().Add(time.Duration(*in.ExpiresInDays) * 24 * time.Hour).Unix(), Valid: true}
	}
	if len(in.Password) > 200 {
		s.fail(w, http.StatusBadRequest, "bad_password", "password is too long")
		return
	}
	res, ok := s.authzShareable(w, r, u, r.PathValue("id"))
	if !ok {
		return
	}
	var pwHash sql.NullString
	if in.Password != "" {
		h, err := hashPassword(in.Password)
		if err != nil {
			s.internal(w, r, fmt.Errorf("hash password: %w", err))
			return
		}
		pwHash = sql.NullString{String: h, Valid: true}
	}
	token := randomToken()
	now := s.now()
	result, err := s.db.ExecContext(r.Context(), `INSERT INTO links(node_id, token_hash, password_hash, expires_at, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, res.Node.ID, tokenHash(token), pwHash, expires, u.ID, now)
	if err != nil {
		s.internal(w, r, fmt.Errorf("create link: %w", err))
		return
	}
	id, _ := result.LastInsertId()
	s.log.Info("link created", "node", res.Node.ID, "by", u.Email, "link", id)
	s.json(w, http.StatusCreated, map[string]any{"link": linkJSON{
		ID: id, CreatedAt: now, ExpiresAt: expires.Int64, HasPassword: pwHash.Valid, URL: s.cfg.BaseURL + "/s/" + token,
	}})
}

func (s *Server) handleDeleteLink(w http.ResponseWriter, r *http.Request, u *User) {
	lid, _ := strconv.ParseInt(r.PathValue("lid"), 10, 64)
	var nodeID string
	err := s.db.QueryRowContext(r.Context(), `SELECT node_id FROM links WHERE id = ?`, lid).Scan(&nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		s.fail(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if err != nil {
		s.internal(w, r, fmt.Errorf("load link: %w", err))
		return
	}
	if _, ok := s.authz(w, r, u.principal(), nodeID, access.Owner); !ok {
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM links WHERE id = ? AND node_id = ?`, lid, nodeID); err != nil {
		s.internal(w, r, fmt.Errorf("delete link: %w", err))
		return
	}
	s.json(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- public access through a link (no session) ----

type link struct {
	ID           int64
	NodeID       string
	PasswordHash string
}

// limiter is a tiny fixed-window counter for password attempts per link.
type limiter struct {
	mu sync.Mutex
	m  map[int64]*window
}

type window struct {
	start time.Time
	count int
}

func (l *limiter) allow(key int64, now time.Time, max int, per time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[int64]*window{}
	}
	w := l.m[key]
	if w == nil || now.Sub(w.start) > per {
		if len(l.m) > 10000 {
			l.m = map[int64]*window{}
		}
		w = &window{start: now}
		l.m[key] = w
	}
	w.count++
	return w.count <= max
}

func unlockCookieName(id int64) string { return "drive_link_" + strconv.FormatInt(id, 10) }

// unlockValue is what the unlock cookie must contain. It depends on the
// password hash, so changing or revoking the link invalidates it.
func (s *Server) unlockValue(l *link) string {
	return s.mac("link-unlock", strconv.FormatInt(l.ID, 10)+"|"+l.PasswordHash)
}

// findLink looks up a live link by token. Unknown, revoked and expired links
// are indistinguishable to the caller.
func (s *Server) findLink(r *http.Request) (*link, error) {
	token := r.PathValue("token")
	if len(token) < 22 || len(token) > 128 {
		return nil, sql.ErrNoRows
	}
	l := &link{}
	var pw sql.NullString
	err := s.db.QueryRowContext(r.Context(), `SELECT id, node_id, password_hash FROM links
		WHERE token_hash = ? AND (expires_at IS NULL OR expires_at > ?)`, tokenHash(token), s.now()).Scan(&l.ID, &l.NodeID, &pw)
	if err != nil {
		return nil, err
	}
	l.PasswordHash = pw.String
	return l, nil
}

func (s *Server) linkUnlocked(r *http.Request, l *link) bool {
	if l.PasswordHash == "" {
		return true
	}
	c, err := r.Cookie(unlockCookieName(l.ID))
	return err == nil && hmac.Equal([]byte(c.Value), []byte(s.unlockValue(l)))
}

// linkPrincipal turns the request's link token into an access.Principal
// scoped to the linked node. Everything a visitor does is then authorized by
// access.Resolve like any other request.
func (s *Server) linkPrincipal(w http.ResponseWriter, r *http.Request) (access.Principal, bool) {
	l, err := s.findLink(r)
	if errors.Is(err, sql.ErrNoRows) {
		s.fail(w, http.StatusNotFound, "not_found", "this link does not exist or has expired")
		return access.Principal{}, false
	}
	if err != nil {
		s.internal(w, r, fmt.Errorf("load link: %w", err))
		return access.Principal{}, false
	}
	if !s.linkUnlocked(r, l) {
		s.fail(w, http.StatusUnauthorized, "password_required", "this link needs a password")
		return access.Principal{}, false
	}
	return access.Principal{LinkNodeID: l.NodeID}, true
}

func (s *Server) handlePublicInfo(w http.ResponseWriter, r *http.Request) {
	l, err := s.findLink(r)
	if errors.Is(err, sql.ErrNoRows) {
		s.fail(w, http.StatusNotFound, "not_found", "this link does not exist or has expired")
		return
	}
	if err != nil {
		s.internal(w, r, fmt.Errorf("load link: %w", err))
		return
	}
	if !s.linkUnlocked(r, l) {
		// Nothing about the content is revealed before the password.
		s.json(w, http.StatusOK, map[string]any{"needs_password": true})
		return
	}
	res, ok := s.authz(w, r, access.Principal{LinkNodeID: l.NodeID}, l.NodeID, access.Viewer)
	if !ok {
		return
	}
	s.json(w, http.StatusOK, map[string]any{"needs_password": false, "node": toJSON(res.Node)})
}

func (s *Server) handlePublicUnlock(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	l, err := s.findLink(r)
	if errors.Is(err, sql.ErrNoRows) {
		s.fail(w, http.StatusNotFound, "not_found", "this link does not exist or has expired")
		return
	}
	if err != nil {
		s.internal(w, r, fmt.Errorf("load link: %w", err))
		return
	}
	if l.PasswordHash == "" {
		s.json(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if !s.unlock.allow(l.ID, s.Now(), 10, time.Minute) {
		s.fail(w, http.StatusTooManyRequests, "too_many_attempts", "too many attempts; wait a minute and try again")
		return
	}
	if len(in.Password) > 200 || !checkPassword(l.PasswordHash, in.Password) {
		s.fail(w, http.StatusForbidden, "wrong_password", "wrong password")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: unlockCookieName(l.ID), Value: s.unlockValue(l), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.cfg.Secure(), MaxAge: 12 * 3600,
	})
	s.json(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePublicNode(w http.ResponseWriter, r *http.Request) {
	p, ok := s.linkPrincipal(w, r)
	if !ok {
		return
	}
	res, ok := s.authz(w, r, p, r.PathValue("id"), access.Viewer)
	if !ok {
		return
	}
	s.describe(w, r, res)
}

func (s *Server) handlePublicContent(w http.ResponseWriter, r *http.Request) {
	p, ok := s.linkPrincipal(w, r)
	if !ok {
		return
	}
	res, ok := s.authz(w, r, p, r.PathValue("id"), access.Viewer)
	if !ok {
		return
	}
	s.serveFile(w, r, res.Node)
}

func (s *Server) handlePublicThumb(w http.ResponseWriter, r *http.Request) {
	p, ok := s.linkPrincipal(w, r)
	if !ok {
		return
	}
	res, ok := s.authz(w, r, p, r.PathValue("id"), access.Viewer)
	if !ok {
		return
	}
	s.serveThumb(w, r, res.Node)
}

func (s *Server) handlePublicZip(w http.ResponseWriter, r *http.Request) {
	p, ok := s.linkPrincipal(w, r)
	if !ok {
		return
	}
	res, ok := s.authz(w, r, p, r.PathValue("id"), access.Viewer)
	if !ok {
		return
	}
	s.serveZip(w, r, res.Node)
}
