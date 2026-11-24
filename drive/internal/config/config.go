// Package config loads and validates the Drive configuration from environment
// variables. Validation is strict: the server refuses to start on bad config.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// MaxChunkSize is the hard upper bound for one upload chunk. Cloudflare's free
// plan rejects request bodies over ~100 MB; 32 MiB leaves a wide margin.
const MaxChunkSize = 32 << 20

// DefaultChunkSize is the chunk size the server tells clients to use.
const DefaultChunkSize = 16 << 20

// Config is the validated runtime configuration.
type Config struct {
	Addr               string
	DBPath             string
	DataDir            string
	BaseURL            string // no trailing slash
	GoogleClientID     string
	GoogleClientSecret string
	AdminEmails        []string // lower-cased
	SessionSecret      string
	CuratorURL         string // empty disables the proxy
	TrashDays          int    // 0 disables the automatic purge
	DevLogin           bool

	// ChunkSize is not configurable by env; tests lower it.
	ChunkSize int64
	// OIDCIssuer is not configurable by env; it is always Google.
	OIDCIssuer string
}

// NormalizeEmail lower-cases and trims an email address. All email comparisons
// in the application go through this.
func NormalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// IsAdmin reports whether email is one of DRIVE_ADMIN_EMAILS.
func (c Config) IsAdmin(email string) bool {
	email = NormalizeEmail(email)
	for _, a := range c.AdminEmails {
		if a == email {
			return true
		}
	}
	return false
}

// Secure reports whether cookies must carry the Secure attribute.
func (c Config) Secure() bool {
	return strings.HasPrefix(c.BaseURL, "https://")
}

// FromEnv builds a Config using lookup (os.LookupEnv in production).
func FromEnv(lookup func(string) (string, bool)) (Config, error) {
	getenv := func(key string) string {
		v, _ := lookup(key)
		return v
	}
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	c := Config{
		Addr:               get("DRIVE_ADDR", "127.0.0.1:8090"),
		DBPath:             get("DRIVE_DB_PATH", "/var/lib/home-driver/drive/drive.db"),
		DataDir:            get("DRIVE_DATA_DIR", "/srv/home-driver/data/drive"),
		BaseURL:            strings.TrimRight(get("DRIVE_BASE_URL", ""), "/"),
		GoogleClientID:     get("DRIVE_GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: get("DRIVE_GOOGLE_CLIENT_SECRET", ""),
		SessionSecret:      getenv("DRIVE_SESSION_SECRET"),
		TrashDays:          30,
		ChunkSize:          DefaultChunkSize,
		OIDCIssuer:         "https://accounts.google.com",
	}
	// CURATOR_URL: unset means the default; set but empty disables the proxy.
	if cu, ok := lookup("CURATOR_URL"); ok {
		c.CuratorURL = strings.TrimRight(strings.TrimSpace(cu), "/")
	} else {
		c.CuratorURL = "http://127.0.0.1:8091"
	}
	for _, e := range strings.Split(getenv("DRIVE_ADMIN_EMAILS"), ",") {
		if e = NormalizeEmail(e); e != "" {
			c.AdminEmails = append(c.AdminEmails, e)
		}
	}
	if v := strings.TrimSpace(getenv("DRIVE_TRASH_DAYS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("DRIVE_TRASH_DAYS must be a non-negative integer, got %q", v)
		}
		c.TrashDays = n
	}
	switch strings.ToLower(strings.TrimSpace(getenv("DRIVE_DEV_LOGIN"))) {
	case "", "0", "false", "no":
	case "1", "true", "yes":
		c.DevLogin = true
	default:
		return c, fmt.Errorf("DRIVE_DEV_LOGIN must be 0 or 1")
	}
	return c, c.Validate()
}

// Validate checks every field and returns one error listing all problems.
func (c Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	host, _, err := net.SplitHostPort(c.Addr)
	if err != nil {
		bad("DRIVE_ADDR %q is not host:port: %v", c.Addr, err)
	}
	if c.DBPath == "" {
		bad("DRIVE_DB_PATH is required")
	}
	if c.DataDir == "" {
		bad("DRIVE_DATA_DIR is required")
	}
	var base *url.URL
	if c.BaseURL == "" {
		bad("DRIVE_BASE_URL is required (e.g. https://drive.example.com)")
	} else {
		base, err = url.Parse(c.BaseURL)
		if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" ||
			base.Path != "" || base.RawQuery != "" || base.Fragment != "" || base.User != nil {
			bad("DRIVE_BASE_URL %q must be http(s)://host[:port] with no path", c.BaseURL)
			base = nil
		}
	}
	if len(c.SessionSecret) < 32 {
		bad("DRIVE_SESSION_SECRET is required and must be at least 32 characters (try: openssl rand -hex 32)")
	}
	if len(c.AdminEmails) == 0 {
		bad("DRIVE_ADMIN_EMAILS is required (comma separated); without an admin nobody can sign in")
	}
	for _, e := range c.AdminEmails {
		if !strings.Contains(e, "@") {
			bad("DRIVE_ADMIN_EMAILS entry %q is not an email address", e)
		}
	}
	if !c.DevLogin && (c.GoogleClientID == "" || c.GoogleClientSecret == "") {
		bad("DRIVE_GOOGLE_CLIENT_ID and DRIVE_GOOGLE_CLIENT_SECRET are required")
	}
	if (c.GoogleClientID == "") != (c.GoogleClientSecret == "") {
		bad("DRIVE_GOOGLE_CLIENT_ID and DRIVE_GOOGLE_CLIENT_SECRET must be set together")
	}
	if c.CuratorURL != "" {
		u, err := url.Parse(c.CuratorURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("CURATOR_URL %q must be an http(s) URL (set it to empty to disable the proxy)", c.CuratorURL)
		}
	}
	if c.ChunkSize <= 0 || c.ChunkSize > MaxChunkSize {
		bad("chunk size %d out of range (1..%d)", c.ChunkSize, MaxChunkSize)
	}
	if c.DevLogin {
		// Dev login lets anyone become any allowed user. It must be impossible
		// to enable it on something reachable from outside this machine.
		if !isLoopbackHost(host) {
			bad("DRIVE_DEV_LOGIN=1 refused: DRIVE_ADDR %q is not a loopback address", c.Addr)
		}
		if base == nil || base.Scheme != "http" || (base.Hostname() != "localhost" && base.Hostname() != "127.0.0.1") {
			bad("DRIVE_DEV_LOGIN=1 refused: DRIVE_BASE_URL %q is not http://localhost or http://127.0.0.1", c.BaseURL)
		}
	}
	return errors.Join(errs...)
}

// IsLoopbackAddr reports whether the host part of addr is a loopback address.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
