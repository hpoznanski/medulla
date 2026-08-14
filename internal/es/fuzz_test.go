package es

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hpoznanski/medulla/internal/config"
)

// FuzzDoPath pins the trust boundary between console input and the cluster:
// whatever path a user submits, the request must land inside the configured
// base path. A cluster URL may carry a path prefix (Medulla behind a proxy
// that mounts ES on a subpath), and escaping it would reach endpoints the
// operator deliberately fenced off.
func FuzzDoPath(f *testing.F) {
	for _, seed := range []string{
		"/", "/_cat/indices", "/idx/_search?q=1", "/_cluster/health?level=indices",
		"/../etc", "/%2e%2e/x", "/%2E%2E%2Fx", "//evil", "/a/../../b", "/./x",
		"/a%2f..%2fb", "\\..\\x", "/a\x00b", "/a?b=../c",
	} {
		f.Add(seed)
	}

	var served string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = r.URL.Path
		w.Write([]byte(`{}`))
	}))
	f.Cleanup(srv.Close)

	const prefix = "/base"
	c, err := NewClient(config.Cluster{Name: "fuzz", URL: srv.URL + prefix})
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, path string) {
		served = ""
		if _, err := c.Do(context.Background(), http.MethodGet, path, nil); err != nil {
			return // rejected before any request left the process
		}
		if served == "" {
			return // request never reached the handler
		}
		if !strings.HasPrefix(served, prefix+"/") && served != prefix {
			t.Fatalf("path %q escaped the base prefix: server saw %q", path, served)
		}
	})
}

// FuzzValidIndexName holds the guarantee the rest of the package leans on:
// an accepted name is safe to interpolate into a URL path.
func FuzzValidIndexName(f *testing.F) {
	for _, seed := range []string{
		"logs-1", ".ds-logs-000001", "..", "a/b", "a%2fb", "UPPER", "", "_x", "a..b", "a\x00b",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if !ValidIndexName(name) {
			return
		}
		for _, bad := range []string{"/", "\\", "..", "?", "#", "%", ":", "@", "\x00"} {
			if strings.Contains(name, bad) {
				t.Fatalf("accepted name %q contains %q", name, bad)
			}
		}
	})
}
