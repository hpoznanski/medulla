package web

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hpoznanski/medulla/internal/auth"
	"github.com/hpoznanski/medulla/internal/config"
	"github.com/hpoznanski/medulla/internal/es"
	"github.com/hpoznanski/medulla/internal/rbac"
)

// testServerClusters builds a server over arbitrary clusters, all visible to
// the "admin" role and only "green" to "green-only".
func testServerClusters(t *testing.T, clusters []config.Cluster) (*Server, *auth.Codec) {
	t.Helper()
	cfg := &config.Config{
		Clusters: clusters,
		Roles: map[string]config.Role{
			"admin":      {Clusters: []string{"*"}, Permissions: []string{"admin"}},
			"green-only": {Clusters: []string{"green"}, Permissions: []string{"view"}},
		},
		Session: config.Session{TTL: time.Hour},
	}
	logger := slog.New(slog.DiscardHandler)
	codec, _, err := auth.NewCodec("0123456789abcdef0123456789abcdef", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := es.NewRegistry(cfg.Clusters)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(cfg, auth.New(cfg, logger), codec, rbac.New(cfg.Roles), registry, logger)
	if err != nil {
		t.Fatal(err)
	}
	return s, codec
}

// fakeCluster answers health with the given status; stats can be made to fail.
func fakeCluster(t *testing.T, health string, statsOK bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Write([]byte(`{"version":{"number":"8.13.0"}}`))
		case "/_cluster/health":
			fmt.Fprintf(w, `{"status":%q,"number_of_nodes":3,"active_shards":10,"unassigned_shards":2,"relocating_shards":1}`, health)
		case "/_cluster/stats":
			if !statsOK {
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":{"type":"security_exception","reason":"no stats for you"}}`))
				return
			}
			w.Write([]byte(`{"indices":{"count":4,"docs":{"count":99},"store":{"size_in_bytes":2048}}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func statusServer(t *testing.T, code int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		w.Write([]byte(`{"error":{"type":"x","reason":"nope"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func esClientFor(t *testing.T, rawURL string) *es.Client {
	t.Helper()
	reg, err := es.NewRegistry([]config.Cluster{{Name: "c", URL: rawURL}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := reg.Get("c")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestProbeClusterReasons(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.NotFoundHandler()) // cert not in any trusted pool
	t.Cleanup(tlsSrv.Close)

	tests := []struct {
		name, url, reason string
	}{
		{"refused", "http://127.0.0.1:1", "refused"},
		{"auth 401", statusServer(t, http.StatusUnauthorized), "auth"},
		{"auth 403", statusServer(t, http.StatusForbidden), "auth"},
		{"server error", statusServer(t, http.StatusServiceUnavailable), "http 503"},
		{"untrusted cert", tlsSrv.URL, "tls"},
	}
	for _, tt := range tests {
		st := probeCluster(context.Background(), esClientFor(t, tt.url))
		if !st.Unreachable() || st.Reason != tt.reason || st.Error == "" {
			t.Errorf("%s: health=%q reason=%q err=%q", tt.name, st.Health, st.Reason, st.Error)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(slow.Close)
	if st := probeCluster(ctx, esClientFor(t, slow.URL)); st.Reason != "timeout" {
		t.Errorf("slow cluster reason = %q, want timeout", st.Reason)
	}
	if got := unreachableReason(&net.DNSError{Err: "no such host", Name: "es"}); got != "dns" {
		t.Errorf("dns error reason = %q", got)
	}
}

// Health answered but stats did not: keep what we know, flag the gap, and
// never show a made-up zero.
func TestProbeClusterPartial(t *testing.T) {
	st := probeCluster(context.Background(), esClientFor(t, fakeCluster(t, "yellow", false)))
	if st.Unreachable() || st.Health != "yellow" || st.Nodes != 3 || st.Relocating != 1 {
		t.Fatalf("health data lost: %+v", st)
	}
	if st.HasStats || !strings.Contains(st.Partial, "no stats for you") {
		t.Errorf("partial not flagged: HasStats=%v Partial=%q", st.HasStats, st.Partial)
	}
}

func TestSortClusters(t *testing.T) {
	list := []clusterStatus{
		{Name: "a-green", Health: "green", HasStats: true, Docs: 5, Nodes: 3},
		{Name: "b-dead", Health: healthUnreachable},
		{Name: "c-red", Health: "red", HasStats: true, Docs: 50, Nodes: 1},
		{Name: "d-partial", Health: "yellow", Nodes: 2},
	}
	names := func() []string {
		var out []string
		for _, c := range list {
			out = append(out, c.Name)
		}
		return out
	}
	for _, tt := range []struct {
		view listView
		want []string
	}{
		{listView{Sort: "health", Desc: true}, []string{"b-dead", "c-red", "d-partial", "a-green"}},
		// missing values sort last in both directions
		{listView{Sort: "docs", Desc: true}, []string{"c-red", "a-green", "b-dead", "d-partial"}},
		{listView{Sort: "docs"}, []string{"a-green", "c-red", "b-dead", "d-partial"}},
		{listView{Sort: "nodes"}, []string{"c-red", "d-partial", "a-green", "b-dead"}},
	} {
		sortClusters(list, tt.view)
		if got := names(); !slices.Equal(got, tt.want) {
			t.Errorf("%s desc=%v: got %v, want %v", tt.view.Sort, tt.view.Desc, got, tt.want)
		}
	}
	if compareVersion("8.9.0", "8.13.0") >= 0 {
		t.Error("versions compared as text")
	}
}

func homeServer(t *testing.T) (*Server, *http.Cookie, *http.Cookie) {
	t.Helper()
	s, codec := testServerClusters(t, []config.Cluster{
		{Name: "green", URL: fakeCluster(t, "green", true)},
		{Name: "partial", URL: fakeCluster(t, "yellow", false)},
		{Name: "dead", URL: "http://127.0.0.1:1"},
	})
	return s, sessionCookieFor(t, codec, "root", "admin"), sessionCookieFor(t, codec, "g", "green-only")
}

func TestHomeListLayout(t *testing.T) {
	s, admin, _ := homeServer(t)

	rec, body := get(t, s, admin, "/?layout=list")
	if rec.Code != http.StatusOK || !strings.Contains(body, `class="cluster-list"`) {
		t.Fatalf("list layout not rendered: %d", rec.Code)
	}
	dead := strings.Index(body, `href="/c/dead/overview"`)
	green := strings.Index(body, `href="/c/green/overview"`)
	if dead < 0 || green < 0 || dead > green {
		t.Error("unreachable cluster must be listed before healthy ones")
	}
	for _, want := range []string{"refused", "partial", "<b>3</b> clusters", "<b>1</b> unreachable", `<option value="dead">`} {
		if !strings.Contains(body, want) {
			t.Errorf("list layout missing %q", want)
		}
	}

	// the choice sticks via the preference cookie
	var pref *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == layoutCookie {
			pref = c
		}
	}
	if pref == nil || pref.Value != "list" {
		t.Fatalf("layout cookie = %+v", pref)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(admin)
	req.AddCookie(pref)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `class="cluster-list"`) {
		t.Error("layout preference not remembered")
	}
}

func TestHomeFilters(t *testing.T) {
	s, admin, _ := homeServer(t)

	_, body := get(t, s, admin, "/?health=unreachable")
	if !strings.Contains(body, "/c/dead/overview") || strings.Contains(body, "/c/green/overview") {
		t.Error("health filter not applied")
	}
	_, body = get(t, s, admin, "/?q=GRE")
	if !strings.Contains(body, "/c/green/overview") || strings.Contains(body, "/c/dead/overview") {
		t.Error("name filter not applied case-insensitively")
	}
	_, body = get(t, s, admin, "/?q=nothing-matches")
	if !strings.Contains(body, "No clusters match") {
		t.Error("empty result not explained")
	}
}
