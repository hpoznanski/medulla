package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/hpoznanski/medulla/internal/config"
)

// The README promises no JavaScript under a default-src 'none' CSP. Hold
// every template to it: no script element, no inline event handler, no
// javascript: URL, no inline style (style-src 'self' would block it anyway,
// silently breaking the page).
func TestTemplatesCarryNoScript(t *testing.T) {
	banned := regexp.MustCompile(`(?i)<script|\son[a-z]+\s*=|javascript:|\sstyle\s*=`)
	err := fs.WalkDir(assets, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(assets, path)
		if err != nil {
			return err
		}
		if m := banned.Find(b); m != nil {
			t.Errorf("%s contains %q", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCSPUnchanged(t *testing.T) {
	s, _ := testServer(t, "http://127.0.0.1:1")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	want := "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'"
	if got := rec.Header().Get("Content-Security-Policy"); got != want {
		t.Errorf("CSP = %q, want %q", got, want)
	}
}

const xss = `"><script>alert(1)</script><img src=x onerror=alert(1)>`

// hostile is a query that throws an injection attempt at every URL
// parameter the list pages read.
var hostile = url.Values{
	"q":       {xss},
	"sort":    {xss},
	"dir":     {xss},
	"refresh": {"0;url=javascript:alert(1)"},
	"health":  {xss},
	"layout":  {xss},
}.Encode()

var hrefRE = regexp.MustCompile(`(?:href|action)="([^"]*)"`)

// Every link the list pages emit must stay on this origin: a path or a
// same-page query, never a scheme or a protocol-relative //host.
func assertSameOriginLinks(t *testing.T, page, body string) {
	t.Helper()
	for _, m := range hrefRE.FindAllStringSubmatch(body, -1) {
		link := m[1]
		sameOrigin := strings.HasPrefix(link, "?") ||
			(strings.HasPrefix(link, "/") && !strings.HasPrefix(link, "//"))
		if !sameOrigin {
			t.Errorf("%s: off-origin link %q", page, link)
		}
	}
}

func TestListPagesRejectInjection(t *testing.T) {
	s, admin, _ := homeServer(t)
	esrv := fakeES(t)
	so, codec := testServer(t, esrv.URL)
	root := sessionCookieFor(t, codec, "root", "admin")

	for _, tc := range []struct {
		name string
		srv  http.Handler
		c    *http.Cookie
		path string
	}{
		{"landing", s, admin, "/?" + hostile},
		{"landing list", s, admin, "/?layout=list&" + hostile},
		{"overview", so, root, "/c/dev/overview?" + hostile},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.AddCookie(tc.c)
		rec := httptest.NewRecorder()
		tc.srv.ServeHTTP(rec, req)
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.name, rec.Code)
		}
		// Escaped payload text is fine; a live tag is not.
		for _, bad := range []string{"<script>alert", "<img src=x", "javascript:"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s: %q reflected unescaped", tc.name, bad)
			}
		}
		if strings.Contains(body, `http-equiv="refresh"`) {
			t.Errorf("%s: invalid refresh value produced a meta refresh", tc.name)
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == layoutCookie && c.Value != layoutCards && c.Value != layoutList {
				t.Errorf("%s: invalid layout stored in cookie: %q", tc.name, c.Value)
			}
		}
		assertSameOriginLinks(t, tc.name, body)
	}
}

// A tampered preference cookie must be ignored, not rendered or trusted.
func TestLayoutCookieTamperIgnored(t *testing.T) {
	s, admin, _ := homeServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(admin)
	req.AddCookie(&http.Cookie{Name: layoutCookie, Value: "list<script>"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "<script>") || !strings.Contains(body, `class="clusters-grid"`) {
		t.Error("tampered layout cookie not ignored")
	}
}

func TestLayoutCookieFlags(t *testing.T) {
	s, admin, _ := homeServer(t)
	rec, _ := get(t, s, admin, "/?layout=list")
	for _, c := range rec.Result().Cookies() {
		if c.Name != layoutCookie {
			continue
		}
		if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
			t.Errorf("layout cookie flags: %+v", c)
		}
		return
	}
	t.Fatal("no layout cookie set")
}

// Cluster names a role cannot view must not leak through the landing page
// in any form — card, count, or the filter's datalist suggestions.
func TestLandingLeaksNoHiddenClusters(t *testing.T) {
	s, _, greenOnly := homeServer(t)
	for _, path := range []string{"/", "/?layout=list", "/?q=dead", "/?health=unreachable"} {
		_, body := get(t, s, greenOnly, path)
		for _, hidden := range []string{"dead", "partial"} {
			if strings.Contains(body, hidden) && !strings.Contains(path, hidden) {
				t.Errorf("%s: hidden cluster %q visible", path, hidden)
			}
			if strings.Contains(body, `"/c/`+hidden) || strings.Contains(body, `value="`+hidden+`">`) {
				t.Errorf("%s: hidden cluster %q linked or suggested", path, hidden)
			}
		}
		if !strings.Contains(body, "<b>1</b> clusters") {
			t.Errorf("%s: total counts clusters outside the role", path)
		}
	}
}

// An unreachable cluster's raw error is shown to every viewer, so it must
// never carry the ES credentials Medulla holds.
func TestUnreachableErrorRedactsCredentials(t *testing.T) {
	s, codec := testServerClusters(t, []config.Cluster{
		{Name: "green", URL: "http://medulla:s3cr3t-pw@127.0.0.1:1"},
	})
	for _, path := range []string{"/", "/?layout=list"} {
		_, body := get(t, s, sessionCookieFor(t, codec, "root", "admin"), path)
		if !strings.Contains(body, "refused") {
			t.Fatalf("%s: cluster not reported unreachable", path)
		}
		if strings.Contains(body, "s3cr3t-pw") {
			t.Errorf("%s: ES password leaked in error text", path)
		}
	}
}

// The landing page is behind a session like every other page.
func TestLandingRequiresSession(t *testing.T) {
	s, _, _ := homeServer(t)
	for _, path := range []string{"/", "/?layout=list", "/?health=red"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("%s: status=%d location=%q", path, rec.Code, rec.Header().Get("Location"))
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Errorf("%s: cookie set for an anonymous request", path)
		}
	}
}
