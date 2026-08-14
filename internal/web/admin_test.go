package web

import (
	"fmt"
	"html"
	"io"
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

// doWith issues a request carrying several cookies and returns the recorder.
func doWith(t *testing.T, s *Server, cookies []*http.Cookie, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if method == http.MethodGet {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func historyCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == consoleCookie {
			return c
		}
	}
	return nil
}

// decodeHistory reads the entries a cookie carries, through the same
// verification path the handler uses.
func decodeHistory(t *testing.T, s *Server, c *http.Cookie) []consoleEntry {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if c != nil {
		req.AddCookie(c)
	}
	return s.consoleHistory(req)
}

func TestConsoleHistoryRoundTrip(t *testing.T) {
	esrv := fakeESAdmin(t)
	s, codec := testServerWithRoles(t, esrv.URL)
	admin := sessionCookieFor(t, codec, "u", "admin")

	rec := doWith(t, s, []*http.Cookie{admin}, http.MethodPost, "/c/dev/console",
		url.Values{"method": {"GET"}, "path": {"/_cluster/health"}})
	hist := historyCookie(t, rec)
	if hist == nil {
		t.Fatal("no history cookie set")
	}
	if !hist.HttpOnly || hist.SameSite != http.SameSiteLaxMode {
		t.Errorf("history cookie flags: HttpOnly=%v SameSite=%v", hist.HttpOnly, hist.SameSite)
	}

	// the executed request comes back on the page it was run from...
	if body := rec.Body.String(); !strings.Contains(body, "History") {
		t.Error("history section missing from the response that created it")
	}
	// ...and on a later page load carrying the cookie.
	rec = doWith(t, s, []*http.Cookie{admin, hist}, http.MethodGet, "/c/dev/console", nil)
	if body := rec.Body.String(); !strings.Contains(body, "/_cluster/health") {
		t.Error("history entry not rendered on later load")
	}

	// prefill from a history link must not execute anything
	rec = doWith(t, s, []*http.Cookie{admin, hist}, http.MethodGet,
		"/c/dev/console?method=DELETE&path=/logs-1", nil)
	if body := rec.Body.String(); !strings.Contains(body, `value="/logs-1"`) {
		t.Error("prefill did not populate the path field")
	}
	if strings.Contains(rec.Body.String(), "Response — HTTP") {
		t.Error("prefill executed the request")
	}
}

// A history link is only useful if the path survives the round trip through
// the URL, so follow the rendered href rather than a hand-built one.
func TestConsoleHistoryLinkRoundTrip(t *testing.T) {
	esrv := fakeESAdmin(t)
	s, codec := testServerWithRoles(t, esrv.URL)
	admin := sessionCookieFor(t, codec, "u", "admin")

	const path = "/_cat/indices?v&h=index,health"
	rec := doWith(t, s, []*http.Cookie{admin}, http.MethodPost, "/c/dev/console",
		url.Values{"method": {"GET"}, "path": {path}})

	href := historyHref(t, rec.Body.String())
	rec = doWith(t, s, []*http.Cookie{admin, historyCookie(t, rec)}, http.MethodGet, href, nil)
	if body := rec.Body.String(); !strings.Contains(body, `value="`+html.EscapeString(path)+`"`) {
		t.Errorf("following %q did not restore the path field", href)
	}
}

// historyHref returns the first console history link in a rendered page.
func historyHref(t *testing.T, body string) string {
	t.Helper()
	_, after, ok := strings.Cut(body, `<a href="/c/dev/console?method=`)
	if !ok {
		t.Fatal("no history link rendered")
	}
	link, _, _ := strings.Cut(after, `"`)
	return "/c/dev/console?method=" + html.UnescapeString(link)
}

func TestConsoleHistoryRejectsTamperedCookie(t *testing.T) {
	esrv := fakeESAdmin(t)
	s, codec := testServerWithRoles(t, esrv.URL)
	admin := sessionCookieFor(t, codec, "u", "admin")

	rec := doWith(t, s, []*http.Cookie{admin}, http.MethodPost, "/c/dev/console",
		url.Values{"method": {"GET"}, "path": {"/_cluster/health"}})
	hist := historyCookie(t, rec)

	body, _, _ := strings.Cut(hist.Value, ".")
	forged := &http.Cookie{Name: consoleCookie, Value: body + ".not-a-real-signature"}
	if got := decodeHistory(t, s, forged); got != nil {
		t.Errorf("forged history accepted: %+v", got)
	}
}

func TestConsoleHistoryEviction(t *testing.T) {
	esrv := fakeESAdmin(t)
	s, codec := testServerWithRoles(t, esrv.URL)
	admin := sessionCookieFor(t, codec, "u", "admin")

	var hist *http.Cookie
	for i := range consoleHistoryMax + 5 {
		cookies := []*http.Cookie{admin}
		if hist != nil {
			cookies = append(cookies, hist)
		}
		rec := doWith(t, s, cookies, http.MethodPost, "/c/dev/console",
			url.Values{"method": {"GET"}, "path": {fmt.Sprintf("/idx-%02d/_stats", i)}})
		if c := historyCookie(t, rec); c != nil {
			hist = c
		}
	}

	entries := decodeHistory(t, s, hist)
	if len(entries) > consoleHistoryMax {
		t.Errorf("history holds %d entries, want at most %d", len(entries), consoleHistoryMax)
	}
	if len(hist.Value) > consoleCookieMax {
		t.Errorf("cookie is %d bytes, want at most %d", len(hist.Value), consoleCookieMax)
	}
	if entries[0].Path != fmt.Sprintf("/idx-%02d/_stats", consoleHistoryMax+4) {
		t.Errorf("newest entry = %q, want the most recent request", entries[0].Path)
	}
}

func TestConsoleOversizedBodyDropped(t *testing.T) {
	esrv := fakeESAdmin(t)
	s, codec := testServerWithRoles(t, esrv.URL)
	admin := sessionCookieFor(t, codec, "u", "admin")

	big := `{"q":"` + strings.Repeat("x", consoleBodyMax) + `"}`
	rec := doWith(t, s, []*http.Cookie{admin}, http.MethodPost, "/c/dev/console",
		url.Values{"method": {"POST"}, "path": {"/idx/_search"}, "body": {big}})

	entries := decodeHistory(t, s, historyCookie(t, rec))
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Body != "" {
		t.Error("oversized body stored; a truncated body would look re-runnable but be invalid JSON")
	}
	if entries[0].Path != "/idx/_search" {
		t.Errorf("path = %q, want the request kept without its body", entries[0].Path)
	}
}

func TestConsolePrettyPrintsResponse(t *testing.T) {
	esrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"a":{"b":1}}`)) // single line in, indented out
	}))
	t.Cleanup(esrv.Close)
	s, codec := testServerWithRoles(t, esrv.URL)

	rec := doWith(t, s, []*http.Cookie{sessionCookieFor(t, codec, "u", "admin")}, http.MethodPost,
		"/c/dev/console", url.Values{"method": {"GET"}, "path": {"/x"}})
	if body := rec.Body.String(); !strings.Contains(body, "&#34;a&#34;: {\n") {
		t.Error("response was not re-indented")
	}
}

// fakeESRouting serves an overview plus cluster settings, recording writes.
func fakeESRouting(t *testing.T, put *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/_cluster/settings" {
			b, _ := io.ReadAll(r.Body)
			*put = string(b)
			w.Write([]byte(`{"acknowledged":true}`))
			return
		}
		switch r.URL.Path {
		case "/_cluster/settings":
			w.Write([]byte(`{"persistent":{},"transient":{},"defaults":{}}`))
		case "/_cluster/health":
			w.Write([]byte(`{"status":"green","number_of_nodes":2}`))
		case "/_cat/nodes":
			w.Write([]byte(`[{"name":"es01","ip":"10.0.0.1","node.role":"dm","master":"*"},{"name":"es02","ip":"10.0.0.2","node.role":"dm","master":"-"}]`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNodeExcludeRequiresConfirmation(t *testing.T) {
	var put string
	esrv := fakeESRouting(t, &put)
	s, codec := testServerWithRoles(t, esrv.URL)
	admin := sessionCookieFor(t, codec, "u", "admin")

	// draining moves every shard off the node, so a mistyped name must not proceed
	rec := post(t, s, admin, "/c/dev/routing/exclude",
		url.Values{"node": {"es01"}, "action": {"exclude"}, "confirm": {"es02"}})
	if put != "" {
		t.Errorf("unconfirmed exclude wrote to ES: %s", put)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "confirm") {
		t.Errorf("notice = %q", loc)
	}

	rec = post(t, s, admin, "/c/dev/routing/exclude",
		url.Values{"node": {"es01"}, "action": {"exclude"}, "confirm": {"es01"}})
	if !strings.Contains(put, `"cluster.routing.allocation.exclude._name":"es01"`) {
		t.Errorf("confirmed exclude body = %s", put)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "draining") {
		t.Errorf("notice = %q", loc)
	}

	// re-including is safe and needs no confirmation
	put = ""
	post(t, s, admin, "/c/dev/routing/exclude", url.Values{"node": {"es01"}, "action": {"include"}})
	if !strings.Contains(put, `"cluster.routing.allocation.exclude._name":null`) {
		t.Errorf("include body = %s", put)
	}
}

// An excluded node reads "draining" only while it still holds shards; once
// empty it must say drained, which is the signal that it is safe to stop.
func TestExcludedNodeShowsDrainProgress(t *testing.T) {
	esrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_cluster/settings":
			w.Write([]byte(`{"persistent":{"cluster":{"routing":{"allocation":{"exclude":{"_name":"es01,es02"}}}}},"transient":{},"defaults":{}}`))
		case "/_cluster/health":
			w.Write([]byte(`{"status":"green","number_of_nodes":2}`))
		case "/_cat/nodes":
			w.Write([]byte(`[{"name":"es01","ip":"10.0.0.1"},{"name":"es02","ip":"10.0.0.2"}]`))
		case "/_cat/shards":
			// es01 still holds two; es02 is empty, and the unassigned shard
			// must not be counted against any node
			w.Write([]byte(`[{"index":"i","shard":"0","prirep":"p","state":"STARTED","node":"es01"},
				{"index":"i","shard":"1","prirep":"p","state":"STARTED","node":"es01"},
				{"index":"i","shard":"2","prirep":"r","state":"UNASSIGNED","node":""}]`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(esrv.Close)
	s, codec := testServerWithRoles(t, esrv.URL)

	_, body := get(t, s, sessionCookieFor(t, codec, "u", "admin"), "/c/dev/overview")
	if !strings.Contains(body, "draining · 2 left") {
		t.Error("node still holding shards is not reported as draining with a count")
	}
	if !strings.Contains(body, "drained — safe to stop") {
		t.Error("emptied node still reads as draining")
	}
}

func TestRoutingNeedsClusterWrite(t *testing.T) {
	var put string
	esrv := fakeESRouting(t, &put)
	s, codec := testServerWithRoles(t, esrv.URL)

	// read-only has view+rest:get but not cluster:write
	for _, path := range []string{"/c/dev/routing", "/c/dev/routing/exclude"} {
		rec := post(t, s, sessionCookieFor(t, codec, "u", "read-only"), path,
			url.Values{"kind": {"allocation"}, "value": {"none"}, "node": {"es01"}, "action": {"exclude"}, "confirm": {"es01"}})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403", path, rec.Code)
		}
	}
	if put != "" {
		t.Errorf("denied user still wrote to ES: %s", put)
	}
}

func TestRoutingControlsHiddenWithoutPermission(t *testing.T) {
	var put string
	esrv := fakeESRouting(t, &put)
	s, codec := testServerWithRoles(t, esrv.URL)

	_, adminBody := get(t, s, sessionCookieFor(t, codec, "u", "admin"), "/c/dev/overview")
	for _, want := range []string{"Cluster routing", "allocation.enable", "exclude"} {
		if !strings.Contains(adminBody, want) {
			t.Errorf("admin overview missing %q", want)
		}
	}

	_, viewerBody := get(t, s, sessionCookieFor(t, codec, "u", "read-only"), "/c/dev/overview")
	if strings.Contains(viewerBody, "Cluster routing") || strings.Contains(viewerBody, `value="exclude"`) {
		t.Error("viewer sees routing controls")
	}
	// Nothing is excluded here, so the column would be entirely empty for them.
	if strings.Contains(viewerBody, "<th>allocation</th>") {
		t.Error("viewer sees an empty allocation column")
	}
}

// The drain badge is read-only status worth showing to a viewer; the buttons
// are not. The column appears for them only when a node is actually excluded.
func TestViewerSeesDrainStatusButNoControls(t *testing.T) {
	esrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_cluster/settings":
			w.Write([]byte(`{"persistent":{"cluster":{"routing":{"allocation":{"exclude":{"_name":"es01"}}}}},"transient":{},"defaults":{}}`))
		case "/_cluster/health":
			w.Write([]byte(`{"status":"green","number_of_nodes":1}`))
		case "/_cat/nodes":
			w.Write([]byte(`[{"name":"es01","ip":"10.0.0.1"}]`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(esrv.Close)
	s, codec := testServerWithRoles(t, esrv.URL)

	_, body := get(t, s, sessionCookieFor(t, codec, "u", "read-only"), "/c/dev/overview")
	if !strings.Contains(body, "<th>allocation</th>") || !strings.Contains(body, "drained") {
		t.Error("viewer cannot see that a node is excluded")
	}
	if strings.Contains(body, "re-include") || strings.Contains(body, `value="exclude"`) {
		t.Error("viewer sees routing buttons")
	}
}

func TestRestrictedRoutingWarns(t *testing.T) {
	esrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_cluster/settings":
			w.Write([]byte(`{"persistent":{"cluster":{"routing":{"allocation":{"enable":"none"}}}},"transient":{},"defaults":{}}`))
		case "/_cluster/health":
			w.Write([]byte(`{"status":"green","number_of_nodes":1}`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(esrv.Close)
	s, codec := testServerWithRoles(t, esrv.URL)

	_, body := get(t, s, sessionCookieFor(t, codec, "u", "read-only"), "/c/dev/overview")
	if !strings.Contains(body, "Shard routing is restricted") {
		t.Error("no warning banner while allocation is disabled")
	}
}

// A cluster that will not report its routing state must still render.
func TestOverviewSurvivesRoutingFailure(t *testing.T) {
	esrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_cluster/settings" {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"type":"security_exception","reason":"no permission"}}`))
			return
		}
		if r.URL.Path == "/_cluster/health" {
			w.Write([]byte(`{"status":"green","number_of_nodes":1}`))
			return
		}
		w.Write([]byte(`[]`))
	}))
	t.Cleanup(esrv.Close)
	s, codec := testServerWithRoles(t, esrv.URL)

	rec, body := get(t, s, sessionCookieFor(t, codec, "u", "admin"), "/c/dev/overview")
	if rec.Code != http.StatusOK || !strings.Contains(body, "shardgrid") {
		t.Errorf("overview lost to a routing read failure: status=%d", rec.Code)
	}
	// An unreadable state must not be dressed up as a known restriction, and
	// controls that would submit a bogus current value must not render.
	if strings.Contains(body, "Shard routing is restricted") {
		t.Error("unreadable routing state reported as restricted")
	}
	if !strings.Contains(body, "could not be read") {
		t.Error("no indication that routing state is unavailable")
	}
	if strings.Contains(body, "Cluster routing") || strings.Contains(body, "<th>allocation</th>") {
		t.Error("routing controls rendered without a known current state")
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
