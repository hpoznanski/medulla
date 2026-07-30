// Package auth authenticates users against LDAP and local accounts and
// manages stateless signed-cookie sessions.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
	"golang.org/x/crypto/bcrypt"

	"github.com/hpoznanski/medulla/internal/config"
)

var ErrBadCredentials = errors.New("invalid username or password")

type Authenticator struct {
	ldap   *config.LDAP
	local  []config.LocalUser
	logger *slog.Logger

	// dialLDAP is swapped in tests.
	dialLDAP func(url string) (ldapConn, error)
}

type ldapConn interface {
	Bind(username, password string) error
	Search(*ldap.SearchRequest) (*ldap.SearchResult, error)
	Close() error
}

func New(cfg *config.Config, logger *slog.Logger) *Authenticator {
	// Hash plaintext local passwords at startup so every comparison path is
	// bcrypt — uniform verify timing blocks user enumeration via fast-path
	// plaintext compares, and the plaintext never sits in memory afterwards.
	local := make([]config.LocalUser, len(cfg.LocalUsers))
	copy(local, cfg.LocalUsers)
	for i, u := range local {
		if !strings.HasPrefix(u.Password.Value(), "bcrypt:") {
			hash, err := bcrypt.GenerateFromPassword([]byte(u.Password.Value()), bcrypt.DefaultCost)
			if err != nil {
				logger.Error("hashing local user password failed", "user", u.Name, "err", err)
				continue
			}
			local[i].Password = config.Secret("bcrypt:" + string(hash))
		}
	}

	return &Authenticator{
		ldap:   cfg.LDAP,
		local:  local,
		logger: logger,
		dialLDAP: func(url string) (ldapConn, error) {
			conn, err := ldap.DialURL(url, ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}))
			if err != nil {
				return nil, err
			}
			conn.SetTimeout(10 * time.Second)
			return conn, nil
		},
	}
}

// Login verifies credentials against LDAP first (when configured), then local
// users. It returns the resolved role names.
func (a *Authenticator) Login(username, password string) ([]string, error) {
	if username == "" || password == "" {
		return nil, ErrBadCredentials
	}
	if a.ldap != nil {
		roles, err := a.loginLDAP(username, password)
		if err == nil {
			return roles, nil
		}
		if !errors.Is(err, ErrBadCredentials) {
			a.logger.Error("ldap login failed", "err", err)
		}
	}
	return a.loginLocal(username, password)
}

func (a *Authenticator) loginLDAP(username, password string) ([]string, error) {
	conn, err := a.dialLDAP(a.ldap.URL)
	if err != nil {
		return nil, fmt.Errorf("ldap dial: %w", err)
	}
	defer conn.Close()

	if err := conn.Bind(a.ldap.BindDN, a.ldap.BindPassword.Value()); err != nil {
		return nil, fmt.Errorf("ldap service bind: %w", err)
	}

	filter := fmt.Sprintf(a.ldap.UserFilter, ldap.EscapeFilter(username))
	res, err := conn.Search(ldap.NewSearchRequest(
		a.ldap.UserBase,
		ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases,
		1,
		0,
		false,
		filter,
		[]string{"dn", "memberOf"},
		nil,
	))
	if err != nil {
		return nil, fmt.Errorf("ldap search: %w", err)
	}
	if len(res.Entries) != 1 {
		return nil, ErrBadCredentials
	}
	entry := res.Entries[0]

	if err := conn.Bind(entry.DN, password); err != nil {
		return nil, ErrBadCredentials
	}

	roles := []string{}
	for _, group := range entry.GetAttributeValues("memberOf") {
		if role, ok := a.ldap.GroupToRole[group]; ok {
			roles = append(roles, role)
		}
	}
	if role, ok := a.ldap.UserToRole[username]; ok && !slices.Contains(roles, role) {
		roles = append(roles, role)
	}
	if len(roles) == 0 {
		return nil, ErrBadCredentials // authenticated but no mapped role: no access
	}
	return roles, nil
}

func (a *Authenticator) loginLocal(username, password string) ([]string, error) {
	for _, u := range a.local {
		if u.Name != username {
			continue
		}
		if verifyPassword(u.Password.Value(), password) {
			return u.Roles, nil
		}
		return nil, ErrBadCredentials
	}
	// Burn comparable time for unknown users to blunt username enumeration.
	verifyPassword("bcrypt:$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy", password)
	return nil, ErrBadCredentials
}

func verifyPassword(stored, given string) bool {
	// All stored passwords are bcrypt by the time they reach here (New hashes
	// plaintext at startup); the fallback covers only a failed startup hash.
	if hash, ok := strings.CutPrefix(stored, "bcrypt:"); ok {
		return bcrypt.CompareHashAndPassword([]byte(hash), []byte(given)) == nil
	}
	a := sha256.Sum256([]byte(stored))
	b := sha256.Sum256([]byte(given))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
