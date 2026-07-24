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

// ValidName covers aliases, templates, repositories and snapshots — same
// conservative charset as index names.
func ValidName(name string) bool { return ValidIndexName(name) }

func prettyJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// --- aliases ---

type AliasInfo struct {
	Alias  string `json:"alias"`
	Index  string `json:"index"`
	Filter string `json:"filter"`
}

func (c *Client) Aliases(ctx context.Context) ([]AliasInfo, error) {
	var out []AliasInfo
	if err := c.GetJSON(ctx, "/_cat/aliases?format=json&h=alias,index,filter", &out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil
}

// AliasAction performs an add or remove action via the _aliases API.
func (c *Client) AliasAction(ctx context.Context, action, index, alias string) error {
	if action != "add" && action != "remove" {
		return fmt.Errorf("unknown alias action %q", action)
	}
	if !ValidIndexName(index) || !ValidName(alias) {
		return fmt.Errorf("invalid index or alias name")
	}
	body, _ := json.Marshal(map[string]any{
		"actions": []map[string]any{{action: map[string]string{"index": index, "alias": alias}}},
	})
	resp, err := c.Do(ctx, http.MethodPost, "/_aliases", bytes.NewReader(body))
	if err != nil {
		return err
	}
	if !resp.OK() {
		return fmt.Errorf("alias %s: %s", action, resp.ErrorReason())
	}
	return nil
}

// --- index templates ---

type TemplateInfo struct {
	Name     string
	Patterns []string
}

func (c *Client) Templates(ctx context.Context) ([]TemplateInfo, error) {
	var raw struct {
		IndexTemplates []struct {
			Name          string `json:"name"`
			IndexTemplate struct {
				IndexPatterns []string `json:"index_patterns"`
			} `json:"index_template"`
		} `json:"index_templates"`
	}
	if err := c.GetJSON(ctx, "/_index_template", &raw); err != nil {
		return nil, err
	}
	out := make([]TemplateInfo, 0, len(raw.IndexTemplates))
	for _, t := range raw.IndexTemplates {
		out = append(out, TemplateInfo{Name: t.Name, Patterns: t.IndexTemplate.IndexPatterns})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *Client) TemplateGet(ctx context.Context, name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("invalid template name %q", name)
	}
	resp, err := c.Do(ctx, http.MethodGet, "/_index_template/"+url.PathEscape(name), nil)
	if err != nil {
		return "", err
	}
	if !resp.OK() {
		return "", fmt.Errorf("template %s: %s", name, resp.ErrorReason())
	}
	return prettyJSON(resp.Body), nil
}

// TemplatePut creates or replaces an index template from raw JSON.
func (c *Client) TemplatePut(ctx context.Context, name, body string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid template name %q", name)
	}
	if !json.Valid([]byte(body)) {
		return fmt.Errorf("template body is not valid JSON")
	}
	resp, err := c.Do(ctx, http.MethodPut, "/_index_template/"+url.PathEscape(name), strings.NewReader(body))
	if err != nil {
		return err
	}
	if !resp.OK() {
		return fmt.Errorf("template put %s: %s", name, resp.ErrorReason())
	}
	return nil
}

func (c *Client) TemplateDelete(ctx context.Context, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid template name %q", name)
	}
	resp, err := c.Do(ctx, http.MethodDelete, "/_index_template/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	if !resp.OK() {
		return fmt.Errorf("template delete %s: %s", name, resp.ErrorReason())
	}
	return nil
}

// --- snapshots ---

type RepoInfo struct {
	Name     string
	Type     string
	Settings string
}

func (c *Client) Repos(ctx context.Context) ([]RepoInfo, error) {
	var raw map[string]struct {
		Type     string         `json:"type"`
		Settings map[string]any `json:"settings"`
	}
	if err := c.GetJSON(ctx, "/_snapshot/_all", &raw); err != nil {
		return nil, err
	}
	out := make([]RepoInfo, 0, len(raw))
	for name, r := range raw {
		settings, _ := json.Marshal(r.Settings)
		out = append(out, RepoInfo{Name: name, Type: r.Type, Settings: string(settings)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RepoCreate registers a repository; settings is raw JSON like
// {"type":"fs","settings":{"location":"/backup"}}.
func (c *Client) RepoCreate(ctx context.Context, name, body string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid repository name %q", name)
	}
	if !json.Valid([]byte(body)) {
		return fmt.Errorf("repository body is not valid JSON")
	}
	resp, err := c.Do(ctx, http.MethodPut, "/_snapshot/"+url.PathEscape(name), strings.NewReader(body))
	if err != nil {
		return err
	}
	if !resp.OK() {
		return fmt.Errorf("repo create %s: %s", name, resp.ErrorReason())
	}
	return nil
}

type SnapshotInfo struct {
	Snapshot  string   `json:"snapshot"`
	State     string   `json:"state"`
	StartTime string   `json:"start_time"`
	Indices   []string `json:"indices"`
}

func (c *Client) Snapshots(ctx context.Context, repo string) ([]SnapshotInfo, error) {
	if !ValidName(repo) {
		return nil, fmt.Errorf("invalid repository name %q", repo)
	}
	var raw struct {
		Snapshots []SnapshotInfo `json:"snapshots"`
	}
	if err := c.GetJSON(ctx, "/_snapshot/"+url.PathEscape(repo)+"/_all", &raw); err != nil {
		return nil, err
	}
	return raw.Snapshots, nil
}

func (c *Client) snapshotPath(repo, name string) (string, error) {
	if !ValidName(repo) || !ValidName(name) {
		return "", fmt.Errorf("invalid repository or snapshot name")
	}
	return "/_snapshot/" + url.PathEscape(repo) + "/" + url.PathEscape(name), nil
}

func (c *Client) SnapshotCreate(ctx context.Context, repo, name string) error {
	path, err := c.snapshotPath(repo, name)
	if err != nil {
		return err
	}
	resp, err := c.Do(ctx, http.MethodPut, path, nil)
	if err != nil {
		return err
	}
	if !resp.OK() {
		return fmt.Errorf("snapshot create: %s", resp.ErrorReason())
	}
	return nil
}

func (c *Client) SnapshotRestore(ctx context.Context, repo, name string) error {
	path, err := c.snapshotPath(repo, name)
	if err != nil {
		return err
	}
	resp, err := c.Do(ctx, http.MethodPost, path+"/_restore", nil)
	if err != nil {
		return err
	}
	if !resp.OK() {
		return fmt.Errorf("snapshot restore: %s", resp.ErrorReason())
	}
	return nil
}

func (c *Client) SnapshotDelete(ctx context.Context, repo, name string) error {
	path, err := c.snapshotPath(repo, name)
	if err != nil {
		return err
	}
	resp, err := c.Do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if !resp.OK() {
		return fmt.Errorf("snapshot delete: %s", resp.ErrorReason())
	}
	return nil
}

// --- analyze ---

type Token struct {
	Token    string `json:"token"`
	Type     string `json:"type"`
	Position int    `json:"position"`
	Start    int    `json:"start_offset"`
	End      int    `json:"end_offset"`
}

// Analyze runs _analyze with the given analyzer (cluster-wide, or scoped to
// an index when index is non-empty).
func (c *Client) Analyze(ctx context.Context, index, analyzer, text string) ([]Token, error) {
	path := "/_analyze"
	if index != "" {
		if !ValidIndexName(index) {
			return nil, fmt.Errorf("invalid index name %q", index)
		}
		path = "/" + url.PathEscape(index) + "/_analyze"
	}
	body, _ := json.Marshal(map[string]string{"analyzer": analyzer, "text": text})
	resp, err := c.Do(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, fmt.Errorf("analyze: %s", resp.ErrorReason())
	}
	var raw struct {
		Tokens []Token `json:"tokens"`
	}
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		return nil, err
	}
	return raw.Tokens, nil
}

// --- cluster settings ---

type ClusterSettings struct {
	Persistent map[string]string
	Transient  map[string]string
	Defaults   map[string]string
}

func (c *Client) ClusterSettings(ctx context.Context) (*ClusterSettings, error) {
	var raw struct {
		Persistent map[string]any `json:"persistent"`
		Transient  map[string]any `json:"transient"`
		Defaults   map[string]any `json:"defaults"`
	}
	if err := c.GetJSON(ctx, "/_cluster/settings?flat_settings=true&include_defaults=true", &raw); err != nil {
		return nil, err
	}
	return &ClusterSettings{
		Persistent: flattenValues(raw.Persistent),
		Transient:  flattenValues(raw.Transient),
		Defaults:   flattenValues(raw.Defaults),
	}, nil
}

func flattenValues(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = fmt.Sprint(v)
	}
	return out
}

var settingKeyPattern = regexp.MustCompile(`^[a-z0-9_.\-*\[\]]+$`)

// ClusterSettingPut sets a persistent cluster setting; empty value resets it.
func (c *Client) ClusterSettingPut(ctx context.Context, key, value string) error {
	if !settingKeyPattern.MatchString(key) {
		return fmt.Errorf("invalid setting key %q", key)
	}
	var v any
	if value != "" {
		v = value
	}
	body, _ := json.Marshal(map[string]any{"persistent": map[string]any{key: v}})
	resp, err := c.Do(ctx, http.MethodPut, "/_cluster/settings", bytes.NewReader(body))
	if err != nil {
		return err
	}
	if !resp.OK() {
		return fmt.Errorf("cluster setting %s: %s", key, resp.ErrorReason())
	}
	return nil
}
