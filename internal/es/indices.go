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
	return PrettyJSON(resp.Body), nil
}

// IndexSetting is one effective setting. Default marks values ES reports as
// built-in defaults rather than explicitly set on the index.
type IndexSetting struct {
	Key     string
	Value   string
	Default bool
}

// IndexSettings returns every effective setting for index, merging explicitly
// set values over ES's defaults. Plain GET /{index} omits defaults entirely,
// which hides most of what an operator wants to see.
func (c *Client) IndexSettings(ctx context.Context, index string) ([]IndexSetting, error) {
	if !ValidIndexName(index) {
		return nil, fmt.Errorf("invalid index name %q", index)
	}
	var raw map[string]struct {
		Settings map[string]any `json:"settings"`
		Defaults map[string]any `json:"defaults"`
	}
	path := "/" + url.PathEscape(index) + "/_settings?flat_settings=true&include_defaults=true"
	if err := c.GetJSON(ctx, path, &raw); err != nil {
		return nil, err
	}

	merged := map[string]IndexSetting{}
	// The response is keyed by the concrete index name, which differs from
	// index when index is an alias; take whatever single entry came back.
	for _, entry := range raw {
		for k, v := range entry.Defaults {
			merged[k] = IndexSetting{Key: k, Value: fmt.Sprint(v), Default: true}
		}
		for k, v := range entry.Settings {
			merged[k] = IndexSetting{Key: k, Value: fmt.Sprint(v)}
		}
	}
	out := make([]IndexSetting, 0, len(merged))
	for _, s := range merged {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// IndexSettingPut sets one index setting; an empty value resets it to the ES
// default. No allowlist of dynamic keys is kept here: ES knows which settings
// are updatable on a live index and rejects static ones (shard count, uuid)
// with a reason the caller surfaces.
func (c *Client) IndexSettingPut(ctx context.Context, index, key, value string) error {
	if !ValidIndexName(index) {
		return fmt.Errorf("invalid index name %q", index)
	}
	if !settingKeyPattern.MatchString(key) {
		return fmt.Errorf("invalid setting key %q", key)
	}
	var v any
	if value != "" {
		v = value
	}
	body, _ := json.Marshal(map[string]any{key: v})
	resp, err := c.Do(ctx, http.MethodPut, "/"+url.PathEscape(index)+"/_settings", bytes.NewReader(body))
	if err != nil {
		return err
	}
	if !resp.OK() {
		return fmt.Errorf("setting %s on %s: %s", key, index, resp.ErrorReason())
	}
	return nil
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
