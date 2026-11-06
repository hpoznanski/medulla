package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hpoznanski/medulla/internal/es"
	"github.com/hpoznanski/medulla/internal/rbac"
)

type indicesData struct {
	pageData
	Indices []es.IndexInfo // filtered and sorted
	Names   []string       // datalist suggestions
	Counts  map[string]int // per health, over every index
	Total   int
	Notice  string
	View    listView
	Updated string
}

var indexSort = &sortSpec{def: "index", health: []string{"red", "yellow", "green"}, keys: map[string]bool{
	"index": false, "health": true, "status": false,
	"pri": true, "rep": true, "docs": true, "size": true,
}}

// maxSuggestions caps the filter's datalist: past a few thousand options it
// costs more page weight than it saves typing.
const maxSuggestions = 1000

var indexHealthRank = map[string]string{"red": "2", "yellow": "1", "green": "0"}

func indexCell(ix es.IndexInfo, key string) string {
	switch key {
	case "health":
		return indexHealthRank[ix.Health]
	case "status":
		return ix.Status
	case "pri":
		return ix.Pri
	case "rep":
		return ix.Rep
	case "docs":
		return ix.DocsCount
	case "size":
		return ix.StoreSize
	}
	return ix.Index
}

func (s *Server) page(r *http.Request, nav string) pageData {
	sess := sessionFrom(r)
	cluster := r.PathValue("cluster")
	return pageData{
		User:       sess.User,
		Roles:      sess.Roles,
		Cluster:    cluster,
		Clusters:   s.rbac.Clusters(sess.Roles, s.clusters.Names()),
		Nav:        nav,
		CanWrite:   s.rbac.Allowed(sess.Roles, cluster, rbac.IndexWrite),
		CanConsole: s.rbac.Allowed(sess.Roles, cluster, rbac.RestGet),
	}
}

func (s *Server) handleIndices(w http.ResponseWriter, r *http.Request) {
	client := clientFrom(r)
	view := parseListView(r.URL.Query(), indexSort)
	data := indicesData{
		pageData: s.page(r, "indices"),
		Notice:   r.URL.Query().Get("notice"),
		View:     view,
		Counts:   map[string]int{},
		Updated:  time.Now().UTC().Format("15:04:05 UTC"),
	}

	indices, err := client.Indices(r.Context())
	if err != nil {
		data.Error = err.Error()
	}
	data.Total = len(indices)
	needle := strings.ToLower(view.Q)
	for _, ix := range indices {
		data.Counts[ix.Health]++
		if len(data.Names) < maxSuggestions {
			data.Names = append(data.Names, ix.Index)
		}
		if view.Health != "" && ix.Health != view.Health {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(ix.Index), needle) {
			continue
		}
		data.Indices = append(data.Indices, ix)
	}
	sortRows(data.Indices, view.Desc, func(ix es.IndexInfo) string { return indexCell(ix, view.Sort) })
	s.render(w, "indices.html", data)
}

func (s *Server) handleIndexAction(w http.ResponseWriter, r *http.Request) {
	cluster, index, action := r.PathValue("cluster"), r.PathValue("index"), r.PathValue("action")
	client := clientFrom(r)

	if action == "delete" && r.PostFormValue("confirm") != index {
		http.Redirect(w, r, "/c/"+cluster+"/indices/"+index+"?notice=type+the+index+name+to+confirm+deletion", http.StatusSeeOther)
		return
	}

	err := client.IndexAction(r.Context(), index, action)
	s.redirectNotice(w, r, "/c/"+cluster+"/indices", action+" "+index, err)
}

func (s *Server) handleIndexCreate(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("cluster")
	client := clientFrom(r)

	name := r.PostFormValue("name")
	err := client.CreateIndex(r.Context(), name, formInt(r, "shards", 1), formInt(r, "replicas", 1))
	s.redirectNotice(w, r, "/c/"+cluster+"/indices", "created "+name, err)
}

type indexDetailData struct {
	pageData
	Index      string
	Detail     string
	Settings   []es.IndexSetting
	SettingKey string // prefills the edit form when a table row is clicked
	Notice     string
}

func (s *Server) handleIndexDetail(w http.ResponseWriter, r *http.Request) {
	client := clientFrom(r)
	index := r.PathValue("index")
	data := indexDetailData{
		pageData:   s.page(r, "indices"),
		Index:      index,
		SettingKey: r.URL.Query().Get("key"),
		Notice:     r.URL.Query().Get("notice"),
	}

	detail, err := client.IndexDetail(r.Context(), index)
	if err != nil {
		data.Error = err.Error()
	}
	data.Detail = detail

	settings, err := client.IndexSettings(r.Context(), index)
	if err != nil && data.Error == "" {
		data.Error = err.Error()
	}
	data.Settings = settings
	s.render(w, "index_detail.html", data)
}

func (s *Server) handleIndexSettingPut(w http.ResponseWriter, r *http.Request) {
	cluster, index := r.PathValue("cluster"), r.PathValue("index")
	client := clientFrom(r)

	key, value := r.PostFormValue("key"), r.PostFormValue("value")
	err := client.IndexSettingPut(r.Context(), index, key, value)
	action := "set " + key
	if value == "" {
		action = "reset " + key
	}
	s.redirectNotice(w, r, "/c/"+cluster+"/indices/"+index, action, err)
}

func formInt(r *http.Request, field string, def int) int {
	n, err := strconv.Atoi(r.PostFormValue(field))
	if err != nil || n < 0 || n > 1024 {
		return def
	}
	return n
}
