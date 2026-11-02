package webui

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, r)
	return w
}

func TestServesTheShell(t *testing.T) {
	w := get(t, "/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), "/assets/app.js") {
		t.Error("the shell does not load the application")
	}
}

func TestRoutesFallBackToTheShell(t *testing.T) {
	// Otherwise reloading on /agents is a 404.
	for _, path := range []string{"/agents", "/agents/agent_1/logs", "/board", "/tasks/task_1", "/anything"} {
		w := get(t, path, nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, w.Code)
		}
		if !strings.Contains(w.Body.String(), "<title>Keera Agents</title>") {
			t.Errorf("%s: did not serve the shell", path)
		}
	}
}

func TestMissingAssetsAre404(t *testing.T) {
	// A missing asset must not return HTML, which the browser would parse as
	// JavaScript.
	w := get(t, "/assets/nope.js", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestAssetsAreServedWithTheRightType(t *testing.T) {
	tests := map[string]string{
		"/assets/app.js":                    "text/javascript",
		"/assets/app.css":                   "text/css",
		"/assets/ui.js":                     "text/javascript",
		"/assets/favicon.svg":               "image/svg+xml",
		"/assets/fonts/space-grotesk.woff2": "font/woff2",
	}
	for path, want := range tests {
		w := get(t, path, nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d", path, w.Code)
			continue
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, want) {
			t.Errorf("%s: Content-Type = %q, want %q", path, ct, want)
		}
	}
}

// relativeImport matches the panel's own imports. There are no bare
// specifiers: with no build step, nothing could resolve them.
var relativeImport = regexp.MustCompile(`from\s+"(\.[^"]+)"`)

func TestEveryImportIsEmbedded(t *testing.T) {
	// A module missing from the binary is a blank page at run time and no
	// error at build time. The list comes from the imports, so new views are
	// covered too.
	roots := []string{"assets/app.js"}
	if err := fs.WalkDir(assets, "assets/views", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".js") {
			roots = append(roots, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(roots) < 2 {
		t.Fatal("no views are embedded at all")
	}

	for _, src := range roots {
		body, err := fs.ReadFile(assets, src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		for _, m := range relativeImport.FindAllStringSubmatch(string(body), -1) {
			target := path.Join(path.Dir(src), m[1])
			url := "/" + target
			if w := get(t, url, nil); w.Code != http.StatusOK {
				t.Errorf("%s imports %s, which is missing from the binary (status %d)",
					src, m[1], w.Code)
			}
		}
	}
}

func TestConditionalRequestsAvoidResending(t *testing.T) {
	first := get(t, "/assets/app.css", nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag, so every reload refetches the whole panel")
	}
	second := get(t, "/assets/app.css", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Error("a 304 must carry no body")
	}
}

func TestSecurityHeaders(t *testing.T) {
	w := get(t, "/", nil)
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"script-src 'self'",      // no inline or third-party scripts
		"frame-ancestors 'none'", // the panel is not embeddable
		"connect-src 'self'",
		"object-src 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP is missing %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "'unsafe-inline'") {
		t.Error("the CSP allows inline scripts or styles: an injected one would run or show")
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options")
	}
	if !strings.Contains(w.Header().Get("Cache-Control"), "no-cache") {
		t.Error("the panel must revalidate rather than be served from a stale cache")
	}
}

func TestOnlyReadMethods(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestPathTraversalCannotEscapeTheAssets(t *testing.T) {
	for _, path := range []string{"/assets/../../etc/passwd", "/assets/./app.js/../app.js"} {
		w := get(t, path, nil)
		if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "root:") {
			t.Fatalf("%s escaped the embedded assets", path)
		}
	}
}

func TestAssetsAreServedPrecompressed(t *testing.T) {
	plain := get(t, "/assets/app.css", nil)
	zipped := get(t, "/assets/app.css", map[string]string{"Accept-Encoding": "gzip"})

	if got := zipped.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("a client that asked for no encoding got Content-Encoding %q", got)
	}
	if zipped.Body.Len() >= plain.Body.Len() {
		t.Errorf("gzipped to %d bytes from %d, which is no saving",
			zipped.Body.Len(), plain.Body.Len())
	}
	body, err := io.ReadAll(mustGunzip(t, zipped.Body.Bytes()))
	if err != nil {
		t.Fatalf("reading the gzip body: %v", err)
	}
	if !bytes.Equal(body, plain.Body.Bytes()) {
		t.Error("the compressed asset does not decompress to the file itself")
	}
	if !strings.Contains(zipped.Header().Get("Vary"), "Accept-Encoding") {
		t.Error("Vary is missing, so a cache cannot tell the two bodies apart")
	}
}

func TestTheTwoBodiesCarryDifferentEntityTags(t *testing.T) {
	// One tag for both bodies lets a cache serve gzip to a client that cannot
	// read it.
	plain := get(t, "/assets/app.js", nil).Header().Get("ETag")
	zipped := get(t, "/assets/app.js", map[string]string{"Accept-Encoding": "gzip"}).Header().Get("ETag")
	if plain == "" || zipped == "" {
		t.Fatal("an asset was served without an ETag")
	}
	if plain == zipped {
		t.Errorf("both representations are tagged %s", plain)
	}
	// Each still revalidates against its own.
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"plain", map[string]string{"If-None-Match": plain}},
		{"gzip", map[string]string{"If-None-Match": zipped, "Accept-Encoding": "gzip"}},
	} {
		if w := get(t, "/assets/app.js", tc.headers); w.Code != http.StatusNotModified {
			t.Errorf("%s: status = %d, want 304", tc.name, w.Code)
		}
	}
}

func TestCompressingTheAssetsIsWorthIt(t *testing.T) {
	// app.js imports every view, so a cold load fetches all of this. The
	// saving is why both bodies are kept in memory.
	var raw, gz int
	for _, f := range files {
		raw += len(f.body)
		if f.gz != nil {
			gz += len(f.gz)
			continue
		}
		gz += len(f.body)
	}
	if gz*2 > raw {
		t.Errorf("the panel is %d bytes and gzips to %d, which is less than half the saving expected",
			raw, gz)
	}
	t.Logf("panel: %d bytes raw, %d gzipped", raw, gz)
}

func mustGunzip(t *testing.T, body []byte) io.Reader {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("response is not gzip: %v", err)
	}
	return zr
}

func TestAcceptsGzip(t *testing.T) {
	for name, c := range map[string]struct {
		header string
		want   bool
	}{
		"none":          {"", false},
		"gzip":          {"gzip", true},
		"among others":  {"br, gzip, deflate", true},
		"upper case":    {"GZIP", true},
		"weighted":      {"gzip;q=0.5", true},
		"refused":       {"gzip;q=0", false},
		"refused, long": {"gzip ; q=0.000", false},
		"only others":   {"br, deflate", false},
		"bad weight":    {"gzip;q=x", true},
	} {
		if got := acceptsGzip(c.header); got != c.want {
			t.Errorf("%s: acceptsGzip(%q) = %v, want %v", name, c.header, got, c.want)
		}
	}
}
