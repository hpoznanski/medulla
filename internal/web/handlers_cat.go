package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/hpoznanski/medulla/internal/es"
)

type catData struct {
	pageData
	Endpoint  string
	Endpoints []string
	Result    *es.CatResult
	Shown     []map[string]string // rows after filter and sort
	View      listView
	Updated   string
}

func (s *Server) handleCat(w http.ResponseWriter, r *http.Request) {
	client := clientFrom(r)
	endpoint := r.PathValue("endpoint")
	data := catData{
		pageData:  s.page(r, "cat"),
		Endpoint:  endpoint,
		Endpoints: es.CatEndpoints,
	}

	if !es.ValidCatEndpoint(endpoint) {
		http.NotFound(w, r)
		return
	}
	result, err := client.Cat(r.Context(), endpoint)
	if err != nil {
		data.Error = err.Error()
	}
	data.Result = result
	data.Updated = time.Now().UTC().Format("15:04:05 UTC")

	// The sortable columns are whatever this endpoint returned, so the
	// allowlist is built per response; with no sort, ES's own row order stays.
	spec := &sortSpec{keys: map[string]bool{}}
	if result != nil {
		for _, col := range result.Columns {
			spec.keys[col] = false
		}
	}
	data.View = parseListView(r.URL.Query(), spec)
	if result != nil {
		needle := strings.ToLower(data.View.Q)
		for _, row := range result.Rows {
			if needle == "" || rowContains(row, needle) {
				data.Shown = append(data.Shown, row)
			}
		}
		if data.View.Sort != "" {
			sortRows(data.Shown, data.View.Desc, func(row map[string]string) string { return row[data.View.Sort] })
		}
	}
	s.render(w, "cat.html", data)
}

func rowContains(row map[string]string, needle string) bool {
	for _, v := range row {
		if strings.Contains(strings.ToLower(v), needle) {
			return true
		}
	}
	return false
}
