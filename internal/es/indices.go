package es

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

type IndexInfo struct {
	Index     string `json:"index"`
	Health    string `json:"health"`
	Status    string `json:"status"`
	Pri       string `json:"pri"`
	Rep       string `json:"rep"`
	DocsCount string `json:"docs.count"`
	StoreSize string `json:"store.size"`
}

func (c *Client) Indices(ctx context.Context) ([]IndexInfo, error) {
	var out []IndexInfo
	cols := "index,health,status,pri,rep,docs.count,store.size"
	if err := c.GetJSON(ctx, "/_cat/indices?format=json&h="+cols, &out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

// indexNamePattern matches valid ES index names (incl. dot-prefixed system
// and .ds- data stream backing indices). Rejects path metacharacters.
var indexNamePattern = regexp.MustCompile(`^\.?[a-z0-9][a-z0-9._-]*$`)

func ValidIndexName(name string) bool {
	return name != "" && name != ".." && !strings.Contains(name, "..") && indexNamePattern.MatchString(name)
}

// indexActions maps UI action names to ES method + path suffix.
var indexActions = map[string]struct {
	Method string
	Suffix string
}{
	"open":       {http.MethodPost, "/_open"},
	"close":      {http.MethodPost, "/_close"},
	"refresh":    {http.MethodPost, "/_refresh"},
	"flush":      {http.MethodPost, "/_flush"},
	"forcemerge": {http.MethodPost, "/_forcemerge"},
	"delete":     {http.MethodDelete, ""},
}

func (c *Client) IndexAction(ctx context.Context, index, action string) error {
	if !ValidIndexName(index) {
		return fmt.Errorf("invalid index name %q", index)
	}
	act, ok := indexActions[action]
	if !ok {
		return fmt.Errorf("unknown action %q", action)
	}
	resp, err := c.Do(ctx, act.Method, "/"+url.PathEscape(index)+act.Suffix, nil)
	if err != nil {
		return err
	}
	if !resp.OK() {
		return fmt.Errorf("%s %s: %s", action, index, resp.ErrorReason())
	}
	return nil
}

func (c *Client) CreateIndex(ctx context.Context, index string, shards, replicas int) error {
	if !ValidIndexName(index) {
		return fmt.Errorf("invalid index name %q", index)
	}
	body, _ := json.Marshal(map[string]any{
		"settings": map[string]int{"number_of_shards": shards, "number_of_replicas": replicas},
	})
	resp, err := c.Do(ctx, http.MethodPut, "/"+url.PathEscape(index), bytes.NewReader(body))
	if err != nil {
		return err
	}
	if !resp.OK() {
		return fmt.Errorf("create %s: %s", index, resp.ErrorReason())
	}
	return nil
}

// IndexDetail returns pretty-printed settings/mappings/aliases JSON.
func (c *Client) IndexDetail(ctx context.Context, index string) (string, error) {
	if !ValidIndexName(index) {
		return "", fmt.Errorf("invalid index name %q", index)
	}
	resp, err := c.Do(ctx, http.MethodGet, "/"+url.PathEscape(index), nil)
	if err != nil {
		return "", err
	}
	if !resp.OK() {
		return "", fmt.Errorf("get %s: %s", index, resp.ErrorReason())
	}
	return prettyJSON(resp.Body), nil
}

// CatEndpoints is the allowlist for the cat browser.
var CatEndpoints = []string{
	"aliases", "allocation", "count", "health", "indices", "master", "nodeattrs",
	"nodes", "pending_tasks", "plugins", "recovery", "repositories", "segments",
	"shards", "templates", "thread_pool",
}

func ValidCatEndpoint(name string) bool {
	for _, e := range CatEndpoints {
		if e == name {
			return true
		}
	}
	return false
}

type CatResult struct {
	Columns []string
	Rows    []map[string]string
}

// Cat runs a _cat endpoint and preserves ES column order (taken from the
// first row's key order in the raw JSON).
func (c *Client) Cat(ctx context.Context, endpoint string) (*CatResult, error) {
	if !ValidCatEndpoint(endpoint) {
		return nil, fmt.Errorf("unknown cat endpoint %q", endpoint)
	}
	resp, err := c.Do(ctx, http.MethodGet, "/_cat/"+endpoint+"?format=json", nil)
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, fmt.Errorf("_cat/%s: %s", endpoint, resp.ErrorReason())
	}

	var raw []json.RawMessage
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		return nil, fmt.Errorf("_cat/%s: %w", endpoint, err)
	}
	result := &CatResult{}
	if len(raw) == 0 {
		return result, nil
	}
	result.Columns, err = jsonKeyOrder(raw[0])
	if err != nil {
		return nil, fmt.Errorf("_cat/%s: %w", endpoint, err)
	}
	for _, row := range raw {
		m := map[string]string{}
		var typed map[string]any
		if err := json.Unmarshal(row, &typed); err != nil {
			return nil, fmt.Errorf("_cat/%s: %w", endpoint, err)
		}
		for k, v := range typed {
			if v == nil {
				continue
			}
			m[k] = fmt.Sprint(v)
		}
		result.Rows = append(result.Rows, m)
	}
	return result, nil
}

// jsonKeyOrder extracts top-level object keys in document order.
func jsonKeyOrder(raw json.RawMessage) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil { // consume '{'
		return nil, err
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected token %v", tok)
		}
		keys = append(keys, key)
		var discard any // skip the value, including nested objects
		if err := dec.Decode(&discard); err != nil {
			return nil, err
		}
	}
	return keys, nil
}
