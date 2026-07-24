package rbac

import (
	"fmt"
	"testing"

	"github.com/hpoznanski/medulla/internal/config"
)

func testStore() *Store {
	return New(map[string]config.Role{
		"admin":    {Clusters: []string{"*"}, Permissions: []string{"admin"}},
		"operator": {Clusters: []string{"prod"}, Permissions: []string{"view", "index:write", "rest:full"}},
		"dev-read": {Clusters: []string{"dev", "staging"}, Permissions: []string{"view", "rest:get"}},
	})
}

func TestAllowed(t *testing.T) {
	s := testStore()
	tests := []struct {
		roles   []string
		cluster string
		perm    Permission
		want    bool
	}{
		// admin implies everything, everywhere
		{[]string{"admin"}, "prod", View, true},
		{[]string{"admin"}, "prod", SnapshotWrite, true},
		{[]string{"admin"}, "anything", RestFull, true},
		// operator scoped to prod
		{[]string{"operator"}, "prod", IndexWrite, true},
		{[]string{"operator"}, "dev", IndexWrite, false},
		{[]string{"operator"}, "prod", SnapshotWrite, false},
		// rest:full implies rest:get, not vice versa
		{[]string{"operator"}, "prod", RestGet, true},
		{[]string{"dev-read"}, "dev", RestFull, false},
		{[]string{"dev-read"}, "dev", RestGet, true},
		// cluster list scoping
		{[]string{"dev-read"}, "staging", View, true},
		{[]string{"dev-read"}, "prod", View, false},
		// multiple roles union
		{[]string{"dev-read", "operator"}, "prod", View, true},
		// unknown role, no roles
		{[]string{"ghost"}, "prod", View, false},
		{nil, "prod", View, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%v/%s/%s", tt.roles, tt.cluster, tt.perm), func(t *testing.T) {
			if got := s.Allowed(tt.roles, tt.cluster, tt.perm); got != tt.want {
				t.Errorf("Allowed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClusters(t *testing.T) {
	s := testStore()
	all := []string{"prod", "dev", "staging"}

	got := s.Clusters([]string{"dev-read"}, all)
	if fmt.Sprint(got) != "[dev staging]" {
		t.Errorf("Clusters() = %v", got)
	}
	if got := s.Clusters([]string{"admin"}, all); len(got) != 3 {
		t.Errorf("admin sees %v", got)
	}
	if got := s.Clusters(nil, all); len(got) != 0 {
		t.Errorf("no roles sees %v", got)
	}
}

func TestNewNilRoles(t *testing.T) {
	if New(nil).Allowed([]string{"any"}, "c", View) {
		t.Error("nil roles store must deny")
	}
}
