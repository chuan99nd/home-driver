package config

import (
	"strings"
	"testing"
)

func env(over map[string]string) func(string) (string, bool) {
	base := map[string]string{
		"DRIVE_BASE_URL":             "https://drive.example.com",
		"DRIVE_GOOGLE_CLIENT_ID":     "id",
		"DRIVE_GOOGLE_CLIENT_SECRET": "secret",
		"DRIVE_ADMIN_EMAILS":         " Owner@Example.com , second@example.com,",
		"DRIVE_SESSION_SECRET":       strings.Repeat("x", 32),
	}
	for k, v := range over {
		if v == "<unset>" {
			delete(base, k)
		} else {
			base[k] = v
		}
	}
	return func(k string) (string, bool) { v, ok := base[k]; return v, ok }
}

func TestDefaults(t *testing.T) {
	c, err := FromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1:8090" || c.CuratorURL != "http://127.0.0.1:8091" || c.TrashDays != 30 || c.DevLogin ||
		c.DBPath != "/var/lib/home-driver/drive/drive.db" || c.DataDir != "/srv/home-driver/data/drive" {
		t.Errorf("defaults: %+v", c)
	}
	if len(c.AdminEmails) != 2 || !c.IsAdmin("OWNER@example.com") || !c.IsAdmin(" second@example.com") || c.IsAdmin("x@example.com") || c.IsAdmin("") {
		t.Errorf("admins: %v", c.AdminEmails)
	}
	if !c.Secure() {
		t.Error("https base must be Secure")
	}
	if c.ChunkSize > MaxChunkSize || c.ChunkSize > 32<<20 {
		t.Errorf("chunk size %d over the Cloudflare-safe limit", c.ChunkSize)
	}
	// Explicitly empty CURATOR_URL disables the proxy.
	c, err = FromEnv(env(map[string]string{"CURATOR_URL": ""}))
	if err != nil || c.CuratorURL != "" {
		t.Errorf("empty CURATOR_URL: %q %v", c.CuratorURL, err)
	}
	c, _ = FromEnv(env(map[string]string{"DRIVE_BASE_URL": "https://drive.example.com/"}))
	if c.BaseURL != "https://drive.example.com" {
		t.Errorf("trailing slash kept: %q", c.BaseURL)
	}
}

func TestInvalidConfig(t *testing.T) {
	cases := map[string]map[string]string{
		"no base url":        {"DRIVE_BASE_URL": "<unset>"},
		"base url with path": {"DRIVE_BASE_URL": "https://example.com/drive"},
		"base url scheme":    {"DRIVE_BASE_URL": "ftp://example.com"},
		"short secret":       {"DRIVE_SESSION_SECRET": "short"},
		"no admins":          {"DRIVE_ADMIN_EMAILS": " , "},
		"bad admin":          {"DRIVE_ADMIN_EMAILS": "not-an-email"},
		"no google":          {"DRIVE_GOOGLE_CLIENT_ID": "<unset>"},
		"half google":        {"DRIVE_GOOGLE_CLIENT_SECRET": "<unset>"},
		"bad addr":           {"DRIVE_ADDR": "8090"},
		"bad trash days":     {"DRIVE_TRASH_DAYS": "-1"},
		"bad trash days 2":   {"DRIVE_TRASH_DAYS": "soon"},
		"bad curator":        {"CURATOR_URL": "localhost:8091"},
		"bad dev login":      {"DRIVE_DEV_LOGIN": "maybe"},
	}
	for name, over := range cases {
		if _, err := FromEnv(env(over)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// DRIVE_DEV_LOGIN must be impossible to enable on anything reachable from
// outside the machine.
func TestDevLoginRefusedUnlessLoopback(t *testing.T) {
	ok := []map[string]string{
		{"DRIVE_ADDR": "127.0.0.1:8090", "DRIVE_BASE_URL": "http://localhost:8090"},
		{"DRIVE_ADDR": "localhost:8090", "DRIVE_BASE_URL": "http://127.0.0.1:8090"},
		{"DRIVE_ADDR": "[::1]:8090", "DRIVE_BASE_URL": "http://localhost:8090"},
		// Google credentials are optional with dev login.
		{"DRIVE_ADDR": "127.0.0.1:8090", "DRIVE_BASE_URL": "http://localhost:8090", "DRIVE_GOOGLE_CLIENT_ID": "<unset>", "DRIVE_GOOGLE_CLIENT_SECRET": "<unset>"},
	}
	for _, over := range ok {
		over["DRIVE_DEV_LOGIN"] = "1"
		if c, err := FromEnv(env(over)); err != nil || !c.DevLogin {
			t.Errorf("%v: refused: %v", over, err)
		}
	}
	bad := []map[string]string{
		{"DRIVE_ADDR": "0.0.0.0:8090", "DRIVE_BASE_URL": "http://localhost:8090"},
		{"DRIVE_ADDR": ":8090", "DRIVE_BASE_URL": "http://localhost:8090"},
		{"DRIVE_ADDR": "192.168.1.10:8090", "DRIVE_BASE_URL": "http://localhost:8090"},
		{"DRIVE_ADDR": "127.0.0.1:8090", "DRIVE_BASE_URL": "https://drive.example.com"},
		{"DRIVE_ADDR": "127.0.0.1:8090", "DRIVE_BASE_URL": "http://drive.example.com"},
		{"DRIVE_ADDR": "127.0.0.1:8090", "DRIVE_BASE_URL": "https://localhost:8090"},
		{"DRIVE_ADDR": "127.0.0.1:8090", "DRIVE_BASE_URL": "http://localhost.evil.example"},
		{"DRIVE_ADDR": "127.0.0.1:8090", "DRIVE_BASE_URL": "http://192.168.1.10:8090"},
		{"DRIVE_ADDR": "127.0.0.1:8090"}, // default-style public base URL
	}
	for _, over := range bad {
		over["DRIVE_DEV_LOGIN"] = "1"
		_, err := FromEnv(env(over))
		if err == nil || !strings.Contains(err.Error(), "DRIVE_DEV_LOGIN=1 refused") {
			t.Errorf("%v: not refused (err = %v)", over, err)
		}
	}
}
