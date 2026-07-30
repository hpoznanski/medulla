package web

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/hpoznanski/medulla/internal/auth"
	"github.com/hpoznanski/medulla/internal/config"
	"github.com/hpoznanski/medulla/internal/es"
	"github.com/hpoznanski/medulla/internal/rbac"
)

// testHash pre-hashes test passwords at MinCost so auth.New doesn't burn
// DefaultCost bcrypt per test.
func testHash(t *testing.T, password string) config.Secret {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return config.Secret("bcrypt:" + string(h))
}

func testServer(t *testing.T, esURL string) (*Server, *auth.Codec) {
	t.Helper()
	cfg := &config.Config{
		Clusters: []config.Cluster{{Name: "dev", URL: esURL}, {Name: "prod", URL: esURL}},
		Roles: map[string]config.Role{
			"admin":    {Clusters: []string{"*"}, Permissions: []string{"admin"}},
			"dev-only": {Clusters: []string{"dev"}, Permissions: []string{"view"}},
		},
		LocalUsers: []config.LocalUser{
			{Name: "root", Password: testHash(t, "rootpw"), Roles: []string{"admin"}},
			{Name: "dev", Password: testHash(t, "devpw"), Roles: []string{"dev-only"}},
		},
		Session: config.Session{TTL: time.Hour},
	}
	logger := slog.New(slog.DiscardHandler)
	sessions, _, err := auth.NewCodec("0123456789abcdef0123456789abcdef", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := es.NewRegistry(cfg.Clusters)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(cfg, auth.New(cfg, logger), sessions, rbac.New(cfg.Roles), registry, logger)
	if err != nil {
		t.Fatal(err)
	}
	return s, sessions
}

func fakeES(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			w.Write([]byte(`{"version":{"number":"8.13.0"}}`))
		case r.URL.Path == "/_cluster/health":
			w.Write([]byte(`{"cluster_name":"t","status":"green","number_of_nodes":1}`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sessionCookieFor(t *testing.T, c *auth.Codec, user string, roles ...string) *http.Cookie {
	t.Helper()
	token, err := c.Encode(user, roles)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: token}
}

func TestSecurityHeaders(t *testing.T) {
	s, _ := testServer(t, "http://127.0.0.1:1")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

	for header, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "default-src 'none'",
	} {
		if got := rec.Header().Get(header); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want containing %q", header, got, want)
		}
	}
}

func TestCrossOriginPOSTRejected(t *testing.T) {
	s, _ := testServer(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestLoginFlow(t *testing.T) {
	s, _ := testServer(t, "http://127.0.0.1:1")

	form := url.Values{"username": {"root"}, "password": {"rootpw"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie || !cookies[0].HttpOnly {
		t.Fatalf("cookies = %+v", cookies)
	}
}

func TestLoginBadCredentials(t *testing.T) {
	s, _ := testServer(t, "http://127.0.0.1:1")
	form := url.Values{"username": {"root"}, "password": {"wrong"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "10.0.0.2:1234"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "Invalid username or password") {
		t.Error("error message missing")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("cookie set on failed login")
	}
}

func TestLoginRateLimit(t *testing.T) {
	s, _ := testServer(t, "http://127.0.0.1:1")
	var last int
	for range 10 {
		form := url.Values{"username": {"root"}, "password": {"wrong"}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "10.9.9.9:1"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("10th attempt status = %d, want 429", last)
	}
}

func TestLoginRateLimitPerUser(t *testing.T) {
	s, _ := testServer(t, "http://127.0.0.1:1")
	var last int
	for i := range 10 {
		form := url.Values{"username": {"root"}, "password": {"wrong"}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = fmt.Sprintf("10.9.9.%d:1", i)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("10th attempt from rotating IPs status = %d, want 429", last)
	}
}

func TestUnauthenticatedRedirects(t *testing.T) {
	s, _ := testServer(t, "http://127.0.0.1:1")
	for _, path := range []string{"/", "/c/dev/overview"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("%s: status=%d location=%q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestTamperedCookieRedirects(t *testing.T) {
	s, _ := testServer(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/c/dev/overview", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "forged.token"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want redirect to login", rec.Code)
	}
}

func TestRBACGating(t *testing.T) {
	esrv := fakeES(t)
	s, codec := testServer(t, esrv.URL)

	tests := []struct {
		name, user, role, path string
		want                   int
	}{
		{"admin sees prod", "root", "admin", "/c/prod/overview", http.StatusOK},
		{"dev-only sees dev", "dev", "dev-only", "/c/dev/overview", http.StatusOK},
		{"dev-only denied prod", "dev", "dev-only", "/c/prod/overview", http.StatusForbidden},
		{"unknown cluster 404", "root", "admin", "/c/ghost/overview", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.AddCookie(sessionCookieFor(t, codec, tt.user, tt.role))
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestHomeListsVisibleClusters(t *testing.T) {
	esrv := fakeES(t)
	s, codec := testServer(t, esrv.URL)

	// dev-only sees only dev, with health details
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(sessionCookieFor(t, codec, "dev", "dev-only"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK || !strings.Contains(string(body), `/c/dev/overview`) {
		t.Errorf("status=%d, dev card missing", rec.Code)
	}
	if strings.Contains(string(body), `/c/prod/overview`) {
		t.Error("dev-only user sees prod cluster")
	}
	if !strings.Contains(string(body), "green") {
		t.Error("health status missing from cluster card")
	}

	// admin sees both
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(sessionCookieFor(t, codec, "root", "admin"))
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body, _ = io.ReadAll(rec.Body)
	for _, want := range []string{"/c/dev/overview", "/c/prod/overview"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("admin clusters page missing %q", want)
		}
	}
}

func TestHomeShowsUnreachableCluster(t *testing.T) {
	s, codec := testServer(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(sessionCookieFor(t, codec, "root", "admin"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "unreachable") {
		t.Error("unreachable cluster not marked")
	}
}

func TestClusterSwitcherDropdown(t *testing.T) {
	esrv := fakeES(t)
	s, codec := testServer(t, esrv.URL)
	req := httptest.NewRequest(http.MethodGet, "/c/dev/indices", nil)
	req.AddCookie(sessionCookieFor(t, codec, "root", "admin"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	// switcher links to the same page on the other cluster
	if !strings.Contains(string(body), `href="/c/prod/indices"`) {
		t.Error("switcher missing same-page link to other cluster")
	}
	if !strings.Contains(string(body), `href="/"`) {
		t.Error("switcher missing all-clusters link")
	}
}

func TestOverviewRendersESError(t *testing.T) {
	s, codec := testServer(t, "http://127.0.0.1:1") // unreachable ES
	req := httptest.NewRequest(http.MethodGet, "/c/dev/overview", nil)
	req.AddCookie(sessionCookieFor(t, codec, "root", "admin"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK || !strings.Contains(string(body), "banner error") {
		t.Errorf("status=%d, error banner missing", rec.Code)
	}
}

func TestOverviewRenders(t *testing.T) {
	esrv := fakeES(t)
	s, codec := testServer(t, esrv.URL)
	req := httptest.NewRequest(http.MethodGet, "/c/dev/overview", nil)
	req.AddCookie(sessionCookieFor(t, codec, "root", "admin"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body, _ := io.ReadAll(rec.Body)
	for _, want := range []string{"green", "es8 8.13.0", "root", "admin"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("overview missing %q", want)
		}
	}
}

func TestClientIPTrustedProxy(t *testing.T) {
	esrv := fakeES(t)
	cfg := &config.Config{
		Clusters:       []config.Cluster{{Name: "dev", URL: esrv.URL}},
		Roles:          map[string]config.Role{"admin": {Clusters: []string{"*"}, Permissions: []string{"admin"}}},
		LocalUsers:     []config.LocalUser{{Name: "root", Password: "rootpw", Roles: []string{"admin"}}},
		Session:        config.Session{TTL: time.Hour},
		TrustedProxies: []string{"10.0.0.0/8"},
	}
	logger := slog.New(slog.DiscardHandler)
	sessions, _, _ := auth.NewCodec("0123456789abcdef0123456789abcdef", time.Hour)
	registry, _ := es.NewRegistry(cfg.Clusters)
	s, err := NewServer(cfg, auth.New(cfg, logger), sessions, rbac.New(cfg.Roles), registry, logger)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, remote, xff, want string
	}{
		{"direct client, no proxy", "203.0.113.5:1234", "", "203.0.113.5"},
		{"via trusted proxy", "10.0.0.1:1234", "203.0.113.9", "203.0.113.9"},
		{"chained trusted proxies", "10.0.0.1:1234", "203.0.113.9, 10.0.0.2", "203.0.113.9"},
		{"spoofed XFF from untrusted peer", "203.0.113.5:1234", "1.2.3.4", "203.0.113.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remote
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if got := s.clientIP(req); got != tt.want {
				t.Errorf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSecureCookieDefault(t *testing.T) {
	esrv := fakeES(t)
	s, _ := testServer(t, esrv.URL) // InsecureCookie false
	form := url.Values{"username": {"root"}, "password": {"rootpw"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "10.1.1.1:1"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure {
		t.Errorf("session cookie must be Secure by default: %+v", cookies)
	}
}
