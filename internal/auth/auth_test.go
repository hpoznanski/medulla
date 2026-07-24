package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/go-ldap/ldap/v3"
	"golang.org/x/crypto/bcrypt"

	"github.com/hpoznanski/medulla/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestLoginLocal(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct-horse"), bcrypt.MinCost)
	cfg := &config.Config{LocalUsers: []config.LocalUser{
		{Name: "plain", Password: "plainpass", Roles: []string{"viewer"}},
		{Name: "hashed", Password: config.Secret("bcrypt:" + string(hash)), Roles: []string{"admin"}},
	}}
	a := New(cfg, discardLogger())

	tests := []struct {
		name, user, pass string
		wantRoles        int
		wantErr          bool
	}{
		{"plaintext ok", "plain", "plainpass", 1, false},
		{"plaintext wrong", "plain", "nope", 0, true},
		{"bcrypt ok", "hashed", "correct-horse", 1, false},
		{"bcrypt wrong", "hashed", "nope", 0, true},
		{"unknown user", "ghost", "x", 0, true},
		{"empty username", "", "x", 0, true},
		{"empty password", "plain", "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roles, err := a.Login(tt.user, tt.pass)
			if tt.wantErr {
				if !errors.Is(err, ErrBadCredentials) {
					t.Errorf("err = %v, want ErrBadCredentials", err)
				}
				return
			}
			if err != nil || len(roles) != tt.wantRoles {
				t.Errorf("roles=%v err=%v", roles, err)
			}
		})
	}
}

// fakeLDAP scripts the conn behavior per test.
type fakeLDAP struct {
	serviceBindErr error
	userBindErr    error
	entries        []*ldap.Entry
	searchErr      error
	binds          int
}

func (f *fakeLDAP) Bind(dn, pw string) error {
	f.binds++
	if f.binds == 1 {
		return f.serviceBindErr
	}
	return f.userBindErr
}

func (f *fakeLDAP) Search(*ldap.SearchRequest) (*ldap.SearchResult, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return &ldap.SearchResult{Entries: f.entries}, nil
}

func (f *fakeLDAP) Close() error { return nil }

func ldapConfig() *config.Config {
	return &config.Config{
		LDAP: &config.LDAP{
			URL:        "ldap://fake",
			UserFilter: "(uid=%s)",
			GroupToRole: map[string]string{
				"cn=es-admins,dc=x": "admin",
				"cn=es-devs,dc=x":   "developer",
			},
		},
	}
}

func userEntry(groups ...string) *ldap.Entry {
	return &ldap.Entry{
		DN:         "uid=alice,dc=x",
		Attributes: []*ldap.EntryAttribute{{Name: "memberOf", Values: groups}},
	}
}

func TestLoginLDAP(t *testing.T) {
	tests := []struct {
		name      string
		fake      *fakeLDAP
		wantRoles []string
		wantErr   bool
	}{
		{"success two groups", &fakeLDAP{entries: []*ldap.Entry{userEntry("cn=es-admins,dc=x", "cn=es-devs,dc=x")}}, []string{"admin", "developer"}, false},
		{"unmapped groups only", &fakeLDAP{entries: []*ldap.Entry{userEntry("cn=other,dc=x")}}, nil, true},
		{"user not found", &fakeLDAP{}, nil, true},
		{"wrong password", &fakeLDAP{entries: []*ldap.Entry{userEntry("cn=es-admins,dc=x")}, userBindErr: errors.New("invalid credentials")}, nil, true},
		{"service bind fails", &fakeLDAP{serviceBindErr: errors.New("down")}, nil, true},
		{"search fails", &fakeLDAP{searchErr: errors.New("down")}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(ldapConfig(), discardLogger())
			a.dialLDAP = func(string) (ldapConn, error) { return tt.fake, nil }
			roles, err := a.Login("alice", "pw")
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(roles) != fmt.Sprint(tt.wantRoles) {
				t.Errorf("roles = %v, want %v", roles, tt.wantRoles)
			}
		})
	}
}

func TestLoginLDAPUnreachableFallsBackToLocal(t *testing.T) {
	cfg := ldapConfig()
	cfg.LocalUsers = []config.LocalUser{{Name: "root", Password: "pw", Roles: []string{"admin"}}}
	a := New(cfg, discardLogger())
	a.dialLDAP = func(string) (ldapConn, error) { return nil, errors.New("connection refused") }

	roles, err := a.Login("root", "pw")
	if err != nil || len(roles) != 1 {
		t.Errorf("local fallback failed: roles=%v err=%v", roles, err)
	}
}

func TestRateLimiter(t *testing.T) {
	rl := NewRateLimiter(3, 60)
	for i := range 3 {
		if !rl.Allow("1.2.3.4") {
			t.Fatalf("attempt %d blocked within burst", i)
		}
	}
	if rl.Allow("1.2.3.4") {
		t.Error("burst exceeded but allowed")
	}
	if !rl.Allow("5.6.7.8") {
		t.Error("independent key blocked")
	}
}
