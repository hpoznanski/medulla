package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/hpoznanski/medulla/internal/es"
)

func TestParseOverviewViewAllowlists(t *testing.T) {
	tests := []struct {
		query string
		want  overviewView
	}{
		{"", overviewView{Sort: "name"}},
		{"sort=cpu", overviewView{Sort: "cpu", Desc: true}},
		{"sort=cpu&dir=asc", overviewView{Sort: "cpu"}},
		{"sort=name&dir=desc", overviewView{Sort: "name", Desc: true}},
		{"sort=__proto__&dir=sideways", overviewView{Sort: "name"}},
		{"refresh=30", overviewView{Sort: "name", Refresh: 30}},
		{"refresh=1", overviewView{Sort: "name"}},
		{"refresh=-10", overviewView{Sort: "name"}},
		{"refresh=10;url=https://evil.example", overviewView{Sort: "name"}},
		{"q=++logs-++", overviewView{Sort: "name", Q: "logs-"}},
	}
	for _, tt := range tests {
		q, _ := url.ParseQuery(tt.query)
		if got := parseOverviewView(q); got != tt.want {
			t.Errorf("%q: got %+v, want %+v", tt.query, got, tt.want)
		}
	}

	long, _ := url.ParseQuery("q=" + strings.Repeat("ż", 500))
	if got := parseOverviewView(long).Q; len([]rune(got)) != maxFilterLen {
		t.Errorf("filter not capped: %d runes", len([]rune(got)))
	}
}

// Links rebuild state from the parsed view, so every param survives a click
// and nothing outside the allowlist leaks back into an href.
func TestOverviewViewLinks(t *testing.T) {
	v := overviewView{Sort: "cpu", Desc: true, Refresh: 10, Q: "a&b"}

	h := v.Header("cpu", "cpu")
	if h.Aria != "descending" || !strings.Contains(h.Href, "dir=asc") {
		t.Errorf("active column must flip: %+v", h)
	}
	h = v.Header("name", "node")
	if h.Aria != "none" || strings.Contains(h.Href, "sort=") { // name A–Z is the default, so no sort param
		t.Errorf("other column must start at its default: %+v", h)
	}
	for _, link := range []string{h.Href, v.RefreshEvery(60), v.ClearFilter()} {
		q, err := url.ParseQuery(strings.TrimPrefix(link, "?"))
		if err != nil {
			t.Fatal(err)
		}
		if link != v.ClearFilter() && q.Get("q") != "a&b" {
			t.Errorf("%s dropped the filter", link)
		}
	}
	if got := v.RefreshEvery(0); strings.Contains(got, "refresh") {
		t.Errorf("refresh off still in link: %s", got)
	}
}

func TestSortNodes(t *testing.T) {
	nodes := []es.NodeInfo{
		{Name: "c", IP: "10.0.0.10", CPUPercent: 5, Load: "0.50"},
		{Name: "a", IP: "10.0.0.2", CPUPercent: 90, Load: ""},
		{Name: "b", IP: "10.0.0.3", CPUPercent: 5, Load: "2.25"},
	}
	names := func() []string {
		var out []string
		for _, n := range nodes {
			out = append(out, n.Name)
		}
		return out
	}
	for _, tt := range []struct {
		view overviewView
		want []string
	}{
		{overviewView{Sort: "name"}, []string{"a", "b", "c"}},
		{overviewView{Sort: "ip"}, []string{"a", "b", "c"}},               // numeric, not 10.0.0.10 < 10.0.0.2
		{overviewView{Sort: "cpu", Desc: true}, []string{"a", "b", "c"}},  // ties stay A–Z
		{overviewView{Sort: "load", Desc: true}, []string{"b", "c", "a"}}, // missing load last
		{overviewView{Sort: "shards", Desc: true}, []string{"c", "a", "b"}},
	} {
		sortNodes(nodes, tt.view, map[string]int{"c": 7, "a": 1})
		if got := names(); !slices.Equal(got, tt.want) {
			t.Errorf("%+v: got %v, want %v", tt.view, got, tt.want)
		}
	}
}

func TestBuildMatrix(t *testing.T) {
	nodes := []es.NodeInfo{{Name: "es02"}, {Name: "es01"}, {Name: "master-only"}}
	shards := []es.ShardInfo{
		{Index: "ok", Shard: "10", PriRep: "p", State: "STARTED", Node: "es01"},
		{Index: "ok", Shard: "2", PriRep: "p", State: "STARTED", Node: "es01"},
		{Index: "ok", Shard: "2", PriRep: "r", State: "RELOCATING", Node: "es02 -> 10.0.0.1 abc es01"},
		{Index: "half", Shard: "0", PriRep: "p", State: "STARTED", Node: "es02"},
		{Index: "half", Shard: "0", PriRep: "r", State: "UNASSIGNED"},
		{Index: "dead", Shard: "0", PriRep: "p", State: "UNASSIGNED"},
	}
	m := buildMatrix(nodes, shards, "")

	if want := []string{"es01", "es02", "master-only", "unassigned"}; !slices.Equal(m.Nodes, want) {
		t.Fatalf("columns = %v, want %v", m.Nodes, want)
	}
	var order []string
	for _, r := range m.Rows {
		order = append(order, r.Index+":"+r.Health)
	}
	if want := []string{"dead:red", "half:yellow", "ok:green"}; !slices.Equal(order, want) {
		t.Errorf("rows = %v, want unhealthy first %v", order, want)
	}

	ok := m.Rows[2]
	if got := []string{ok.Cells[0][0].Shard, ok.Cells[0][1].Shard}; !slices.Equal(got, []string{"2", "10"}) {
		t.Errorf("es01 shards = %v, want numeric order", got)
	}
	if len(ok.Cells[1]) != 1 || ok.Cells[1][0].State != "RELOCATING" {
		t.Errorf("relocating shard must sit on its source node es02: %v", ok.Cells)
	}

	f := buildMatrix(nodes, shards, "OK")
	if len(f.Rows) != 1 || f.Total != 3 || slices.Contains(f.Nodes, "unassigned") {
		t.Errorf("filtered: rows=%d total=%d cols=%v", len(f.Rows), f.Total, f.Nodes)
	}
}

func TestOverviewRefreshAndFilterRender(t *testing.T) {
	esrv := fakeES(t)
	s, codec := testServer(t, esrv.URL)
	cookie := sessionCookieFor(t, codec, "root", "admin")

	_, body := get(t, s, cookie, "/c/dev/overview")
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("auto-refresh must be opt-in")
	}

	_, body = get(t, s, cookie, "/c/dev/overview?refresh=10&q=nomatch")
	if !strings.Contains(body, `<meta http-equiv="refresh" content="10">`) {
		t.Error("refresh=10 not rendered as a bare meta refresh")
	}
	if strings.Contains(body, `href="/c/dev/indices/logs-1"`) || !strings.Contains(body, "0 of 1 indices") {
		t.Error("filter not applied to the shard matrix")
	}

	req := httptest.NewRequest(http.MethodGet, "/c/dev/overview?q="+url.QueryEscape(`"><script>x</script>`), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "<script>x") {
		t.Error("filter value reflected unescaped")
	}
}
