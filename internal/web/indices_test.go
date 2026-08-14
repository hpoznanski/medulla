package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func fakeESIndices(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_cat/indices":
			w.Write([]byte(`[{"index":"logs-1","health":"green","status":"open","pri":"1","rep":"1","docs.count":"5","store.size":"10kb"}]`))
		case r.URL.Path == "/_cat/shards":
			w.Write([]byte(`[{"index":"logs-1","shard":"0","prirep":"p","state":"STARTED","node":"n1"}]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_refresh"):
			w.Write([]byte(`{}`))
		case r.Method == http.MethodDelete:
			w.Write([]byte(`{"acknowledged":true}`))
		case r.URL.Path == "/logs-1/_settings":
			w.Write([]byte(`{"logs-1":{"settings":{"index.number_of_shards":"1"},"defaults":{"index.codec":"LZ4"}}}`))
		case r.URL.Path == "/logs-1":
			w.Write([]byte(`{"logs-1":{"settings":{"index":{"number_of_shards":"1"}}}}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, s *Server, cookie *http.Cookie, path string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	return rec, string(body)
}

func post(t *testing.T, s *Server, cookie *http.Cookie, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestIndicesRBACVisibility(t *testing.T) {
	esrv := fakeESIndices(t)
	s, codec := testServer(t, esrv.URL)

	// dev-only role has only "view": no create form, no action buttons
	_, viewerBody := get(t, s, sessionCookieFor(t, codec, "dev", "dev-only"), "/c/dev/indices")
	if strings.Contains(viewerBody, "Create index") || strings.Contains(viewerBody, "forcemerge") {
		t.Error("viewer sees write controls")
	}
	if !strings.Contains(viewerBody, "logs-1") {
		t.Error("viewer missing index list")
	}

	// admin sees write controls
	_, adminBody := get(t, s, sessionCookieFor(t, codec, "root", "admin"), "/c/dev/indices")
	for _, want := range []string{"Create index", "refresh", "forcemerge", "close"} {
		if !strings.Contains(adminBody, want) {
			t.Errorf("admin missing %q", want)
		}
	}
}

func TestIndexActionRBACEnforced(t *testing.T) {
	esrv := fakeESIndices(t)
	s, codec := testServer(t, esrv.URL)

	// viewer blocked server-side even if they forge the request
	rec := post(t, s, sessionCookieFor(t, codec, "dev", "dev-only"), "/c/dev/indices/logs-1/refresh", url.Values{})
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer refresh = %d, want 403", rec.Code)
	}

	rec = post(t, s, sessionCookieFor(t, codec, "root", "admin"), "/c/dev/indices/logs-1/refresh", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Errorf("admin refresh = %d, want 303", rec.Code)
	}
}

func TestDeleteRequiresConfirmation(t *testing.T) {
	esrv := fakeESIndices(t)
	s, codec := testServer(t, esrv.URL)
	admin := sessionCookieFor(t, codec, "root", "admin")

	rec := post(t, s, admin, "/c/dev/indices/logs-1/delete", url.Values{"confirm": {"wrong-name"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "confirm") {
		t.Errorf("mismatched confirmation proceeded: %q", loc)
	}

	rec = post(t, s, admin, "/c/dev/indices/logs-1/delete", url.Values{"confirm": {"logs-1"}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "delete+logs-1") {
		t.Errorf("confirmed delete failed: %q", loc)
	}
}

func TestIndexDetailPage(t *testing.T) {
	esrv := fakeESIndices(t)
	s, codec := testServer(t, esrv.URL)

	_, body := get(t, s, sessionCookieFor(t, codec, "root", "admin"), "/c/dev/indices/logs-1")
	if !strings.Contains(body, "number_of_shards") || !strings.Contains(body, "Delete index") {
		t.Error("detail page incomplete for admin")
	}
	// settings table shows explicit values and ES defaults, each labelled
	for _, want := range []string{"index.number_of_shards", "index.codec", "LZ4", ">default<", ">set<"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings table missing %q", want)
		}
	}

	_, viewerBody := get(t, s, sessionCookieFor(t, codec, "dev", "dev-only"), "/c/dev/indices/logs-1")
	if strings.Contains(viewerBody, "Delete index") {
		t.Error("viewer sees delete form")
	}
}

func TestIndexSettingPut(t *testing.T) {
	var gotPath, gotBody string
	esrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/_settings") {
			gotPath = r.URL.Path
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
		}
		w.Write([]byte(`{"acknowledged":true}`))
	}))
	t.Cleanup(esrv.Close)
	s, codec := testServer(t, esrv.URL)
	admin := sessionCookieFor(t, codec, "root", "admin")

	rec := post(t, s, admin, "/c/dev/indices/logs-1/settings",
		url.Values{"key": {"index.number_of_replicas"}, "value": {"2"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if gotPath != "/logs-1/_settings" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody != `{"index.number_of_replicas":"2"}` {
		t.Errorf("body = %s", gotBody)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "set+index.number_of_replicas") {
		t.Errorf("notice = %q", loc)
	}

	// empty value resets: ES takes null, and the notice says reset
	rec = post(t, s, admin, "/c/dev/indices/logs-1/settings",
		url.Values{"key": {"index.refresh_interval"}, "value": {""}})
	if gotBody != `{"index.refresh_interval":null}` {
		t.Errorf("reset body = %s", gotBody)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "reset+index.refresh_interval") {
		t.Errorf("reset notice = %q", loc)
	}

	// bad key never reaches ES
	gotBody = ""
	rec = post(t, s, admin, "/c/dev/indices/logs-1/settings",
		url.Values{"key": {"index.bad key"}, "value": {"x"}})
	if gotBody != "" {
		t.Errorf("bad key sent to ES: %s", gotBody)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "invalid+setting+key") {
		t.Errorf("bad key notice = %q", loc)
	}

	// viewer blocked server-side
	rec = post(t, s, sessionCookieFor(t, codec, "dev", "dev-only"), "/c/dev/indices/logs-1/settings",
		url.Values{"key": {"index.number_of_replicas"}, "value": {"0"}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer setting put = %d, want 403", rec.Code)
	}
}

// The settings route must win over the generic {action} route, which would
// otherwise treat "settings" as an unknown index action.
func TestIndexSettingsRouteBeatsActionRoute(t *testing.T) {
	esrv := fakeESIndices(t)
	s, codec := testServer(t, esrv.URL)

	rec := post(t, s, sessionCookieFor(t, codec, "root", "admin"), "/c/dev/indices/logs-1/settings",
		url.Values{"key": {"index.number_of_replicas"}, "value": {"1"}})
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "unknown+action") {
		t.Errorf("settings was routed to handleIndexAction: %q", loc)
	}
}

func TestCatPage(t *testing.T) {
	esrv := fakeESIndices(t)
	s, codec := testServer(t, esrv.URL)
	viewer := sessionCookieFor(t, codec, "dev", "dev-only")

	rec, body := get(t, s, viewer, "/c/dev/cat/shards")
	if rec.Code != http.StatusOK || !strings.Contains(body, "STARTED") {
		t.Errorf("cat page: status=%d", rec.Code)
	}

	rec, _ = get(t, s, viewer, "/c/dev/cat/not-an-endpoint")
	if rec.Code != http.StatusNotFound {
		t.Errorf("bad endpoint = %d, want 404", rec.Code)
	}
}
