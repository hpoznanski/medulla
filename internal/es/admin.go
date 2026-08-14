package es

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// ValidName covers aliases, templates, repositories and snapshots — same
// conservative charset as index names.
func ValidName(name string) bool { return ValidIndexName(name) }

// PrettyJSON re-indents raw JSON, returning it unchanged when it is not JSON
// (HEAD responses, NDJSON bulk bodies, plain-text errors).
func PrettyJSON(raw []byte) string {
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
	return PrettyJSON(resp.Body), nil
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

// --- shard routing ---

// Routing is the cluster's effective shard-routing state.
type Routing struct {
	// Unknown marks a routing state that could not be read at all — distinct
	// from "read successfully and nothing is restricted".
	Unknown          bool
	AllocationEnable string   // all, primaries, new_primaries, none
	RebalanceEnable  string   // all, primaries, replicas, none
	ExcludedNodes    []string // cluster.routing.allocation.exclude._name
}

// Restricted reports whether routing is anything other than fully enabled —
// the state an operator must not forget to undo after a rolling restart. An
// unreadable state is not reported as restricted; callers surface that
// separately rather than claiming a restriction that may not exist.
func (r *Routing) Restricted() bool {
	if r.Unknown {
		return false
	}
	return r.AllocationEnable != "all" || r.RebalanceEnable != "all" || len(r.ExcludedNodes) > 0
}

const (
	allocationEnableKey = "cluster.routing.allocation.enable"
	rebalanceEnableKey  = "cluster.routing.rebalance.enable"
	excludeNameKey      = "cluster.routing.allocation.exclude._name"
)

// routingBlock mirrors the nested shape of the three routing settings. It is
// nested rather than flat because filter_path treats dots as path separators,
// so flat_settings and filter_path cannot be combined.
type routingBlock struct {
	Cluster struct {
		Routing struct {
			Allocation struct {
				Enable  string `json:"enable"`
				Exclude struct {
					Name string `json:"_name"`
				} `json:"exclude"`
			} `json:"allocation"`
			Rebalance struct {
				Enable string `json:"enable"`
			} `json:"rebalance"`
		} `json:"routing"`
	} `json:"cluster"`
}

// routingPath trims _cluster/settings to the three keys below. Unfiltered with
// include_defaults it is ~34 kB of settings this never looks at, on a page
// operators refresh constantly during an incident.
const routingPath = "/_cluster/settings?include_defaults=true&filter_path=" +
	"*.cluster.routing.allocation.enable," +
	"*.cluster.routing.rebalance.enable," +
	"*.cluster.routing.allocation.exclude"

func (c *Client) Routing(ctx context.Context) (*Routing, error) {
	var raw struct {
		Persistent routingBlock `json:"persistent"`
		Transient  routingBlock `json:"transient"`
		Defaults   routingBlock `json:"defaults"`
	}
	if err := c.GetJSON(ctx, routingPath, &raw); err != nil {
		return nil, err
	}
	// Precedence is the one ES applies: transient beats persistent beats default.
	effective := func(pick func(routingBlock) string, fallback string) string {
		for _, b := range []routingBlock{raw.Transient, raw.Persistent, raw.Defaults} {
			if v := pick(b); v != "" {
				return v
			}
		}
		return fallback
	}

	r := &Routing{
		AllocationEnable: effective(func(b routingBlock) string { return b.Cluster.Routing.Allocation.Enable }, "all"),
		RebalanceEnable:  effective(func(b routingBlock) string { return b.Cluster.Routing.Rebalance.Enable }, "all"),
	}
	excluded := effective(func(b routingBlock) string { return b.Cluster.Routing.Allocation.Exclude.Name }, "")
	for _, n := range strings.Split(excluded, ",") {
		if n = strings.TrimSpace(n); n != "" {
			r.ExcludedNodes = append(r.ExcludedNodes, n)
		}
	}
	return r, nil
}

// nodeNamePattern is deliberately narrower than ES allows: the exclusion
// setting is a comma-separated list, so a name containing a comma would
// silently exclude something else.
var nodeNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ExcludeNode adds or removes one node from the allocation exclusion list,
// preserving the other entries. Excluding a node drains every shard off it.
func (c *Client) ExcludeNode(ctx context.Context, node string, exclude bool) error {
	if !nodeNamePattern.MatchString(node) {
		return fmt.Errorf("invalid node name %q", node)
	}
	current, err := c.Routing(ctx)
	if err != nil {
		return err
	}
	list := slices.DeleteFunc(slices.Clone(current.ExcludedNodes), func(n string) bool { return n == node })
	if exclude {
		list = append(list, node)
	}
	sort.Strings(list)
	// An empty join resets the setting instead of storing an empty string.
	return c.ClusterSettingPut(ctx, excludeNameKey, strings.Join(list, ","))
}

// The two settings accept different value sets. Exported so the UI renders
// its dropdowns from the same list that validates the submission.
var (
	AllocationEnableValues = []string{"all", "primaries", "new_primaries", "none"}
	RebalanceEnableValues  = []string{"all", "primaries", "replicas", "none"}
)

// RoutingEnablePut sets cluster.routing.{allocation,rebalance}.enable.
func (c *Client) RoutingEnablePut(ctx context.Context, kind, value string) error {
	var key string
	var allowed []string
	switch kind {
	case "allocation":
		key, allowed = allocationEnableKey, AllocationEnableValues
	case "rebalance":
		key, allowed = rebalanceEnableKey, RebalanceEnableValues
	default:
		return fmt.Errorf("unknown routing kind %q", kind)
	}
	if !slices.Contains(allowed, value) {
		return fmt.Errorf("invalid %s.enable value %q", kind, value)
	}
	return c.ClusterSettingPut(ctx, key, value)
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
