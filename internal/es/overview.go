package es

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Overview aggregates the data behind the cluster overview page.
type Overview struct {
	Health HealthInfo
	Nodes  []NodeInfo
	Shards []ShardInfo
}

type HealthInfo struct {
	ClusterName      string `json:"cluster_name"`
	Status           string `json:"status"`
	NumberOfNodes    int    `json:"number_of_nodes"`
	ActiveShards     int    `json:"active_shards"`
	UnassignedShards int    `json:"unassigned_shards"`
	RelocatingShards int    `json:"relocating_shards"`
	ActivePrimary    int    `json:"active_primary_shards"`
}

type NodeInfo struct {
	Name        string
	IP          string
	Roles       []string
	Master      bool
	HeapPercent int
	DiskPercent int
	CPUPercent  int
	Load        string
}

type ShardInfo struct {
	Index   string `json:"index"`
	Shard   string `json:"shard"`
	PriRep  string `json:"prirep"`
	State   string `json:"state"`
	Node    string `json:"node"`
	Docs    string `json:"docs"`
	Store   string `json:"store"`
	Reason  string `json:"unassigned.reason"`
	Details string `json:"unassigned.details"`
	At      string `json:"unassigned.at"`
}

func (c *Client) Overview(ctx context.Context) (*Overview, error) {
	var o Overview
	if err := c.GetJSON(ctx, "/_cluster/health", &o.Health); err != nil {
		return nil, err
	}

	var cat []struct {
		Name        string `json:"name"`
		IP          string `json:"ip"`
		NodeRole    string `json:"node.role"`
		Master      string `json:"master"`
		HeapPercent string `json:"heap.percent"`
		DiskUsedPct string `json:"disk.used_percent"`
		CPU         string `json:"cpu"`
		Load1m      string `json:"load_1m"`
	}
	nodeCols := "name,ip,node.role,master,heap.percent,disk.used_percent,cpu,load_1m"
	if err := c.GetJSON(ctx, "/_cat/nodes?format=json&h="+nodeCols, &cat); err != nil {
		return nil, err
	}
	for _, n := range cat {
		o.Nodes = append(o.Nodes, NodeInfo{
			Name:        n.Name,
			IP:          n.IP,
			Roles:       []string{n.NodeRole},
			Master:      n.Master == "*",
			HeapPercent: atoi(n.HeapPercent),
			DiskPercent: atoi(n.DiskUsedPct),
			CPUPercent:  atoi(n.CPU),
			Load:        n.Load1m,
		})
	}
	sort.Slice(o.Nodes, func(i, j int) bool { return o.Nodes[i].Name < o.Nodes[j].Name })

	shardCols := "index,shard,prirep,state,node,docs,store,unassigned.reason,unassigned.details,unassigned.at"
	if err := c.GetJSON(ctx, "/_cat/shards?format=json&h="+shardCols, &o.Shards); err != nil {
		return nil, err
	}
	return &o, nil
}

// atoi parses the leading digits, tolerating _cat values like "" or "51.5"
// (fraction dropped); strconv.Atoi would reject both.
func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// AllocationExplain describes why an unassigned shard is unallocated.
type AllocationExplain struct {
	Explanation string
	RawJSON     string
}

// AllocationExplainFor explains one specific unassigned shard.
func (c *Client) AllocationExplainFor(ctx context.Context, index string, shard int, primary bool) (*AllocationExplain, error) {
	if !ValidIndexName(index) {
		return nil, fmt.Errorf("invalid index name %q", index)
	}
	body, _ := json.Marshal(map[string]any{"index": index, "shard": shard, "primary": primary})
	resp, err := c.Do(ctx, "POST", "/_cluster/allocation/explain", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, fmt.Errorf("allocation explain: %s", resp.ErrorReason())
	}
	var raw struct {
		AllocateExplanation string `json:"allocate_explanation"`
		UnassignedInfo      struct {
			Reason string `json:"reason"`
		} `json:"unassigned_info"`
		NodeDecisions []struct {
			NodeName string `json:"node_name"`
			Deciders []struct {
				Explanation string `json:"explanation"`
			} `json:"deciders"`
		} `json:"node_allocation_decisions"`
	}
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		return nil, err
	}

	// The decider explanations carry the actual cause ("a copy of this shard
	// is already allocated to this node"); allocate_explanation is boilerplate.
	seen := map[string]bool{}
	var causes []string
	for _, node := range raw.NodeDecisions {
		for _, d := range node.Deciders {
			// Drop the bracketed shard-routing internals ES appends.
			cause, _, _ := strings.Cut(d.Explanation, " [[")
			if cause != "" && !seen[cause] {
				seen[cause] = true
				causes = append(causes, fmt.Sprintf("%s: %s", node.NodeName, cause))
			}
		}
	}
	explanation := strings.Join(causes, " · ")
	if explanation == "" {
		explanation = raw.UnassignedInfo.Reason
	}
	if explanation == "" {
		explanation = raw.AllocateExplanation
	}
	return &AllocationExplain{
		Explanation: explanation,
		RawJSON:     prettyJSON(resp.Body),
	}, nil
}
