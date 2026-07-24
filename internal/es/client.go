// Package es is a thin REST client for Elasticsearch and OpenSearch clusters.
package es

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hpoznanski/medulla/internal/config"
)

type Flavor string

const (
	FlavorES7        Flavor = "es7"
	FlavorES8        Flavor = "es8"
	FlavorOpenSearch Flavor = "opensearch"
	FlavorUnknown    Flavor = "unknown"
)

type Client struct {
	Name string

	baseURL    *url.URL
	authHeader string
	http       *http.Client

	mu   sync.Mutex
	info *Info
}

type Info struct {
	Flavor  Flavor
	Version string
}

// Response carries an ES reply. Body is the raw JSON; Status the HTTP status.
type Response struct {
	Status int
	Body   []byte
}

func (r *Response) OK() bool { return r.Status >= 200 && r.Status < 300 }

// ErrorReason extracts the ES error reason from a failed response body.
// (Not named Error to avoid accidentally satisfying the error interface.)
func (r *Response) ErrorReason() string {
	var e struct {
		Error struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if json.Unmarshal(r.Body, &e) == nil && e.Error.Reason != "" {
		return fmt.Sprintf("%s: %s", e.Error.Type, e.Error.Reason)
	}
	return fmt.Sprintf("HTTP %d", r.Status)
}

func NewClient(cfg config.Cluster) (*Client, error) {
	base, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("cluster %s: %w", cfg.Name, err)
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.TLS.Insecure {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // explicit operator opt-in per cluster
	}
	if cfg.TLS.CAFile != "" {
		pem, err := os.ReadFile(cfg.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("cluster %s: reading ca_file: %w", cfg.Name, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("cluster %s: no certificates in ca_file", cfg.Name)
		}
		tlsCfg.RootCAs = pool
	}

	c := &Client{
		Name:    cfg.Name,
		baseURL: base,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg, MaxIdleConnsPerHost: 4},
		},
	}

	switch cfg.Auth.Type {
	case "basic":
		c.authHeader = "Basic " + basicAuth(cfg.Auth.Username, cfg.Auth.Password.Value())
	case "api_key":
		c.authHeader = "ApiKey " + cfg.Auth.APIKey.Value()
	}
	return c, nil
}

// Do performs an ES request. path must start with "/"; query is passed through.
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader) (*Response, error) {
	ref, err := url.Parse(path)
	// Check the decoded path: %2e%2e would slip past a check on the raw string.
	if err != nil || !strings.HasPrefix(ref.Path, "/") || strings.Contains(ref.Path, "..") {
		return nil, fmt.Errorf("invalid path %q", path)
	}
	target := *c.baseURL
	target.Path = strings.TrimSuffix(target.Path, "/") + ref.Path
	target.RawQuery = ref.RawQuery

	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	if c.authHeader != "" {
		req.Header.Set("Authorization", c.authHeader)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cluster %s: %w", c.Name, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("cluster %s: reading response: %w", c.Name, err)
	}
	return &Response{Status: resp.StatusCode, Body: data}, nil
}

// GetJSON performs a GET and decodes the response into out.
func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if !resp.OK() {
		return fmt.Errorf("cluster %s: %s: %s", c.Name, path, resp.ErrorReason())
	}
	return json.Unmarshal(resp.Body, out)
}

// Info detects and caches the cluster flavor and version. The network fetch
// runs outside the lock so concurrent callers don't serialize on slow
// clusters; a rare duplicate fetch is harmless.
func (c *Client) Info(ctx context.Context) (*Info, error) {
	c.mu.Lock()
	cached := c.info
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	var root struct {
		Version struct {
			Number       string `json:"number"`
			Distribution string `json:"distribution"`
		} `json:"version"`
	}
	if err := c.GetJSON(ctx, "/", &root); err != nil {
		return nil, err
	}

	info := &Info{Version: root.Version.Number, Flavor: FlavorUnknown}
	switch {
	case root.Version.Distribution == "opensearch":
		info.Flavor = FlavorOpenSearch
	case strings.HasPrefix(root.Version.Number, "7."):
		info.Flavor = FlavorES7
	case strings.HasPrefix(root.Version.Number, "8."):
		info.Flavor = FlavorES8
	}
	c.mu.Lock()
	c.info = info
	c.mu.Unlock()
	return info, nil
}

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}
