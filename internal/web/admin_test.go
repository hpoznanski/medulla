package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hpoznanski/medulla/internal/auth"
	"github.com/hpoznanski/medulla/internal/config"
	"github.com/hpoznanski/medulla/internal/es"
	"github.com/hpoznanski/medulla/internal/rbac"
	"log/slog"
	"time"
)

// testServerWithRoles builds a server with fine-grained roles for console gating.
func testServerWithRoles(t *testing.T, esURL string) (*Server, *auth.Codec) {
	t.Helper()
	cfg := &config.Config{
		Clusters: []config.Cluster{{Name: "dev", URL: esURL}},
		Roles: map[string]config.Role{
			"admin":     {Clusters: []string{"*"}, Permissions: []string{"admin"}},
			"read-only": {Clusters: []string{"*"}, Permissions: []string{"view", "rest:get"}},
			"no-rest":   {Clusters: []string{"*"}, Permissions: []string{"view"}},
		},
		LocalUsers: []config.LocalUser{{Name: "x", Password: testHash(t, "x"), Roles: []string{"admin"}}},
		Session:    config.Session{TTL: time.Hour},
	}
	logger := slog.New(slog.DiscardHandler)
	sessions, _, _ := auth.NewCodec("0123456789abcdef0123456789abcdef", time.Hour)
	registry, _ := es.NewRegistry(cfg.Clusters)
	s, err := NewServer(cfg, auth.New(cfg, logger), sessions, rbac.New(cfg.Roles), registry, logger)
	if err != nil {
		t.Fatal(err)
	}
	return s, sessions
}

func fakeESAdmin(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_cat/aliases":
			w.Write([]byte(`[{"alias":"logs","index":"logs-1","filter":"-"}]`))
		case "/_index_template":
			w.Write([]byte(`{"index_templates":[{"name":"t1","index_template":{"index_patterns":["x-*"]}}]}`))
		case "/_snapshot/_all":
			w.Write([]byte(`{"backup":{"type":"fs","settings":{}}}`))
		case "/_snapshot/backup/_all":
			w.Write([]byte(`{"snapshots":[{"snapshot":"s1","state":"SUCCESS","indices":["a"]}]}`))
		case "/_analyze":
			w.Write([]byte(`{"tokens":[{"token":"hi","type":"<ALPHANUM>","position":0,"start_offset":0,"end_offset":2}]}`))
		case "/_cluster/health":
			w.Write([]byte(`{"status":"green"}`))
		default:
			w.Write([]byte(`{"acknowledged":true}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestConsoleGating(t *testing.T) {
	esrv := fakeESAdmin(t)
	s, codec := testServerWithRoles(t, esrv.URL)

	// no rest permission: console page 403
	rec, _ := get(t, s, sessionCookieFor(t, codec, "u", "no-rest"), "/c/dev/console")
	if rec.Code != http.StatusForbidden {
		t.Errorf("no-rest console = %d, want 403", rec.Code)
	}

	// rest:get: page renders without write methods, GET executes
	rec, body := get(t, s, sessionCookieFor(t, codec, "u", "read-only"), "/c/dev/console")
	if rec.Code != http.StatusOK || strings.Contains(body, "<option>DELETE</option>") {
		t.Errorf("read-only console: status=%d, must not offer DELETE", rec.Code)
	}
	recPost := post(t, s, sessionCookieFor(t, codec, "u", "read-only"), "/c/dev/console",
		url.Values{"method": {"GET"}, "path": {"/_cluster/health"}})
	if recPost.Code != http.StatusOK {
		t.Errorf("read-only GET = %d", recPost.Code)
	}

	// rest:get forging a DELETE: 403 server-side
	recPost = post(t, s, sessionCookieFor(t, codec, "u", "read-only"), "/c/dev/console",
		url.Values{"method": {"DELETE"}, "path": {"/logs-1"}})
	if recPost.Code != http.StatusForbidden {
		t.Errorf("read-only DELETE = %d, want 403", recPost.Code)
	}

	// admin DELETE allowed
	recPost = post(t, s, sessionCookieFor(t, codec, "u", "admin"), "/c/dev/console",
		url.Values{"method": {"DELETE"}, "path": {"/logs-1"}})
	if recPost.Code != http.StatusOK {
		t.Errorf("admin DELETE = %d", recPost.Code)
	}

	// junk method rejected
	recPost = post(t, s, sessionCookieFor(t, codec, "u", "admin"), "/c/dev/console",
		url.Values{"method": {"TRACE"}, "path": {"/"}})
	if recPost.Code != http.StatusBadRequest {
		t.Errorf("TRACE = %d, want 400", recPost.Code)
	}
}

func TestAliasesPage(t *testing.T) {
	esrv := fakeESAdmin(t)
	s, codec := testServerWithRoles(t, esrv.URL)

	_, body := get(t, s, sessionCookieFor(t, codec, "u", "admin"), "/c/dev/aliases")
	if !strings.Contains(body, "logs-1") || !strings.Contains(body, "Add / remove alias") {
		t.Error("admin aliases page incomplete")
	}

	_, viewerBody := get(t, s, sessionCookieFor(t, codec, "u", "no-rest"), "/c/dev/aliases")
	if strings.Contains(viewerBody, "Add / remove alias") {
		t.Error("viewer sees alias form")
	}

	rec := post(t, s, sessionCookieFor(t, codec, "u", "no-rest"), "/c/dev/aliases",
		url.Values{"action": {"add"}, "index": {"logs-1"}, "alias": {"x"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer alias POST = %d, want 403", rec.Code)
	}
}

func TestTemplatesPage(t *testing.T) {
	esrv := fakeESAdmin(t)
	s, codec := testServerWithRoles(t, esrv.URL)

	_, body := get(t, s, sessionCookieFor(t, codec, "u", "admin"), "/c/dev/templates")
	if !strings.Contains(body, "t1") || !strings.Contains(body, "Create / update template") {
		t.Error("admin templates page incomplete")
	}

	rec := post(t, s, sessionCookieFor(t, codec, "u", "admin"), "/c/dev/templates",
		url.Values{"name": {"t2"}, "body": {`{"index_patterns":["y-*"]}`}})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "ok") {
		t.Errorf("template save: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	rec = post(t, s, sessionCookieFor(t, codec, "u", "no-rest"), "/c/dev/templates", url.Values{})
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer template POST = %d, want 403", rec.Code)
	}
}

func TestSnapshotsPage(t *testing.T) {
	esrv := fakeESAdmin(t)
	s, codec := testServerWithRoles(t, esrv.URL)

	_, body := get(t, s, sessionCookieFor(t, codec, "u", "admin"), "/c/dev/snapshots")
	for _, want := range []string{"backup", "s1", "Register repository", "restore"} {
		if !strings.Contains(body, want) {
			t.Errorf("snapshots page missing %q", want)
		}
	}

	// delete without matching confirmation bounces
	rec := post(t, s, sessionCookieFor(t, codec, "u", "admin"), "/c/dev/snapshots/delete",
		url.Values{"repo": {"backup"}, "name": {"s1"}, "confirm": {"nope"}})
	if !strings.Contains(rec.Header().Get("Location"), "confirm") {
		t.Errorf("unconfirmed delete proceeded: %q", rec.Header().Get("Location"))
	}

	rec = post(t, s, sessionCookieFor(t, codec, "u", "no-rest"), "/c/dev/snapshots/create",
		url.Values{"repo": {"backup"}, "name": {"s2"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer snapshot POST = %d, want 403", rec.Code)
	}
}

func TestAnalyzePage(t *testing.T) {
	esrv := fakeESAdmin(t)
	s, codec := testServerWithRoles(t, esrv.URL)

	rec := post(t, s, sessionCookieFor(t, codec, "u", "no-rest"), "/c/dev/analyze",
		url.Values{"analyzer": {"standard"}, "text": {"hi"}})
	if rec.Code != http.StatusOK {
		t.Errorf("analyze = %d", rec.Code)
	}
}

func TestClusterSettingsPage(t *testing.T) {
	esrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_cluster/settings" && r.Method == http.MethodGet {
			w.Write([]byte(`{"persistent":{"cluster.routing.allocation.enable":"all"},"transient":{},"defaults":{"cluster.name":"x"}}`))
			return
		}
		w.Write([]byte(`{"acknowledged":true}`))
	}))
	t.Cleanup(esrv.Close)
	s, codec := testServerWithRoles(t, esrv.URL)

	// admin sees settings + edit form
	_, body := get(t, s, sessionCookieFor(t, codec, "u", "admin"), "/c/dev/settings")
	for _, want := range []string{"cluster.routing.allocation.enable", "Set persistent setting", "Defaults (1)"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}

	// viewer: no edit form, POST forbidden
	_, viewerBody := get(t, s, sessionCookieFor(t, codec, "u", "no-rest"), "/c/dev/settings")
	if strings.Contains(viewerBody, "Set persistent setting") {
		t.Error("viewer sees settings form")
	}
	rec := post(t, s, sessionCookieFor(t, codec, "u", "no-rest"), "/c/dev/settings",
		url.Values{"key": {"cluster.routing.allocation.enable"}, "value": {"none"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer settings POST = %d, want 403", rec.Code)
	}

	// admin can set
	rec = post(t, s, sessionCookieFor(t, codec, "u", "admin"), "/c/dev/settings",
		url.Values{"key": {"cluster.routing.allocation.enable"}, "value": {"none"}})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "ok") {
		t.Errorf("admin settings POST: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// bad key rejected via redirect notice
	rec = post(t, s, sessionCookieFor(t, codec, "u", "admin"), "/c/dev/settings",
		url.Values{"key": {"bad key with spaces"}, "value": {"x"}})
	if !strings.Contains(rec.Header().Get("Location"), "invalid+setting+key") {
		t.Errorf("bad key accepted: %q", rec.Header().Get("Location"))
	}
}
