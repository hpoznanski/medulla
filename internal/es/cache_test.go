package es

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpoznanski/medulla/internal/config"
)

// countingES counts GETs to /_cluster/health; release, when non-nil, holds
// each GET until closed.
func countingES(t *testing.T, release chan struct{}) (*Client, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/_cluster/health" {
			hits.Add(1)
			if release != nil {
				<-release
			}
		}
		w.Write([]byte(`{"status":"green"}`))
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(config.Cluster{Name: "t", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c, &hits
}

func TestGetCachedSharesOneRequest(t *testing.T) {
	release := make(chan struct{})
	c, hits := countingES(t, release)

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := c.GetCached(context.Background(), "/_cluster/health"); err != nil {
				t.Error(err)
			}
		})
	}
	time.Sleep(50 * time.Millisecond) // let all 20 queue on the one in-flight fetch
	close(release)
	wg.Wait()
	if n := hits.Load(); n != 1 {
		t.Errorf("20 concurrent viewers caused %d ES requests, want 1", n)
	}

	c.GetCached(context.Background(), "/_cluster/health")
	if n := hits.Load(); n != 1 {
		t.Errorf("read within TTL went to ES (%d requests)", n)
	}
}

func TestGetCachedExpires(t *testing.T) {
	old := cacheTTL
	cacheTTL = 10 * time.Millisecond
	t.Cleanup(func() { cacheTTL = old })
	c, hits := countingES(t, nil)

	c.GetCached(context.Background(), "/_cluster/health")
	time.Sleep(20 * time.Millisecond)
	c.GetCached(context.Background(), "/_cluster/health")
	if n := hits.Load(); n != 2 {
		t.Errorf("expired entry served from cache: %d requests, want 2", n)
	}
}

// A change made through Medulla must show on the very next page load.
func TestWriteInvalidatesCache(t *testing.T) {
	c, hits := countingES(t, nil)
	ctx := context.Background()

	c.GetCached(ctx, "/_cluster/health")
	c.Do(ctx, http.MethodPost, "/_cluster/allocation/explain", strings.NewReader(`{}`))
	c.GetCached(ctx, "/_cluster/health")
	if n := hits.Load(); n != 1 {
		t.Errorf("read-only explain invalidated the cache: %d requests", n)
	}

	c.Do(ctx, http.MethodPut, "/_cluster/settings", strings.NewReader(`{}`))
	c.GetCached(ctx, "/_cluster/health")
	if n := hits.Load(); n != 2 {
		t.Errorf("read after a write served stale data: %d requests, want 2", n)
	}
}

// A write landing mid-read: waiters get the in-flight answer, but it is not
// kept for anyone who asks after the write.
func TestWriteDuringReadNotCached(t *testing.T) {
	release := make(chan struct{})
	c, hits := countingES(t, release)
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		c.GetCached(ctx, "/_cluster/health")
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	c.invalidate()
	close(release)
	<-done

	c.GetCached(ctx, "/_cluster/health")
	if n := hits.Load(); n != 2 {
		t.Errorf("pre-write read was cached: %d requests, want 2", n)
	}
}

// One viewer giving up must not fail the fetch for everyone else.
func TestCancelledCallerDoesNotPoisonOthers(t *testing.T) {
	release := make(chan struct{})
	c, _ := countingES(t, release)

	leader, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := c.GetCached(leader, "/_cluster/health")
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-errc; err == nil {
		t.Error("cancelled caller kept waiting")
	}

	close(release)
	resp, err := c.GetCached(context.Background(), "/_cluster/health")
	if err != nil || !resp.OK() {
		t.Errorf("follower got err=%v after leader cancelled", err)
	}
}
