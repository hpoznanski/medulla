package web

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hpoznanski/medulla/internal/es"
	"github.com/hpoznanski/medulla/internal/rbac"
)

// probeBudget bounds the whole landing page: every cluster is probed in
// parallel under one deadline, so one dead cluster costs at most this long.
const probeBudget = 5 * time.Second

const (
	healthUnreachable = "unreachable"
	layoutCookie      = "medulla_layout"
	layoutCards       = "cards"
	layoutList        = "list"
)

var clusterSort = &sortSpec{def: "health", health: []string{healthUnreachable, "red", "yellow", "green"}, keys: map[string]bool{
	"health": true, "name": false, "version": false,
	"nodes": true, "indices": true, "docs": true, "shards": true,
	"unassigned": true, "relocating": true, "store": true, "latency": true,
}}

type clusterStatus struct {
	Name string
	// Health is green, yellow, red, or unreachable — the last covering every
	// way the health call can fail, including ES refusing our credentials.
	Health  string
	Reason  string // short cause when unreachable: timeout, refused, dns, tls, auth, http NNN, error
	Error   string // full cause, shown collapsed
	Partial string // health answered but stats or info did not

	LatencyMS  int64
	Nodes      int
	Shards     int
	Unassigned int
	Relocating int

	HasStats   bool
	Indices    int
	Docs       int64
	StoreBytes int64
	Store      string

	HasInfo bool
	Flavor  string
	Version string

	CanConsole bool
}

func (c clusterStatus) Unreachable() bool { return c.Health == healthUnreachable }

type homeData struct {
	pageData
	Statuses []clusterStatus // filtered and sorted
	Counts   map[string]int  // per health, over every visible cluster
	Total    int
	View     listView
	Layout   string
	Updated  string
	Budget   string
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	visible := s.rbac.Clusters(sess.Roles, s.clusters.Names())
	if len(visible) == 0 {
		http.Error(w, "no clusters visible for your roles", http.StatusForbidden)
		return
	}
	view := parseListView(r.URL.Query(), clusterSort)

	ctx, cancel := context.WithTimeout(r.Context(), probeBudget)
	defer cancel()

	// One goroutine per visible cluster, each writing its own slot: bounded by
	// the config, and every one is joined before render.
	all := make([]clusterStatus, len(visible))
	var wg sync.WaitGroup
	for i, name := range visible {
		wg.Go(func() {
			client, err := s.clusters.Get(name)
			if err != nil {
				all[i] = clusterStatus{Name: name, Health: healthUnreachable, Reason: "error", Error: err.Error()}
				return
			}
			all[i] = probeCluster(ctx, client)
			all[i].Name = name
		})
	}
	wg.Wait()

	counts := map[string]int{}
	shown := make([]clusterStatus, 0, len(all))
	needle := strings.ToLower(view.Q)
	for _, st := range all {
		counts[st.Health]++
		if view.Health != "" && st.Health != view.Health {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(st.Name), needle) {
			continue
		}
		st.CanConsole = s.rbac.Allowed(sess.Roles, st.Name, rbac.RestGet)
		shown = append(shown, st)
	}
	sortClusters(shown, view)

	s.render(w, "clusters.html", homeData{
		pageData: pageData{User: sess.User, Roles: sess.Roles, Clusters: visible},
		Statuses: shown,
		Counts:   counts,
		Total:    len(all),
		View:     view,
		Layout:   s.homeLayout(w, r),
		Updated:  time.Now().UTC().Format("15:04:05 UTC"),
		Budget:   probeBudget.String(),
	})
}

// homeLayout picks cards or list. An explicit ?layout= wins and is remembered
// in a preference cookie, so the choice survives clicking back to the
// landing page. Only the two literal values are ever accepted or stored.
func (s *Server) homeLayout(w http.ResponseWriter, r *http.Request) string {
	valid := func(l string) bool { return l == layoutCards || l == layoutList }
	if l := r.URL.Query().Get("layout"); valid(l) {
		http.SetCookie(w, &http.Cookie{
			Name:     layoutCookie,
			Value:    l,
			Path:     "/",
			HttpOnly: true,
			Secure:   !s.cfg.Session.InsecureCookie,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   365 * 24 * 3600,
		})
		return l
	}
	if c, err := r.Cookie(layoutCookie); err == nil && valid(c.Value) {
		return c.Value
	}
	return layoutCards
}

// probeCluster reads health (required), then stats and info (optional: a
// failure there marks the cluster partial instead of hiding what we have).
func probeCluster(ctx context.Context, client *es.Client) clusterStatus {
	var st clusterStatus
	resp, err := client.GetCached(ctx, "/_cluster/health")
	if err != nil {
		st.Health, st.Reason, st.Error = healthUnreachable, unreachableReason(err), err.Error()
		return st
	}
	st.LatencyMS = resp.Took.Milliseconds()
	if !resp.OK() {
		st.Health, st.Reason = healthUnreachable, httpReason(resp.Status)
		st.Error = "/_cluster/health: " + resp.ErrorReason()
		return st
	}
	var health es.HealthInfo
	if err := json.Unmarshal(resp.Body, &health); err != nil || health.Status == "" {
		st.Health, st.Reason = healthUnreachable, "error"
		st.Error = "/_cluster/health: unexpected response"
		return st
	}
	st.Health = health.Status
	st.Nodes = health.NumberOfNodes
	st.Shards = health.ActiveShards
	st.Unassigned = health.UnassignedShards
	st.Relocating = health.RelocatingShards

	var partial []string
	var stats struct {
		Indices struct {
			Count int `json:"count"`
			Docs  struct {
				Count int64 `json:"count"`
			} `json:"docs"`
			Store struct {
				SizeInBytes int64 `json:"size_in_bytes"`
			} `json:"store"`
		} `json:"indices"`
	}
	if err := client.GetJSONCached(ctx, "/_cluster/stats", &stats); err != nil {
		partial = append(partial, err.Error())
	} else {
		st.HasStats = true
		st.Indices = stats.Indices.Count
		st.Docs = stats.Indices.Docs.Count
		st.StoreBytes = stats.Indices.Store.SizeInBytes
		st.Store = es.HumanBytes(st.StoreBytes)
	}
	if info, err := client.Info(ctx); err != nil {
		partial = append(partial, err.Error())
	} else {
		st.HasInfo = true
		st.Flavor = string(info.Flavor)
		st.Version = info.Version
	}
	st.Partial = strings.Join(partial, "; ")
	return st
}

// unreachableReason turns a transport error into a short operator-facing
// cause; the full error is still shown alongside it.
func unreachableReason(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	var recordErr tls.RecordHeaderError
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &dnsErr):
		return "dns"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.As(err, &certErr), errors.As(err, &unknownCA), errors.As(err, &hostErr),
		errors.As(err, &invalidCert), errors.As(err, &recordErr):
		return "tls"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	}
	return "error"
}

func httpReason(status int) string {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "auth"
	}
	return fmt.Sprintf("http %d", status)
}

var healthRank = map[string]int{healthUnreachable: 3, "red": 2, "yellow": 1, "green": 0}

// hasValue reports whether a cluster has data for a sort column. Rows
// without it sort last in either direction, so a dash never interleaves
// with real numbers.
func (c clusterStatus) hasValue(key string) bool {
	switch key {
	case "health", "name":
		return true
	case "indices", "docs", "store":
		return c.HasStats
	case "version":
		return c.HasInfo
	}
	return !c.Unreachable()
}

func sortClusters(list []clusterStatus, v listView) {
	slices.SortStableFunc(list, func(a, b clusterStatus) int {
		ha, hb := a.hasValue(v.Sort), b.hasValue(v.Sort)
		if ha != hb {
			if ha {
				return -1
			}
			return 1
		}
		var c int
		switch v.Sort {
		case "health":
			c = cmp.Compare(healthRank[a.Health], healthRank[b.Health])
		case "version":
			c = cmp.Or(strings.Compare(a.Flavor, b.Flavor), compareVersion(a.Version, b.Version))
		case "nodes":
			c = cmp.Compare(a.Nodes, b.Nodes)
		case "indices":
			c = cmp.Compare(a.Indices, b.Indices)
		case "docs":
			c = cmp.Compare(a.Docs, b.Docs)
		case "shards":
			c = cmp.Compare(a.Shards, b.Shards)
		case "unassigned":
			c = cmp.Compare(a.Unassigned, b.Unassigned)
		case "relocating":
			c = cmp.Compare(a.Relocating, b.Relocating)
		case "store":
			c = cmp.Compare(a.StoreBytes, b.StoreBytes)
		case "latency":
			c = cmp.Compare(a.LatencyMS, b.LatencyMS)
		}
		if v.Desc {
			c = -c
		}
		return cmp.Or(c, strings.Compare(a.Name, b.Name))
	})
}

// compareVersion orders dotted versions numerically (8.9 before 8.13).
func compareVersion(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			fmt.Sscan(pa[i], &x)
		}
		if i < len(pb) {
			fmt.Sscan(pb[i], &y)
		}
		if c := cmp.Compare(x, y); c != 0 {
			return c
		}
	}
	return 0
}
