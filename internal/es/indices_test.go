package es

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidIndexName(t *testing.T) {
	valid := []string{"logs-app-2026.07.22", ".ds-logs-000001", "metrics_demo", "a"}
	invalid := []string{"", "..", "a/../b", "UPPER", "_internal", "a b", "-lead", "a/b"}
	for _, n := range valid {
		if !ValidIndexName(n) {
			t.Errorf("%q rejected", n)
		}
	}
	for _, n := range invalid {
		if ValidIndexName(n) {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestIndexAction(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Write([]byte(`{"acknowledged":true}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL)

	tests := []struct {
		action, wantMethod, wantPath string
	}{
		{"open", "POST", "/idx/_open"},
		{"close", "POST", "/idx/_close"},
		{"refresh", "POST", "/idx/_refresh"},
		{"flush", "POST", "/idx/_flush"},
		{"forcemerge", "POST", "/idx/_forcemerge"},
		{"delete", "DELETE", "/idx"},
	}
	for _, tt := range tests {
		if err := c.IndexAction(context.Background(), "idx", tt.action); err != nil {
			t.Fatalf("%s: %v", tt.action, err)
		}
		if gotMethod != tt.wantMethod || gotPath != tt.wantPath {
			t.Errorf("%s: %s %s, want %s %s", tt.action, gotMethod, gotPath, tt.wantMethod, tt.wantPath)
		}
	}

	if err := c.IndexAction(context.Background(), "idx", "explode"); err == nil {
		t.Error("unknown action accepted")
	}
	if err := c.IndexAction(context.Background(), "../etc", "open"); err == nil {
		t.Error("bad index name accepted")
	}
}

func TestIndexActionESError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"type":"illegal_argument_exception","reason":"cannot close open index"}}`))
	}))
	t.Cleanup(srv.Close)
	err := newTestClient(t, srv.URL).IndexAction(context.Background(), "idx", "close")
	if err == nil || !strings.Contains(err.Error(), "cannot close open index") {
		t.Errorf("err = %v", err)
	}
}

func TestCreateIndex(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte(`{"acknowledged":true}`))
	}))
	t.Cleanup(srv.Close)

	if err := newTestClient(t, srv.URL).CreateIndex(context.Background(), "newidx", 3, 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"number_of_shards":3`) || !strings.Contains(gotBody, `"number_of_replicas":1`) {
		t.Errorf("body = %s", gotBody)
	}
}

func TestCat(t *testing.T) {
	srv := fakeCluster(t, `{}`, map[string]string{
		"/_cat/shards": `[{"index":"i1","shard":"0","prirep":"p","state":"STARTED","node":"n1","docs":null}]`,
	})
	res, err := newTestClient(t, srv.URL).Cat(context.Background(), "shards")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"index", "shard", "prirep", "state", "node", "docs"}
	if len(res.Columns) != len(want) {
		t.Fatalf("columns = %v", res.Columns)
	}
	for i, col := range want {
		if res.Columns[i] != col {
			t.Errorf("column %d = %q, want %q (order must match ES)", i, res.Columns[i], col)
		}
	}
	if res.Rows[0]["state"] != "STARTED" {
		t.Errorf("rows = %v", res.Rows)
	}
	if _, ok := res.Rows[0]["docs"]; ok {
		t.Error("null value must be omitted")
	}
}

func TestCatEmpty(t *testing.T) {
	srv := fakeCluster(t, `{}`, map[string]string{"/_cat/aliases": `[]`})
	res, err := newTestClient(t, srv.URL).Cat(context.Background(), "aliases")
	if err != nil || len(res.Rows) != 0 {
		t.Errorf("res=%+v err=%v", res, err)
	}
}

func TestCatAllowlist(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1")
	for _, bad := range []string{"", "tasks?detailed", "../_search", "snapshots"} {
		if _, err := c.Cat(context.Background(), bad); err == nil {
			t.Errorf("endpoint %q accepted", bad)
		}
	}
}

func TestIndicesList(t *testing.T) {
	srv := fakeCluster(t, `{}`, map[string]string{
		"/_cat/indices": `[{"index":"b","health":"green","status":"open","pri":"1","rep":"1","docs.count":"5","store.size":"10kb"},{"index":"a","health":"yellow","status":"open","pri":"2","rep":"2"}]`,
	})
	list, err := newTestClient(t, srv.URL).Indices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Index != "a" {
		t.Errorf("list = %+v, want sorted by name", list)
	}
}
