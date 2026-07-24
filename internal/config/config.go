// Package config loads and validates Medulla's YAML configuration.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Secret is a string that redacts itself in any formatted or marshaled output.
type Secret string

func (Secret) String() string               { return "[redacted]" }
func (Secret) GoString() string             { return "[redacted]" }
func (Secret) MarshalYAML() (any, error)    { return "[redacted]", nil }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (s Secret) Value() string              { return string(s) }

type Config struct {
	Listen     string          `yaml:"listen"`
	Env        string          `yaml:"env"`
	Clusters   []Cluster       `yaml:"clusters"`
	Roles      map[string]Role `yaml:"roles"`
	LDAP       *LDAP           `yaml:"ldap"`
	LocalUsers []LocalUser     `yaml:"local_users"`
	Session    Session         `yaml:"session"`

	// TrustedProxies lists CIDRs of reverse proxies (K8s ingress) whose
	// X-Forwarded-For may be used to derive the real client IP.
	TrustedProxies []string `yaml:"trusted_proxies"`
}

type Cluster struct {
	Name string      `yaml:"name"`
	URL  string      `yaml:"url"`
	Auth ClusterAuth `yaml:"auth"`
	TLS  TLS         `yaml:"tls"`
}

type ClusterAuth struct {
	Type     string `yaml:"type"` // basic, api_key, none
	Username string `yaml:"username"`
	Password Secret `yaml:"password"`
	APIKey   Secret `yaml:"api_key"`
}

type TLS struct {
	CAFile   string `yaml:"ca_file"`
	Insecure bool   `yaml:"insecure"`
}

type Role struct {
	Clusters    []string `yaml:"clusters"`
	Permissions []string `yaml:"permissions"`
}

type LDAP struct {
	URL          string            `yaml:"url"`
	BindDN       string            `yaml:"bind_dn"`
	BindPassword Secret            `yaml:"bind_password"`
	UserBase     string            `yaml:"user_base"`
	UserFilter   string            `yaml:"user_filter"` // default (uid=%s)
	GroupToRole  map[string]string `yaml:"group_to_role"`
}

type LocalUser struct {
	Name     string   `yaml:"name"`
	Password Secret   `yaml:"password"` // plaintext or "bcrypt:$2..."
	Roles    []string `yaml:"roles"`
}

type Session struct {
	Secret Secret        `yaml:"secret"` // comma-separated keys: sign with first, verify all
	TTL    time.Duration `yaml:"ttl"`

	// InsecureCookie disables the cookie Secure flag. Secure is the default;
	// opt out only for plain-HTTP dev setups.
	InsecureCookie bool `yaml:"insecure_cookie"`
}

var validPermissions = map[string]bool{
	"view": true, "index:write": true, "alias:write": true, "template:write": true,
	"snapshot:write": true, "cluster:write": true, "rest:get": true, "rest:full": true, "admin": true,
}

// Load reads path, interpolates ${VAR} and ${file:/path} references, and validates.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	interpolated, err := interpolate(string(raw))
	if err != nil {
		return nil, err
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(interpolated))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var interpolationPattern = regexp.MustCompile(`\$\{(file:)?([^}]+)\}`)

func interpolate(raw string) (string, error) {
	var errs []string
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue // full-line comments keep their ${...} references verbatim
		}
		var lineErrs []string
		lines[i], lineErrs = interpolateLine(line)
		errs = append(errs, lineErrs...)
	}
	if len(errs) > 0 {
		return "", fmt.Errorf("config interpolation: %s", strings.Join(errs, "; "))
	}
	return strings.Join(lines, "\n"), nil
}

func interpolateLine(line string) (string, []string) {
	var errs []string
	out := interpolationPattern.ReplaceAllStringFunc(line, func(match string) string {
		groups := interpolationPattern.FindStringSubmatch(match)
		var val string
		if groups[1] == "file:" {
			content, err := os.ReadFile(groups[2])
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", match, err))
				return match
			}
			val = strings.TrimRight(string(content), "\r\n")
		} else {
			v, ok := os.LookupEnv(groups[2])
			if !ok {
				errs = append(errs, fmt.Sprintf("%s: environment variable not set", match))
				return match
			}
			val = strings.TrimRight(v, "\r\n")
		}
		// Values are spliced into YAML text: embedded newlines could inject
		// config keys. Multiline secrets must be mounted as files and
		// referenced by path instead.
		if strings.ContainsAny(val, "\n\r") {
			errs = append(errs, fmt.Sprintf("%s: value contains newlines (mount multiline secrets as files)", match))
			return match
		}
		return val
	})
	return out, errs
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Env == "" {
		c.Env = os.Getenv("MEDULLA_ENV")
	}
	if c.Session.TTL == 0 {
		c.Session.TTL = 12 * time.Hour
	}
	if c.LDAP != nil && c.LDAP.UserFilter == "" {
		c.LDAP.UserFilter = "(uid=%s)"
	}
}

func (c *Config) validate() error {
	if len(c.Clusters) == 0 {
		return fmt.Errorf("config: at least one cluster required")
	}
	seen := map[string]bool{}
	for _, cl := range c.Clusters {
		if cl.Name == "" {
			return fmt.Errorf("config: cluster with empty name")
		}
		if seen[cl.Name] {
			return fmt.Errorf("config: duplicate cluster name %q", cl.Name)
		}
		seen[cl.Name] = true
		u, err := url.Parse(cl.URL)
		if err != nil {
			return fmt.Errorf("config: cluster %q: invalid url %q: %w", cl.Name, cl.URL, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("config: cluster %q: url %q must include scheme and host", cl.Name, cl.URL)
		}
		switch cl.Auth.Type {
		case "", "none", "basic", "api_key":
		default:
			return fmt.Errorf("config: cluster %q: unknown auth type %q", cl.Name, cl.Auth.Type)
		}
	}

	for name, role := range c.Roles {
		for _, p := range role.Permissions {
			if !validPermissions[p] {
				return fmt.Errorf("config: role %q: unknown permission %q", name, p)
			}
		}
		for _, cl := range role.Clusters {
			if cl != "*" && !seen[cl] {
				return fmt.Errorf("config: role %q: unknown cluster %q", name, cl)
			}
		}
	}

	for _, u := range c.LocalUsers {
		if u.Name == "" || u.Password == "" {
			return fmt.Errorf("config: local user with empty name or password")
		}
		for _, r := range u.Roles {
			if _, ok := c.Roles[r]; !ok {
				return fmt.Errorf("config: local user %q: unknown role %q", u.Name, r)
			}
		}
	}
	if c.LDAP != nil {
		for group, role := range c.LDAP.GroupToRole {
			if _, ok := c.Roles[role]; !ok {
				return fmt.Errorf("config: ldap group %q: unknown role %q", group, role)
			}
		}
	}
	if c.LDAP == nil && len(c.LocalUsers) == 0 {
		return fmt.Errorf("config: no authentication configured (ldap or local_users)")
	}

	for _, cidr := range c.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("config: trusted_proxies: invalid CIDR %q: %w", cidr, err)
		}
	}

	if c.Env == "production" {
		if c.Session.Secret == "" {
			return fmt.Errorf("config: session.secret required when env is production")
		}
		for _, key := range strings.Split(c.Session.Secret.Value(), ",") {
			if len(key) < 32 {
				return fmt.Errorf("config: session.secret keys must be at least 32 bytes")
			}
		}
	}
	return nil
}
