package es

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hpoznanski/medulla/internal/config"
)

func fakeCluster(t *testing.T, rootJSON string, extra map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := extra[r.URL.Path]; ok {
			w.Write([]byte(body))
			return
		}
		if r.URL.Path == "/" {
			w.Write([]byte(rootJSON))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"type":"index_not_found_exception","reason":"no such index [x]"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := NewClient(config.Cluster{Name: "test", URL: url})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFlavorDetection(t *testing.T) {
	tests := []struct {
		name string
		root string
		want Flavor
	}{
		{"es8", `{"version":{"number":"8.13.4"}}`, FlavorES8},
		{"es7", `{"version":{"number":"7.17.9"}}`, FlavorES7},
		{"opensearch", `{"version":{"number":"2.13.0","distribution":"opensearch"}}`, FlavorOpenSearch},
		{"unknown", `{"version":{"number":"5.6.0"}}`, FlavorUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fakeCluster(t, tt.root, nil)
			info, err := newTestClient(t, srv.URL).Info(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if info.Flavor != tt.want {
				t.Errorf("flavor = %s, want %s", info.Flavor, tt.want)
			}
		})
	}
}

func TestInfoCached(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"version":{"number":"8.0.0"}}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL)
	c.Info(context.Background())
	c.Info(context.Background())
	if calls != 1 {
		t.Errorf("root endpoint called %d times, want 1", calls)
	}
}

func TestAuthHeaderInjection(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	tests := []struct {
		name string
		auth config.ClusterAuth
		want string
	}{
		{"basic", config.ClusterAuth{Type: "basic", Username: "u", Password: "p"}, "Basic dTpw"},
		{"api key", config.ClusterAuth{Type: "api_key", APIKey: "abc123"}, "ApiKey abc123"},
		{"none", config.ClusterAuth{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewClient(config.Cluster{Name: "t", URL: srv.URL, Auth: tt.auth})
			if err != nil {
				t.Fatal(err)
			}
			c.Do(context.Background(), http.MethodGet, "/", nil)
			if gotAuth != tt.want {
				t.Errorf("Authorization = %q, want %q", gotAuth, tt.want)
			}
		})
	}
}

func TestErrorPropagation(t *testing.T) {
	srv := fakeCluster(t, `{}`, nil)
	resp, err := newTestClient(t, srv.URL).Do(context.Background(), http.MethodGet, "/missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK() {
		t.Error("404 reported as OK")
	}
	if !strings.Contains(resp.ErrorReason(), "no such index") {
		t.Errorf("Error() = %q, want ES reason surfaced", resp.ErrorReason())
	}
}

func TestUnreachableCluster(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1") // nothing listens there
	if _, err := c.Do(context.Background(), http.MethodGet, "/", nil); err == nil {
		t.Error("want connection error")
	}
}

func TestPathValidation(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1")
	for _, bad := range []string{"no-slash", "/a/../b", "http://evil/"} {
		if _, err := c.Do(context.Background(), http.MethodGet, bad, nil); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}
}

func TestOverview(t *testing.T) {
	srv := fakeCluster(t, `{"version":{"number":"8.13.0"}}`, map[string]string{
		"/_cluster/health": `{"cluster_name":"t","status":"yellow","number_of_nodes":2,"active_shards":10,"unassigned_shards":1}`,
		"/_cat/nodes":      `[{"name":"n1","ip":"10.0.0.1","node.role":"dm","master":"*","heap.percent":"42","disk.used_percent":"51.5","cpu":"7","load_1m":"0.42"}]`,
		"/_cat/shards":     `[{"index":"i1","shard":"0","prirep":"p","state":"STARTED","node":"n1"},{"index":"i1","shard":"1","prirep":"r","state":"UNASSIGNED","node":"","unassigned.reason":"NODE_LEFT"}]`,
	})

	o, err := newTestClient(t, srv.URL).Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if o.Health.Status != "yellow" || o.Health.UnassignedShards != 1 {
		t.Errorf("health = %+v", o.Health)
	}
	if len(o.Nodes) != 1 || !o.Nodes[0].Master || o.Nodes[0].HeapPercent != 42 || o.Nodes[0].DiskPercent != 51 {
		t.Errorf("nodes = %+v", o.Nodes)
	}
	if len(o.Shards) != 2 || o.Shards[1].Reason != "NODE_LEFT" {
		t.Errorf("shards = %+v", o.Shards)
	}
}

func TestRegistry(t *testing.T) {
	r, err := NewRegistry([]config.Cluster{
		{Name: "a", URL: "http://a:9200"},
		{Name: "b", URL: "http://b:9200"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Names(); len(got) != 2 || got[0] != "a" {
		t.Errorf("Names() = %v, want config order", got)
	}
	if _, err := r.Get("a"); err != nil {
		t.Error(err)
	}
	if _, err := r.Get("nope"); err == nil {
		t.Error("unknown cluster accepted")
	}
}

func TestNewClientTLSConfig(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if _, err := NewClient(config.Cluster{Name: "t", URL: "https://x", TLS: config.TLS{CAFile: caFile}}); err == nil {
		t.Error("missing ca_file accepted")
	}
	os.WriteFile(caFile, []byte("not a pem"), 0o600)
	if _, err := NewClient(config.Cluster{Name: "t", URL: "https://x", TLS: config.TLS{CAFile: caFile}}); err == nil {
		t.Error("invalid ca_file accepted")
	}
	if _, err := NewClient(config.Cluster{Name: "t", URL: "https://x", TLS: config.TLS{Insecure: true}}); err != nil {
		t.Errorf("insecure client: %v", err)
	}
	if _, err := NewClient(config.Cluster{Name: "t", URL: "://bad"}); err == nil {
		t.Error("invalid url accepted")
	}
}

func TestPathValidationEncoded(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1")
	if _, err := c.Do(context.Background(), http.MethodGet, "/%2e%2e/secret", nil); err == nil {
		t.Error("percent-encoded traversal accepted")
	}
}
