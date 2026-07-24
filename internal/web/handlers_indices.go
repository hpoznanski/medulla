package web

import (
	"net/http"
	"strconv"

	"github.com/hpoznanski/medulla/internal/es"
	"github.com/hpoznanski/medulla/internal/rbac"
)

type indicesData struct {
	pageData
	Indices []es.IndexInfo
	Notice  string
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
	data := indicesData{pageData: s.page(r, "indices"), Notice: r.URL.Query().Get("notice")}

	indices, err := client.Indices(r.Context())
	if err != nil {
		data.Error = err.Error()
	}
	data.Indices = indices
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
	Index  string
	Detail string
	Notice string
}

func (s *Server) handleIndexDetail(w http.ResponseWriter, r *http.Request) {
	client := clientFrom(r)
	index := r.PathValue("index")
	data := indexDetailData{
		pageData: s.page(r, "indices"),
		Index:    index,
		Notice:   r.URL.Query().Get("notice"),
	}

	detail, err := client.IndexDetail(r.Context(), index)
	if err != nil {
		data.Error = err.Error()
	}
	data.Detail = detail
	s.render(w, "index_detail.html", data)
}

func formInt(r *http.Request, field string, def int) int {
	n, err := strconv.Atoi(r.PostFormValue(field))
	if err != nil || n < 0 || n > 1024 {
		return def
	}
	return n
}
