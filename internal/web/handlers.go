package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/hpoznanski/medulla/internal/auth"
	"github.com/hpoznanski/medulla/internal/es"
	"github.com/hpoznanski/medulla/internal/rbac"
)

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("render failed", "template", name, "err", err)
	}
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", nil)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	username := r.PostFormValue("username")
	// Keyed on username, not IP: works identically with or without proxies and
	// trusted_proxies config, and cannot be reset by rotating X-Forwarded-For.
	if !s.loginRate.Allow(username) {
		s.logger.Warn("audit", "type", "audit", "event", "login", "outcome", "rate_limited", "ip", ip, "user", username)
		http.Error(w, "too many attempts, retry later", http.StatusTooManyRequests)
		return
	}

	roles, err := s.auth.Login(username, r.PostFormValue("password"))
	if err != nil {
		outcome := "bad_credentials"
		if !errors.Is(err, auth.ErrBadCredentials) {
			outcome = "error"
			s.logger.Error("login failed", "err", err)
		}
		s.logger.Warn("audit", "type", "audit", "event", "login", "outcome", outcome, "user", username, "ip", ip)
		s.render(w, "login.html", map[string]any{"Error": "Invalid username or password."})
		return
	}

	token, err := s.sessions.Encode(username, roles)
	if err != nil {
		s.logger.Error("session encode failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setSession(w, token)
	s.logger.Info("audit", "type", "audit", "event", "login", "outcome", "success",
		"user", username, "roles", roles, "ip", ip)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type clusterStatus struct {
	Name       string
	Error      string
	Status     string
	Nodes      int
	Indices    int
	Docs       int64
	Shards     int
	Unassigned int
	Flavor     string
	Version    string
}

type homeData struct {
	pageData
	Statuses []clusterStatus
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	visible := s.rbac.Clusters(sess.Roles, s.clusters.Names())
	if len(visible) == 0 {
		http.Error(w, "no clusters visible for your roles", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// One goroutine per visible cluster, each writing its own slot: bounded by
	// the config, and every one is joined before render.
	statuses := make([]clusterStatus, len(visible))
	var wg sync.WaitGroup
	for i, name := range visible {
		wg.Go(func() {
			st := clusterStatus{Name: name}
			client, err := s.clusters.Get(name)
			if err != nil {
				st.Error = err.Error()
				statuses[i] = st
				return
			}
			var health es.HealthInfo
			if err := client.GetJSON(ctx, "/_cluster/health", &health); err != nil {
				st.Error = err.Error()
				statuses[i] = st
				return
			}
			st.Status = health.Status
			st.Nodes = health.NumberOfNodes
			st.Shards = health.ActiveShards
			st.Unassigned = health.UnassignedShards
			var stats struct {
				Indices struct {
					Count int `json:"count"`
					Docs  struct {
						Count int64 `json:"count"`
					} `json:"docs"`
				} `json:"indices"`
			}
			if err := client.GetJSON(ctx, "/_cluster/stats", &stats); err == nil {
				st.Indices = stats.Indices.Count
				st.Docs = stats.Indices.Docs.Count
			}
			if info, err := client.Info(ctx); err == nil {
				st.Flavor = string(info.Flavor)
				st.Version = info.Version
			}
			statuses[i] = st
		})
	}
	wg.Wait()

	s.render(w, "clusters.html", homeData{
		pageData: pageData{User: sess.User, Roles: sess.Roles, Clusters: visible},
		Statuses: statuses,
	})
}

type pageData struct {
	User       string
	Roles      []string
	Cluster    string
	Clusters   []string
	Nav        string
	CanWrite   bool
	CanConsole bool
	Error      string
}

type overviewData struct {
	pageData
	Overview   *es.Overview
	Info       *es.Info
	NodeShards []nodeShards
	Explains   []explainGroup
	ExplainCap int
	Routing    *es.Routing
	Excluded   map[string]bool // node name -> excluded from allocation
	ShardCount map[string]int  // node name -> shards still held
	CanCluster bool
	Notice     string

	AllocationValues []string
	RebalanceValues  []string
}

// explainGroup collects unassigned shards sharing one root cause.
type explainGroup struct {
	Shards      []string
	Explanation string
	RawJSON     string
}

type nodeShards struct {
	Node   string
	Shards []es.ShardInfo
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	cluster := r.PathValue("cluster")
	client := clientFrom(r)
	page := s.page(r, "overview")

	overview, err := client.Overview(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "overview failed", "cluster", cluster, "err", err)
		page.Error = err.Error()
		s.render(w, "overview.html", overviewData{pageData: page})
		return
	}
	info, err := client.Info(r.Context())
	if err != nil {
		info = &es.Info{Flavor: es.FlavorUnknown}
	}

	explains, capped := s.explainUnassigned(r.Context(), client, overview.Shards)

	// Routing state only drives the controls and the warning banner, so a
	// failure to read it must not cost the operator the whole overview.
	routing, err := client.Routing(r.Context())
	if err != nil {
		s.logger.WarnContext(r.Context(), "reading routing state failed", "cluster", cluster, "err", err)
		routing = &es.Routing{Unknown: true}
	}
	excluded := make(map[string]bool, len(routing.ExcludedNodes))
	for _, n := range routing.ExcludedNodes {
		excluded[n] = true
	}
	// An excluded node is only *drained* once it holds nothing — that is the
	// point at which it is safe to stop, so the two states must look different.
	shardCount := map[string]int{}
	for _, sh := range overview.Shards {
		if sh.Node != "" && sh.State != "UNASSIGNED" {
			shardCount[sh.Node]++
		}
	}

	s.render(w, "overview.html", overviewData{
		pageData:   page,
		Overview:   overview,
		Info:       info,
		NodeShards: groupShards(overview.Shards),
		Explains:   explains,
		ExplainCap: capped,
		Routing:    routing,
		Excluded:   excluded,
		ShardCount: shardCount,
		CanCluster: s.rbac.Allowed(sess.Roles, cluster, rbac.ClusterWrite),
		Notice:     r.URL.Query().Get("notice"),

		AllocationValues: es.AllocationEnableValues,
		RebalanceValues:  es.RebalanceEnableValues,
	})
}

// handleRoutingPut sets cluster-wide allocation or rebalance enablement.
func (s *Server) handleRoutingPut(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("cluster")
	client := clientFrom(r)

	kind, value := r.PostFormValue("kind"), r.PostFormValue("value")
	err := client.RoutingEnablePut(r.Context(), kind, value)
	s.redirectNotice(w, r, "/c/"+cluster+"/overview", kind+".enable = "+value, err)
}

// handleNodeExclude drains a node by adding it to the allocation exclusion
// list, or puts it back. Excluding moves every shard off the node, so it
// takes the same type-the-name confirmation as an index delete.
func (s *Server) handleNodeExclude(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("cluster")
	client := clientFrom(r)

	node := r.PostFormValue("node")
	exclude := r.PostFormValue("action") == "exclude"
	if exclude && r.PostFormValue("confirm") != node {
		http.Redirect(w, r, "/c/"+cluster+"/overview?notice="+
			url.QueryEscape("type the node name to confirm draining "+node), http.StatusSeeOther)
		return
	}

	action := "re-include " + node
	if exclude {
		action = "exclude " + node + " (draining shards)"
	}
	err := client.ExcludeNode(r.Context(), node, exclude)
	s.redirectNotice(w, r, "/c/"+cluster+"/overview", action, err)
}

// maxExplains bounds allocation-explain calls per overview render.
// ponytail: 8 sequential calls worst case; batch endpoint doesn't exist.
const maxExplains = 8

// explainUnassigned runs allocation-explain for each unassigned shard (up to
// maxExplains) and groups shards that share the same root cause. Returns the
// number of unassigned shards left unexplained by the cap.
func (s *Server) explainUnassigned(ctx context.Context, client *es.Client, shards []es.ShardInfo) ([]explainGroup, int) {
	var unassigned []es.ShardInfo
	for _, sh := range shards {
		if sh.State == "UNASSIGNED" {
			unassigned = append(unassigned, sh)
		}
	}
	if len(unassigned) == 0 {
		return nil, 0
	}

	capped := 0
	if len(unassigned) > maxExplains {
		capped = len(unassigned) - maxExplains
		unassigned = unassigned[:maxExplains]
	}

	byCause := map[string]*explainGroup{}
	var order []string
	for _, sh := range unassigned {
		shardNum, err := strconv.Atoi(sh.Shard)
		if err != nil {
			continue
		}
		kind := "replica"
		if sh.PriRep == "p" {
			kind = "primary"
		}
		label := fmt.Sprintf("%s[%s] %s", sh.Index, sh.Shard, kind)

		ex, err := client.AllocationExplainFor(ctx, sh.Index, shardNum, sh.PriRep == "p")
		if err != nil {
			s.logger.Warn("allocation explain failed", "shard", label, "err", err)
			continue
		}
		g, ok := byCause[ex.Explanation]
		if !ok {
			g = &explainGroup{Explanation: ex.Explanation, RawJSON: ex.RawJSON}
			byCause[ex.Explanation] = g
			order = append(order, ex.Explanation)
		}
		g.Shards = append(g.Shards, label)
	}

	out := make([]explainGroup, 0, len(order))
	for _, cause := range order {
		out = append(out, *byCause[cause])
	}
	return out, capped
}

// groupShards buckets shards per node; unassigned shards get their own bucket.
func groupShards(shards []es.ShardInfo) []nodeShards {
	byNode := map[string][]es.ShardInfo{}
	for _, sh := range shards {
		node := sh.Node
		if sh.State == "UNASSIGNED" {
			node = "unassigned"
		}
		byNode[node] = append(byNode[node], sh)
	}
	names := make([]string, 0, len(byNode))
	for n := range byNode {
		if n != "unassigned" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if _, ok := byNode["unassigned"]; ok {
		names = append(names, "unassigned")
	}

	out := make([]nodeShards, 0, len(names))
	for _, n := range names {
		out = append(out, nodeShards{Node: n, Shards: byNode[n]})
	}
	return out
}

var _ http.Handler = (*Server)(nil)
