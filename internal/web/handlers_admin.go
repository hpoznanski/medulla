package web

import (
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/hpoznanski/medulla/internal/es"
	"github.com/hpoznanski/medulla/internal/rbac"
)

// --- REST console ---

type consoleData struct {
	pageData
	Method     string
	Path       string
	Body       string
	RestFull   bool
	RespStatus int
	RespBody   string
}

var consoleMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete}

func (s *Server) handleConsolePage(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	cluster := r.PathValue("cluster")
	s.render(w, "console.html", consoleData{
		pageData: s.page(r, "console"),
		Method:   http.MethodGet,
		Path:     "/_cluster/health",
		RestFull: s.rbac.Allowed(sess.Roles, cluster, rbac.RestFull),
	})
}

func (s *Server) handleConsoleRun(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	cluster := r.PathValue("cluster")
	client := clientFrom(r)

	data := consoleData{
		pageData: s.page(r, "console"),
		Method:   r.PostFormValue("method"),
		Path:     r.PostFormValue("path"),
		Body:     r.PostFormValue("body"),
		RestFull: s.rbac.Allowed(sess.Roles, cluster, rbac.RestFull),
	}

	if !slices.Contains(consoleMethods, data.Method) {
		http.Error(w, "method not allowed", http.StatusBadRequest)
		return
	}
	readOnly := data.Method == http.MethodGet || data.Method == http.MethodHead
	if !readOnly && !data.RestFull {
		s.logger.Warn("audit", "type", "audit", "event", "console", "outcome", "denied",
			"user", sess.User, "cluster", cluster, "es_method", data.Method, "es_path", data.Path)
		http.Error(w, "rest:full permission required for write requests", http.StatusForbidden)
		return
	}

	var body io.Reader
	if data.Body != "" && !readOnly {
		body = strings.NewReader(data.Body)
	}
	outcome := "executed"
	resp, err := client.Do(r.Context(), data.Method, data.Path, body)
	if err != nil {
		data.Error = err.Error()
		outcome = "error"
	} else {
		data.RespStatus = resp.Status
		data.RespBody = string(resp.Body)
	}
	// Audit records the ES target, never the request or response body.
	s.logger.Info("audit", "type", "audit", "event", "console", "outcome", outcome,
		"user", sess.User, "cluster", cluster, "es_method", data.Method, "es_path", data.Path,
		"es_status", data.RespStatus)
	s.render(w, "console.html", data)
}

// --- analyze ---

type analyzeData struct {
	pageData
	Index    string
	Analyzer string
	Text     string
	Tokens   []es.Token
	Ran      bool
}

func (s *Server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	client := clientFrom(r)
	data := analyzeData{pageData: s.page(r, "analyze"), Analyzer: "standard"}

	if r.Method == http.MethodPost {
		data.Index = r.PostFormValue("index")
		data.Analyzer = r.PostFormValue("analyzer")
		data.Text = r.PostFormValue("text")
		data.Ran = true
		tokens, err := client.Analyze(r.Context(), data.Index, data.Analyzer, data.Text)
		if err != nil {
			data.Error = err.Error()
		}
		data.Tokens = tokens
	}
	s.render(w, "analyze.html", data)
}

// --- aliases ---

type aliasesData struct {
	pageData
	Aliases  []es.AliasInfo
	CanAlias bool
	Notice   string
}

func (s *Server) handleAliases(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	cluster := r.PathValue("cluster")
	client := clientFrom(r)
	data := aliasesData{
		pageData: s.page(r, "aliases"),
		CanAlias: s.rbac.Allowed(sess.Roles, cluster, rbac.AliasWrite),
		Notice:   r.URL.Query().Get("notice"),
	}
	aliases, err := client.Aliases(r.Context())
	if err != nil {
		data.Error = err.Error()
	}
	data.Aliases = aliases
	s.render(w, "aliases.html", data)
}

func (s *Server) handleAliasAction(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("cluster")
	client := clientFrom(r)
	action := r.PostFormValue("action")
	err := client.AliasAction(r.Context(), action, r.PostFormValue("index"), r.PostFormValue("alias"))
	s.redirectNotice(w, r, "/c/"+cluster+"/aliases", "alias "+action, err)
}

// --- templates ---

type templatesData struct {
	pageData
	Templates []es.TemplateInfo
	CanEdit   bool
	Notice    string
	Selected  string
	Detail    string
}

func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	cluster := r.PathValue("cluster")
	client := clientFrom(r)
	data := templatesData{
		pageData: s.page(r, "templates"),
		CanEdit:  s.rbac.Allowed(sess.Roles, cluster, rbac.TemplateWrite),
		Notice:   r.URL.Query().Get("notice"),
		Selected: r.URL.Query().Get("name"),
	}
	templates, err := client.Templates(r.Context())
	if err != nil {
		data.Error = err.Error()
	}
	data.Templates = templates
	if data.Selected != "" {
		detail, err := client.TemplateGet(r.Context(), data.Selected)
		if err != nil {
			data.Error = err.Error()
		}
		data.Detail = detail
	}
	s.render(w, "templates.html", data)
}

func (s *Server) handleTemplateSave(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("cluster")
	client := clientFrom(r)
	name := r.PostFormValue("name")
	err := client.TemplatePut(r.Context(), name, r.PostFormValue("body"))
	s.redirectNotice(w, r, "/c/"+cluster+"/templates", "template save "+name, err)
}

func (s *Server) handleTemplateDelete(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("cluster")
	client := clientFrom(r)
	name := r.PathValue("name")
	err := client.TemplateDelete(r.Context(), name)
	s.redirectNotice(w, r, "/c/"+cluster+"/templates", "template delete "+name, err)
}

// --- snapshots ---

type snapshotsData struct {
	pageData
	Repos     []es.RepoInfo
	Repo      string
	Snapshots []es.SnapshotInfo
	CanSnap   bool
	Notice    string
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	cluster := r.PathValue("cluster")
	client := clientFrom(r)
	data := snapshotsData{
		pageData: s.page(r, "snapshots"),
		CanSnap:  s.rbac.Allowed(sess.Roles, cluster, rbac.SnapshotWrite),
		Notice:   r.URL.Query().Get("notice"),
		Repo:     r.URL.Query().Get("repo"),
	}
	repos, err := client.Repos(r.Context())
	if err != nil {
		data.Error = err.Error()
	}
	data.Repos = repos
	if data.Repo == "" && len(repos) > 0 {
		data.Repo = repos[0].Name
	}
	if data.Repo != "" {
		snaps, err := client.Snapshots(r.Context(), data.Repo)
		if err != nil {
			data.Error = err.Error()
		}
		data.Snapshots = snaps
	}
	s.render(w, "snapshots.html", data)
}

func (s *Server) handleSnapshotAction(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("cluster")
	client := clientFrom(r)
	repo := r.PostFormValue("repo")
	name := r.PostFormValue("name")
	target := "/c/" + cluster + "/snapshots?repo=" + url.QueryEscape(repo)

	var err error
	action := r.PathValue("action")
	switch action {
	case "repo-create":
		repo = r.PostFormValue("reponame")
		err = client.RepoCreate(r.Context(), repo, r.PostFormValue("body"))
	case "create":
		err = client.SnapshotCreate(r.Context(), repo, name)
	case "restore":
		err = client.SnapshotRestore(r.Context(), repo, name)
	case "delete":
		if r.PostFormValue("confirm") != name {
			http.Redirect(w, r, target+"&notice="+url.QueryEscape("type the snapshot name to confirm deletion"), http.StatusSeeOther)
			return
		}
		err = client.SnapshotDelete(r.Context(), repo, name)
	default:
		http.NotFound(w, r)
		return
	}
	s.redirectNotice(w, r, "/c/"+cluster+"/snapshots?repo="+url.QueryEscape(repo), "snapshot "+action+" "+name, err)
}

// --- shared redirect helpers ---

func (s *Server) redirectNotice(w http.ResponseWriter, r *http.Request, base, action string, err error) {
	if err != nil {
		s.audit(r, "error", http.StatusSeeOther)
		http.Redirect(w, r, base+noticeSep(base)+"notice="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	s.audit(r, "success", http.StatusSeeOther)
	http.Redirect(w, r, base+noticeSep(base)+"notice="+url.QueryEscape(action+": ok"), http.StatusSeeOther)
}

func noticeSep(base string) string {
	if strings.Contains(base, "?") {
		return "&"
	}
	return "?"
}

// --- cluster settings ---

type settingsData struct {
	pageData
	Settings   *es.ClusterSettings
	CanCluster bool
	Notice     string
	Persistent []settingRow
	Transient  []settingRow
	Defaults   []settingRow
}

type settingRow struct{ Key, Value string }

func sortedRows(m map[string]string) []settingRow {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([]settingRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, settingRow{Key: k, Value: m[k]})
	}
	return rows
}

func (s *Server) handleClusterSettings(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	cluster := r.PathValue("cluster")
	client := clientFrom(r)
	data := settingsData{
		pageData:   s.page(r, "settings"),
		CanCluster: s.rbac.Allowed(sess.Roles, cluster, rbac.ClusterWrite),
		Notice:     r.URL.Query().Get("notice"),
	}
	settings, err := client.ClusterSettings(r.Context())
	if err != nil {
		data.Error = err.Error()
	} else {
		data.Persistent = sortedRows(settings.Persistent)
		data.Transient = sortedRows(settings.Transient)
		data.Defaults = sortedRows(settings.Defaults)
	}
	s.render(w, "settings.html", data)
}

func (s *Server) handleClusterSettingPut(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("cluster")
	client := clientFrom(r)
	key := r.PostFormValue("key")
	err := client.ClusterSettingPut(r.Context(), key, r.PostFormValue("value"))
	action := "set " + key
	if r.PostFormValue("value") == "" {
		action = "reset " + key
	}
	s.redirectNotice(w, r, "/c/"+cluster+"/settings", action, err)
}
