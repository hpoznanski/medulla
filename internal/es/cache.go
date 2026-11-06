package es

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// cacheTTL is how long a cached read is served to every viewer of a cluster.
// Auto-refresh multiplies page loads by viewers × tabs; this collapses them
// into at most one ES request per path per window, per replica.
// ponytail: per-replica memory cache, no shared store — N replicas mean up
// to N requests per window, which is still bounded.
var cacheTTL = 5 * time.Second

// readOnlyPOST lists POST endpoints that change nothing, so they must not
// invalidate the cache (the overview calls explain on every load).
var readOnlyPOST = map[string]bool{
	"/_cluster/allocation/explain": true,
}

type cacheEntry struct {
	done chan struct{}
	at   time.Time // zero while the fetch is in flight
	resp *Response
	err  error
}

// GetCached is a GET whose result is shared: concurrent callers wait on one
// in-flight request, and the answer is reused for cacheTTL. Errors are
// cached too, so a dead cluster is not hammered by every viewer.
//
// The fetch runs detached from the caller's context, bounded by the HTTP
// client timeout, so one viewer navigating away cannot fail it for the
// others. Each caller still stops waiting when its own context ends.
// Callers must treat the returned Body as read-only.
func (c *Client) GetCached(ctx context.Context, path string) (*Response, error) {
	c.cacheMu.Lock()
	e := c.cache[path]
	if e == nil || (!e.at.IsZero() && time.Since(e.at) > cacheTTL) {
		e = &cacheEntry{done: make(chan struct{})}
		if c.cache == nil {
			c.cache = map[string]*cacheEntry{}
		}
		c.cache[path] = e
		gen := c.cacheGen
		go func() {
			resp, err := c.Do(context.WithoutCancel(ctx), http.MethodGet, path, nil)
			c.cacheMu.Lock()
			e.resp, e.err, e.at = resp, err, time.Now()
			// A write landed while this read was in flight: hand the result
			// to whoever is waiting, but don't keep it for later readers.
			if c.cacheGen != gen && c.cache[path] == e {
				delete(c.cache, path)
			}
			c.cacheMu.Unlock()
			close(e.done)
		}()
	}
	c.cacheMu.Unlock()

	select {
	case <-e.done:
		return e.resp, e.err
	case <-ctx.Done():
		return nil, fmt.Errorf("cluster %s: %w", c.Name, ctx.Err())
	}
}

// GetJSONCached is GetJSON through the shared cache.
func (c *Client) GetJSONCached(ctx context.Context, path string, out any) error {
	resp, err := c.GetCached(ctx, path)
	if err != nil {
		return err
	}
	return decodeOK(c.Name, path, resp, out)
}

// invalidate drops every cached read, so a page loaded right after a change
// made through Medulla shows the change.
func (c *Client) invalidate() {
	c.cacheMu.Lock()
	c.cache = nil
	c.cacheGen++
	c.cacheMu.Unlock()
}
