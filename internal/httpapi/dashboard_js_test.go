package httpapi_test

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/OzanAlpkaya/pulsego/web"
)

// readDashboardFile returns one embedded dashboard file as text.
func readDashboardFile(t *testing.T, name string) string {
	t.Helper()

	data, err := fs.ReadFile(web.Static(), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// TestDashboardScriptInsertsTextOnly guards the XSS rule: strings from the API
// reach the DOM through textContent, never through an HTML parser.
func TestDashboardScriptInsertsTextOnly(t *testing.T) {
	t.Parallel()

	js := readDashboardFile(t, "app.js")

	forbidden := map[string]*regexp.Regexp{
		"innerHTML":            regexp.MustCompile(`\binnerHTML\b`),
		"outerHTML":            regexp.MustCompile(`\bouterHTML\b`),
		"insertAdjacentHTML":   regexp.MustCompile(`\binsertAdjacentHTML\b`),
		"document.write":       regexp.MustCompile(`\bdocument\.write`),
		"eval":                 regexp.MustCompile(`\beval\s*\(`),
		"new Function":         regexp.MustCompile(`\bnew\s+Function\s*\(`),
		"createContextualFrag": regexp.MustCompile(`createContextualFragment`),
		"DOMParser":            regexp.MustCompile(`\bDOMParser\b`),
		"inline style write":   regexp.MustCompile(`\.style\.cssText|setAttribute\(\s*["']style["']`),
	}
	for name, re := range forbidden {
		if m := re.FindString(js); m != "" {
			t.Errorf("app.js uses %s (%q)", name, m)
		}
	}
	if !strings.Contains(js, ".textContent") {
		t.Error("app.js never uses textContent")
	}
}

// TestDashboardScriptKeepsTheKeyOutOfURLs guards the API key handling: it is
// stored only in sessionStorage and sent only in the Authorization header.
func TestDashboardScriptKeepsTheKeyOutOfURLs(t *testing.T) {
	t.Parallel()

	js := readDashboardFile(t, "app.js")

	if regexp.MustCompile(`\blocalStorage\b`).MatchString(js) {
		t.Error("app.js uses localStorage; the key must live in sessionStorage only")
	}
	if regexp.MustCompile(`document\.cookie`).MatchString(js) {
		t.Error("app.js writes cookies; the key must live in sessionStorage only")
	}
	if !strings.Contains(js, "sessionStorage.setItem(") {
		t.Error("app.js does not store the key in sessionStorage")
	}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?i)[?&](api_?key|key|token)=`),
		regexp.MustCompile(`\b(location|history)\b[^;\n]*getKey\(`),
		regexp.MustCompile(`URLSearchParams`),
	} {
		if m := re.FindString(js); m != "" {
			t.Errorf("app.js may put the key in a URL: %q", m)
		}
	}

	// Every read of the key must be the Authorization header, or a
	// presence check.
	for i, line := range strings.Split(js, "\n") {
		if !strings.Contains(line, "getKey()") || strings.Contains(line, "function getKey") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.Contains(line, `Authorization: "Bearer " + getKey()`) || strings.HasPrefix(trimmed, "if (getKey())") {
			continue
		}
		t.Errorf("app.js:%d uses the key outside the Authorization header: %s", i+1, trimmed)
	}
}

// TestDashboardStylesheetHasNoRemoteImports guards default-src 'self' for CSS.
func TestDashboardStylesheetHasNoRemoteImports(t *testing.T) {
	t.Parallel()

	css := readDashboardFile(t, "style.css")
	if m := regexp.MustCompile(`(?i)(@import|url\(\s*["']?(https?:)?//)`).FindString(css); m != "" {
		t.Errorf("style.css loads a remote resource: %q", m)
	}
}
