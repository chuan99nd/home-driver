package server

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every string id used by the UI must exist in both dictionaries, and the two
// dictionaries must have the same keys (docs/DESIGN.md: Vietnamese + English).
func TestI18nCoverage(t *testing.T) {
	read := func(name string) string {
		b, err := webFS.ReadFile("web/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	src := read("i18n.js")
	viStart, enStart, end := strings.Index(src, "const vi = {"), strings.Index(src, "const en = {"), strings.Index(src, "const DICTS")
	if viStart < 0 || enStart < viStart || end < enStart {
		t.Fatal("cannot locate dictionaries in i18n.js")
	}
	keyRe := regexp.MustCompile(`(?m)^\s*'([a-z0-9_.]+)':`)
	keys := func(block string) map[string]bool {
		m := map[string]bool{}
		for _, k := range keyRe.FindAllStringSubmatch(block, -1) {
			m[k[1]] = true
		}
		return m
	}
	vi, en := keys(src[viStart:enStart]), keys(src[enStart:end])
	if len(vi) < 100 {
		t.Fatalf("only %d Vietnamese keys parsed", len(vi))
	}
	for k := range en {
		// ".one" forms are optional per language (Vietnamese has no plural).
		if !vi[k] && !strings.HasSuffix(k, ".one") {
			t.Errorf("key %q missing in vi", k)
		}
	}
	for k := range vi {
		if !en[k] && !strings.HasSuffix(k, ".one") {
			t.Errorf("key %q missing in en", k)
		}
	}

	used := map[string]bool{}
	code := read("app.js") + read("ui.js") + read("public.js")
	for _, m := range regexp.MustCompile(`\btn?\('([a-z0-9_.]+)'`).FindAllStringSubmatch(code, -1) {
		used[m[1]] = true
	}
	// Keys passed indirectly (tables of [value, key]) are quoted literals too.
	for _, m := range regexp.MustCompile(`'((?:nav|theme|sort|link|view|upload|err|sync)\.[a-z0-9_.]+)'`).FindAllStringSubmatch(code, -1) {
		used[m[1]] = true
	}
	for _, page := range []string{"index.html", "public.html"} {
		for _, m := range regexp.MustCompile(`data-i18n(?:-label|-placeholder)?="([a-z0-9_.]+)"`).FindAllStringSubmatch(read(page), -1) {
			used[m[1]] = true
		}
	}
	for _, code := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(
		code[strings.Index(code, "const known = ["):strings.Index(code, "if (e instanceof ApiError && known")], -1) {
		used["err."+code[1]] = true
	}
	var missing []string
	for k := range used {
		if k == "err." || strings.HasSuffix(k, ".") {
			continue
		}
		if !en[k] || !vi[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("string ids used by the UI but missing from a dictionary: %v", missing)
	}
	// The UI code never builds HTML from strings.
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		if strings.Contains(code, bad+" =") || strings.Contains(code, bad+"(") || strings.Contains(code, "."+bad+"=") {
			t.Errorf("UI code uses %s", bad)
		}
	}
}
