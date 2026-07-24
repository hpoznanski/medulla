package web

import (
	"net/http"

	"github.com/hpoznanski/medulla/internal/es"
)

type catData struct {
	pageData
	Endpoint  string
	Endpoints []string
	Result    *es.CatResult
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
	s.render(w, "cat.html", data)
}
