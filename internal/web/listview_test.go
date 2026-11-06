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
		want  listView
	}{
		{"", listView{Sort: "name"}},
		{"sort=cpu", listView{Sort: "cpu", Desc: true}},
		{"sort=cpu&dir=asc", listView{Sort: "cpu"}},
		{"sort=name&dir=desc", listView{Sort: "name", Desc: true}},
		{"sort=__proto__&dir=sideways", listView{Sort: "name"}},
		{"refresh=30", listView{Sort: "name", Refresh: 30}},
		{"refresh=1", listView{Sort: "name"}},
		{"refresh=-10", listView{Sort: "name"}},
		{"refresh=10;url=https://evil.example", listView{Sort: "name"}},
		{"q=++logs-++", listView{Sort: "name", Q: "logs-"}},
	}
	for _, tt := range tests {
		q, _ := url.ParseQuery(tt.query)
		got := parseListView(q, nodeSort)
		got.spec = nil
		if got != tt.want {
			t.Errorf("%q: got %+v, want %+v", tt.query, got, tt.want)
		}
	}

	long, _ := url.ParseQuery("q=" + strings.Repeat("ż", 500))
	if got := parseListView(long, nodeSort).Q; len([]rune(got)) != maxFilterLen {
		t.Errorf("filter not capped: %d runes", len([]rune(got)))
	}
}

// Links rebuild state from the parsed view, so every param survives a click
// and nothing outside the allowlist leaks back into an href.
func TestOverviewViewLinks(t *testing.T) {
	v := listView{spec: nodeSort, Sort: "cpu", Desc: true, Refresh: 10, Q: "a&b"}

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
		view listView
		want []string
	}{
		{listView{Sort: "name"}, []string{"a", "b", "c"}},
		{listView{Sort: "ip"}, []string{"a", "b", "c"}},               // numeric, not 10.0.0.10 < 10.0.0.2
		{listView{Sort: "cpu", Desc: true}, []string{"a", "b", "c"}},  // ties stay A–Z
		{listView{Sort: "load", Desc: true}, []string{"b", "c", "a"}}, // missing load last
		{listView{Sort: "shards", Desc: true}, []string{"c", "a", "b"}},
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

func TestHumanCount(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0", 999: "999", 1000: "1k", 1260: "1.3k", 999_949: "999.9k",
		999_950: "1M", 12_345_678: "12.3M", 3_000_000_000: "3B",
	} {
		if got := humanCount(n); got != want {
			t.Errorf("humanCount(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestSortRowsByValue(t *testing.T) {
	rows := []string{"10kb", "", "1.5gb", "900b", "2mb"}
	sortRows(rows, false, func(s string) string { return s })
	if want := []string{"900b", "10kb", "2mb", "1.5gb", ""}; !slices.Equal(rows, want) {
		t.Errorf("sizes asc = %v, want %v", rows, want)
	}
	sortRows(rows, true, func(s string) string { return s })
	if want := []string{"1.5gb", "2mb", "10kb", "900b", ""}; !slices.Equal(rows, want) {
		t.Errorf("sizes desc = %v, want %v (empty stays last)", rows, want)
	}

	nums := []string{"10", "9", "100", "45%", "abc"}
	sortRows(nums, false, func(s string) string { return s })
	if want := []string{"9", "10", "45%", "100", "abc"}; !slices.Equal(nums, want) {
		t.Errorf("numbers asc = %v, want %v", nums, want)
	}
}

func TestNodeVersions(t *testing.T) {
	nodes := []es.NodeInfo{
		{Name: "a", Version: "8.19.7"}, {Name: "b", Version: "9.2.1"},
		{Name: "c", Version: "9.2.1"}, {Name: "d", Version: "8.9.0"}, {Name: "e"},
	}
	got := nodeVersions(nodes)
	want := []versionCount{{"9.2.1", 2}, {"8.19.7", 1}, {"8.9.0", 1}}
	if !slices.Equal(got, want) {
		t.Errorf("nodeVersions = %v, want %v (newest first, numeric)", got, want)
	}

	sortNodes(nodes, listView{Sort: "version"}, nil)
	var order []string
	for _, n := range nodes {
		order = append(order, n.Name)
	}
	if want := []string{"e", "d", "a", "b", "c"}; !slices.Equal(order, want) {
		t.Errorf("version asc = %v, want %v (8.9 before 8.19)", order, want)
	}
}

func TestOverviewMixedVersions(t *testing.T) {
	nodesJSON := `[{"name":"es01","version":"9.2.1"},{"name":"es02","version":"8.19.7"},{"name":"es03","version":"9.2.1"}]`
	esrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_cluster/health":
			w.Write([]byte(`{"status":"green","number_of_nodes":3}`))
		case "/_cat/nodes":
			w.Write([]byte(nodesJSON))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(esrv.Close)
	s, codec := testServer(t, esrv.URL)
	root := sessionCookieFor(t, codec, "root", "admin")

	_, body := get(t, s, root, "/c/dev/overview")
	if !strings.Contains(body, "Mixed versions:") || !strings.Contains(body, "<code>9.2.1</code> × 2") {
		t.Error("mixed-version banner missing or miscounted")
	}
	if !strings.Contains(body, `<td class="warn-text">8.19.7<span class="sr-only">, older than 9.2.1</span></td>`) {
		t.Error("older node not flagged")
	}
	if strings.Contains(body, `<td class="warn-text">9.2.1`) {
		t.Error("newest node flagged as older")
	}
	if !strings.Contains(body, `<span class="v warn-text">8.19.7 → 9.2.1</span>`) {
		t.Error("version stat shows one node's version instead of the spread")
	}
}

func TestOverviewUniformVersionNoBanner(t *testing.T) {
	esrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_cluster/health":
			w.Write([]byte(`{"status":"green","number_of_nodes":2}`))
		case "/_cat/nodes":
			w.Write([]byte(`[{"name":"es01","version":"9.2.1"},{"name":"es02","version":"9.2.1"}]`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(esrv.Close)
	s, codec := testServer(t, esrv.URL)
	_, body := get(t, s, sessionCookieFor(t, codec, "root", "admin"), "/c/dev/overview")
	if strings.Contains(body, "Mixed versions") || strings.Contains(body, "warn-text\">9.2.1") {
		t.Error("uniform cluster shows a mixed-version warning")
	}
	if !strings.Contains(body, "<td class=\"\">9.2.1</td>") {
		t.Error("version column missing")
	}
}
