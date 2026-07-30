package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/hpoznanski/medulla/internal/auth"
	"github.com/hpoznanski/medulla/internal/es"
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

	statuses := make([]clusterStatus, len(visible))
	var wg sync.WaitGroup
	for i, name := range visible {
		wg.Add(1)
		go func() {
			defer wg.Done()
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
		}()
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

	s.render(w, "overview.html", overviewData{
		pageData:   page,
		Overview:   overview,
		Info:       info,
		NodeShards: groupShards(overview.Shards),
		Explains:   explains,
		ExplainCap: capped,
	})
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
