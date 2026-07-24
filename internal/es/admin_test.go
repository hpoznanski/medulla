package es

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recorded struct {
	method, path, body string
}

func recordingServer(t *testing.T, responses map[string]string) (*Client, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.method, rec.path, rec.body = r.Method, r.URL.Path, string(b)
		if resp, ok := responses[r.URL.Path]; ok {
			w.Write([]byte(resp))
			return
		}
		w.Write([]byte(`{"acknowledged":true}`))
	}))
	t.Cleanup(srv.Close)
	return newTestClient(t, srv.URL), rec
}

func TestAliasAction(t *testing.T) {
	c, rec := recordingServer(t, nil)

	if err := c.AliasAction(context.Background(), "add", "logs-1", "logs"); err != nil {
		t.Fatal(err)
	}
	if rec.path != "/_aliases" || !strings.Contains(rec.body, `"add"`) || !strings.Contains(rec.body, `"logs-1"`) {
		t.Errorf("request = %+v", rec)
	}

	if err := c.AliasAction(context.Background(), "explode", "i", "a"); err == nil {
		t.Error("bad action accepted")
	}
	if err := c.AliasAction(context.Background(), "add", "../x", "a"); err == nil {
		t.Error("bad index accepted")
	}
}

func TestTemplates(t *testing.T) {
	c, rec := recordingServer(t, map[string]string{
		"/_index_template": `{"index_templates":[{"name":"b","index_template":{"index_patterns":["b-*"]}},{"name":"a","index_template":{"index_patterns":["a-*"]}}]}`,
	})

	list, err := c.Templates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "a" {
		t.Errorf("list = %+v, want sorted", list)
	}

	if err := c.TemplatePut(context.Background(), "t1", `{"index_patterns":["x-*"]}`); err != nil {
		t.Fatal(err)
	}
	if rec.method != "PUT" || rec.path != "/_index_template/t1" {
		t.Errorf("put = %+v", rec)
	}
	if err := c.TemplatePut(context.Background(), "t1", `not json`); err == nil {
		t.Error("invalid JSON body accepted")
	}

	if err := c.TemplateDelete(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	if rec.method != "DELETE" {
		t.Errorf("delete method = %s", rec.method)
	}
}

func TestSnapshots(t *testing.T) {
	c, rec := recordingServer(t, map[string]string{
		"/_snapshot/_all":        `{"backup":{"type":"fs","settings":{"location":"/b"}}}`,
		"/_snapshot/backup/_all": `{"snapshots":[{"snapshot":"s1","state":"SUCCESS","start_time":"2026-07-22T00:00:00Z","indices":["i1","i2"]}]}`,
	})

	repos, err := c.Repos(context.Background())
	if err != nil || len(repos) != 1 || repos[0].Type != "fs" {
		t.Fatalf("repos=%+v err=%v", repos, err)
	}

	snaps, err := c.Snapshots(context.Background(), "backup")
	if err != nil || len(snaps) != 1 || len(snaps[0].Indices) != 2 {
		t.Fatalf("snaps=%+v err=%v", snaps, err)
	}

	if err := c.SnapshotCreate(context.Background(), "backup", "s2"); err != nil {
		t.Fatal(err)
	}
	if rec.method != "PUT" || rec.path != "/_snapshot/backup/s2" {
		t.Errorf("create = %+v", rec)
	}

	if err := c.SnapshotRestore(context.Background(), "backup", "s1"); err != nil {
		t.Fatal(err)
	}
	if rec.path != "/_snapshot/backup/s1/_restore" {
		t.Errorf("restore path = %s", rec.path)
	}

	if err := c.SnapshotDelete(context.Background(), "backup", "s1"); err != nil {
		t.Fatal(err)
	}
	if rec.method != "DELETE" {
		t.Errorf("delete method = %s", rec.method)
	}

	if _, err := c.Snapshots(context.Background(), "../etc"); err == nil {
		t.Error("bad repo name accepted")
	}
	if err := c.RepoCreate(context.Background(), "r", "nope"); err == nil {
		t.Error("invalid repo JSON accepted")
	}
}

func TestAnalyze(t *testing.T) {
	c, rec := recordingServer(t, map[string]string{
		"/_analyze":     `{"tokens":[{"token":"hello","type":"<ALPHANUM>","position":0,"start_offset":0,"end_offset":5}]}`,
		"/idx/_analyze": `{"tokens":[]}`,
	})

	tokens, err := c.Analyze(context.Background(), "", "standard", "hello")
	if err != nil || len(tokens) != 1 || tokens[0].Token != "hello" {
		t.Fatalf("tokens=%+v err=%v", tokens, err)
	}
	if !strings.Contains(rec.body, `"analyzer":"standard"`) {
		t.Errorf("body = %s", rec.body)
	}

	if _, err := c.Analyze(context.Background(), "idx", "standard", "x"); err != nil {
		t.Fatal(err)
	}
	if rec.path != "/idx/_analyze" {
		t.Errorf("path = %s", rec.path)
	}

	if _, err := c.Analyze(context.Background(), "../x", "standard", "x"); err == nil {
		t.Error("bad index accepted")
	}
}

func TestAliasesList(t *testing.T) {
	c, _ := recordingServer(t, map[string]string{
		"/_cat/aliases": `[{"alias":"z","index":"i2","filter":"-"},{"alias":"a","index":"i1","filter":"*"}]`,
	})
	aliases, err := c.Aliases(context.Background())
	if err != nil || len(aliases) != 2 {
		t.Fatalf("aliases=%v err=%v", aliases, err)
	}
	if aliases[0].Alias != "a" {
		t.Errorf("not sorted: %v", aliases)
	}
}

func TestTemplateGet(t *testing.T) {
	c, _ := recordingServer(t, map[string]string{
		"/_index_template/t1": `{"index_templates":[{"name":"t1"}]}`,
	})
	detail, err := c.TemplateGet(context.Background(), "t1")
	if err != nil || !strings.Contains(detail, "\n") {
		t.Errorf("detail=%q err=%v (want pretty-printed JSON)", detail, err)
	}
	if _, err := c.TemplateGet(context.Background(), "../x"); err == nil {
		t.Error("bad name accepted")
	}
}

func TestIndexDetail(t *testing.T) {
	c, _ := recordingServer(t, map[string]string{
		"/idx": `{"idx":{"settings":{"index":{"number_of_shards":"3"}}}}`,
	})
	detail, err := c.IndexDetail(context.Background(), "idx")
	if err != nil || !strings.Contains(detail, "number_of_shards") {
		t.Errorf("detail=%q err=%v", detail, err)
	}
	if _, err := c.IndexDetail(context.Background(), "UPPER"); err == nil {
		t.Error("bad name accepted")
	}
}

func TestClusterSettings(t *testing.T) {
	c, rec := recordingServer(t, map[string]string{
		"/_cluster/settings": `{"persistent":{"cluster.routing.allocation.enable":"all"},"transient":{"x.y":42},"defaults":{"a.b":true}}`,
	})
	s, err := c.ClusterSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Persistent["cluster.routing.allocation.enable"] != "all" || s.Transient["x.y"] != "42" || s.Defaults["a.b"] != "true" {
		t.Errorf("settings=%+v (non-string values must flatten)", s)
	}

	if err := c.ClusterSettingPut(context.Background(), "cluster.routing.allocation.enable", "none"); err != nil {
		t.Fatal(err)
	}
	if rec.method != "PUT" || !strings.Contains(rec.body, `"none"`) {
		t.Errorf("put = %+v", rec)
	}
	// empty value resets: body must carry JSON null
	if err := c.ClusterSettingPut(context.Background(), "cluster.routing.allocation.enable", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.body, "null") {
		t.Errorf("reset body = %s, want null", rec.body)
	}
	if err := c.ClusterSettingPut(context.Background(), "bad key", "x"); err == nil {
		t.Error("invalid key accepted")
	}
}

func TestAllocationExplainFor(t *testing.T) {
	c, rec := recordingServer(t, map[string]string{
		"/_cluster/allocation/explain": `{
			"index":"i1","shard":0,"primary":false,
			"allocate_explanation":"generic boilerplate",
			"unassigned_info":{"reason":"REPLICA_ADDED"},
			"node_allocation_decisions":[
				{"node_name":"n1","deciders":[{"explanation":"a copy of this shard is already allocated to this node [[i1][0], node[abc]]"}]},
				{"node_name":"n2","deciders":[{"explanation":"a copy of this shard is already allocated to this node [[i1][0], node[def]]"}]}
			]}`,
	})
	ex, err := c.AllocationExplainFor(context.Background(), "i1", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.body, `"shard":0`) {
		t.Errorf("request body = %s", rec.body)
	}
	if strings.Contains(ex.Explanation, "[[") {
		t.Errorf("bracketed internals not stripped: %q", ex.Explanation)
	}
	if !strings.Contains(ex.Explanation, "n1: a copy of this shard is already allocated") {
		t.Errorf("decider cause missing: %q", ex.Explanation)
	}
	if strings.Count(ex.Explanation, "already allocated") != 1 {
		t.Errorf("duplicate causes not deduped: %q", ex.Explanation)
	}

	if _, err := c.AllocationExplainFor(context.Background(), "../x", 0, false); err == nil {
		t.Error("bad index accepted")
	}
}

func TestAllocationExplainFallbacks(t *testing.T) {
	c, _ := recordingServer(t, map[string]string{
		"/_cluster/allocation/explain": `{"unassigned_info":{"reason":"NODE_LEFT"},"node_allocation_decisions":[]}`,
	})
	ex, err := c.AllocationExplainFor(context.Background(), "i1", 0, true)
	if err != nil || ex.Explanation != "NODE_LEFT" {
		t.Errorf("ex=%+v err=%v (want unassigned_info fallback)", ex, err)
	}
}
