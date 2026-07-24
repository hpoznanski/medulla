// Package rbac maps user roles to per-cluster permissions.
package rbac

import "github.com/hpoznanski/medulla/internal/config"

type Permission string

const (
	View          Permission = "view"
	IndexWrite    Permission = "index:write"
	AliasWrite    Permission = "alias:write"
	TemplateWrite Permission = "template:write"
	SnapshotWrite Permission = "snapshot:write"
	ClusterWrite  Permission = "cluster:write"
	RestGet       Permission = "rest:get"
	RestFull      Permission = "rest:full"
	Admin         Permission = "admin"
)

type Store struct {
	roles map[string]config.Role
}

func New(roles map[string]config.Role) *Store {
	if roles == nil {
		roles = map[string]config.Role{}
	}
	return &Store{roles: roles}
}

// Allowed reports whether any of userRoles grants perm on cluster.
// Admin implies every permission; rest:full implies rest:get.
func (s *Store) Allowed(userRoles []string, cluster string, perm Permission) bool {
	for _, name := range userRoles {
		role, ok := s.roles[name]
		if !ok {
			continue
		}
		if !coversCluster(role.Clusters, cluster) {
			continue
		}
		for _, p := range role.Permissions {
			if Permission(p) == perm || Permission(p) == Admin {
				return true
			}
			if perm == RestGet && Permission(p) == RestFull {
				return true
			}
		}
	}
	return false
}

// Clusters returns the cluster names (from all) visible to userRoles.
func (s *Store) Clusters(userRoles []string, all []string) []string {
	visible := []string{}
	for _, cl := range all {
		if s.Allowed(userRoles, cl, View) {
			visible = append(visible, cl)
		}
	}
	return visible
}

func coversCluster(list []string, cluster string) bool {
	for _, c := range list {
		if c == "*" || c == cluster {
			return true
		}
	}
	return false
}
