package web

import (
	"cmp"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hpoznanski/medulla/internal/es"
)

// refreshIntervals are the only auto-refresh periods offered, in seconds;
// 0 is off. Each refresh re-runs every ES call behind the page, so there is
// a floor and no free-form value.
var refreshIntervals = []int{0, 10, 30, 60}

const maxFilterLen = 100

// sortSpec lists a table's sortable columns, each mapped to whether it sorts
// descending by default (metrics open worst-first, identifiers A–Z), plus
// the column the page sorts by when the URL names none.
type sortSpec struct {
	def  string
	keys map[string]bool
	// health enables the landing page's ?health= filter.
	health bool
}

var nodeSort = &sortSpec{def: "name", keys: map[string]bool{
	"name": false, "ip": false,
	"heap": true, "disk": true, "cpu": true, "load": true, "shards": true,
}}

// healthFilters are the values ?health= accepts on the landing page.
var healthFilters = []string{"unreachable", "red", "yellow", "green"}

// listView is a page's URL state. Every field is parsed against an
// allowlist, and every link the page emits is rebuilt from it — never from
// the raw query — so no user-supplied string reaches an href or the meta
// refresh except the name filter, which html/template escapes as a value.
type listView struct {
	spec    *sortSpec
	Sort    string
	Desc    bool
	Refresh int
	Q       string
	Health  string
}

func parseListView(q url.Values, spec *sortSpec) listView {
	v := listView{spec: spec, Sort: spec.def}
	if _, ok := spec.keys[q.Get("sort")]; ok {
		v.Sort = q.Get("sort")
	}
	switch q.Get("dir") {
	case "asc":
	case "desc":
		v.Desc = true
	default:
		v.Desc = spec.keys[v.Sort]
	}
	if n, err := strconv.Atoi(q.Get("refresh")); err == nil && slices.Contains(refreshIntervals, n) {
		v.Refresh = n
	}
	v.Q = strings.TrimSpace(q.Get("q"))
	if utf8.RuneCountInString(v.Q) > maxFilterLen {
		v.Q = string([]rune(v.Q)[:maxFilterLen])
	}
	if spec.health && slices.Contains(healthFilters, q.Get("health")) {
		v.Health = q.Get("health")
	}
	return v
}

func (v listView) Dir() string {
	if v.Desc {
		return "desc"
	}
	return "asc"
}

// SortIsDefault reports whether sort and dir can be left out of a link.
func (v listView) SortIsDefault() bool {
	return v.Sort == v.spec.def && v.Desc == v.spec.keys[v.spec.def]
}

func (v listView) query() string {
	q := url.Values{}
	if !v.SortIsDefault() {
		q.Set("sort", v.Sort)
		q.Set("dir", v.Dir())
	}
	if v.Refresh > 0 {
		q.Set("refresh", strconv.Itoa(v.Refresh))
	}
	if v.Q != "" {
		q.Set("q", v.Q)
	}
	if v.Health != "" {
		q.Set("health", v.Health)
	}
	return "?" + q.Encode()
}

// RefreshEvery links to the same view auto-refreshing every n seconds.
func (v listView) RefreshEvery(n int) string {
	v.Refresh = n
	return v.query()
}

// ClearFilter links to the same view without the name filter.
func (v listView) ClearFilter() string {
	v.Q = ""
	return v.query()
}

// HealthLink links to the same view showing only clusters in health h
// ("" for all).
func (v listView) HealthLink(h string) string {
	v.Health = h
	return v.query()
}

// LayoutLink links to the same view in another landing-page layout.
func (v listView) LayoutLink(layout string) string {
	q := v.query()
	if q != "?" {
		q += "&"
	}
	return q + "layout=" + url.QueryEscape(layout)
}

// RefreshIntervals is exposed for the shared refresh bar.
func (listView) RefreshIntervals() []int { return refreshIntervals }

type sortHeader struct {
	Label, Href, Class, Aria string
}

// Header describes a sortable column heading: clicking the active column
// flips direction, any other column starts at its default direction.
func (v listView) Header(key, label string) sortHeader {
	h := sortHeader{Label: label, Aria: "none"}
	next := v
	next.Sort = key
	if v.Sort == key {
		next.Desc = !v.Desc
		h.Class = "sorted " + v.Dir()
		h.Aria = v.Dir() + "ending"
	} else {
		next.Desc = v.spec.keys[key]
	}
	h.Href = next.query()
	return h
}

// sortNodes orders the Nodes table in place; ties fall back to name A–Z so
// equal rows don't shuffle between refreshes.
func sortNodes(nodes []es.NodeInfo, v listView, shards map[string]int) {
	slices.SortStableFunc(nodes, func(a, b es.NodeInfo) int {
		var c int
		switch v.Sort {
		case "ip":
			c = compareIP(a.IP, b.IP)
		case "heap":
			c = cmp.Compare(a.HeapPercent, b.HeapPercent)
		case "disk":
			c = cmp.Compare(a.DiskPercent, b.DiskPercent)
		case "cpu":
			c = cmp.Compare(a.CPUPercent, b.CPUPercent)
		case "load":
			c = cmp.Compare(parseLoad(a.Load), parseLoad(b.Load))
		case "shards":
			c = cmp.Compare(shards[a.Name], shards[b.Name])
		default:
			c = strings.Compare(a.Name, b.Name)
		}
		if v.Desc {
			c = -c
		}
		if c == 0 {
			c = strings.Compare(a.Name, b.Name)
		}
		return c
	})
}

// compareIP orders addresses numerically (10.0.0.2 before 10.0.0.10),
// falling back to text for anything that does not parse.
func compareIP(a, b string) int {
	pa, errA := netip.ParseAddr(a)
	pb, errB := netip.ParseAddr(b)
	if errA != nil || errB != nil {
		return strings.Compare(a, b)
	}
	return pa.Compare(pb)
}

// parseLoad reads load_1m; a missing value (Windows nodes report none)
// sorts below every real load.
func parseLoad(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return -1
	}
	return f
}

// shardNode is the node a shard currently occupies. _cat/shards reports a
// relocating shard as "src -> ip id dst"; it stays on src until the move
// completes, so that is where it counts.
func shardNode(sh es.ShardInfo) string {
	node, _, _ := strings.Cut(sh.Node, " -> ")
	return node
}

const unassignedCol = "unassigned"

// shardMatrix is the shard allocation grid: one row per index, one column
// per node, plus an unassigned column when a shown index has any.
type shardMatrix struct {
	Nodes []string
	Rows  []indexRow
	Total int // indices before filtering
}

type indexRow struct {
	Index  string
	Health string // red: unassigned primary, yellow: unassigned replica
	Cells  [][]es.ShardInfo
}

func buildMatrix(nodes []es.NodeInfo, shards []es.ShardInfo, filter string) shardMatrix {
	byIndex := map[string][]es.ShardInfo{}
	onNode := map[string]bool{}
	for _, n := range nodes {
		onNode[n.Name] = true
	}
	for _, sh := range shards {
		byIndex[sh.Index] = append(byIndex[sh.Index], sh)
		if sh.State != "UNASSIGNED" && shardNode(sh) != "" {
			onNode[shardNode(sh)] = true
		}
	}

	m := shardMatrix{Total: len(byIndex)}
	needle := strings.ToLower(filter)
	unassigned := false
	for index, list := range byIndex {
		if needle != "" && !strings.Contains(strings.ToLower(index), needle) {
			continue
		}
		row := indexRow{Index: index, Health: "green"}
		for _, sh := range list {
			if sh.State != "UNASSIGNED" {
				continue
			}
			unassigned = true
			if sh.PriRep == "p" {
				row.Health = "red"
			} else if row.Health == "green" {
				row.Health = "yellow"
			}
		}
		m.Rows = append(m.Rows, row)
	}

	for n := range onNode {
		m.Nodes = append(m.Nodes, n)
	}
	slices.Sort(m.Nodes)
	if unassigned {
		m.Nodes = append(m.Nodes, unassignedCol)
	}
	col := make(map[string]int, len(m.Nodes))
	for i, n := range m.Nodes {
		col[n] = i
	}

	rank := map[string]int{"red": 0, "yellow": 1, "green": 2}
	slices.SortFunc(m.Rows, func(a, b indexRow) int {
		return cmp.Or(cmp.Compare(rank[a.Health], rank[b.Health]), strings.Compare(a.Index, b.Index))
	})
	for i := range m.Rows {
		row := &m.Rows[i]
		row.Cells = make([][]es.ShardInfo, len(m.Nodes))
		list := byIndex[row.Index]
		slices.SortStableFunc(list, func(a, b es.ShardInfo) int {
			na, _ := strconv.Atoi(a.Shard)
			nb, _ := strconv.Atoi(b.Shard)
			return cmp.Or(cmp.Compare(na, nb), strings.Compare(a.PriRep, b.PriRep))
		})
		for _, sh := range list {
			node := shardNode(sh)
			if sh.State == "UNASSIGNED" {
				node = unassignedCol
			}
			if c, ok := col[node]; ok {
				row.Cells[c] = append(row.Cells[c], sh)
			}
		}
	}
	return m
}
