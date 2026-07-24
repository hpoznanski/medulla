package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const minimalYAML = `
clusters:
  - name: dev
    url: http://localhost:9200
roles:
  admin:
    clusters: ["*"]
    permissions: [admin]
local_users:
  - name: root
    password: hunter2hunter2
    roles: [admin]
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMinimal(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("default listen = %q, want :8080", cfg.Listen)
	}
	if cfg.Session.TTL.Hours() != 12 {
		t.Errorf("default ttl = %v, want 12h", cfg.Session.TTL)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	tests := []struct {
		name, yaml, wantErr string
	}{
		{"no clusters", `roles: {}`, "at least one cluster"},
		{"empty cluster name", `clusters: [{url: "http://x:9200"}]`, "empty name"},
		{"duplicate cluster", `clusters: [{name: a, url: "http://x:9200"}, {name: a, url: "http://y:9200"}]`, "duplicate cluster"},
		{"bad url", `clusters: [{name: a, url: "not a url"}]`, "must include scheme and host"},
		{"bad trusted proxy", minimalYAML + "\ntrusted_proxies: [\"not-a-cidr\"]", "invalid CIDR"},
		{"bad auth type", `clusters: [{name: a, url: "http://x:9200", auth: {type: kerberos}}]`, "unknown auth type"},
		{
			"bad permission",
			minimalYAML + "\n" + `extra: x`,
			"field extra not found", // strict decoding rejects unknown fields
		},
		{
			"unknown role on user",
			strings.Replace(minimalYAML, "roles: [admin]", "roles: [nope]", 1),
			`unknown role "nope"`,
		},
		{
			"no auth configured",
			`{clusters: [{name: a, url: "http://x:9200"}], roles: {r: {clusters: ["*"], permissions: [view]}}}`,
			"no authentication configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestUnknownPermissionRejected(t *testing.T) {
	bad := strings.Replace(minimalYAML, "permissions: [admin]", "permissions: [superuser]", 1)
	_, err := Load(writeConfig(t, bad))
	if err == nil || !strings.Contains(err.Error(), `unknown permission "superuser"`) {
		t.Errorf("err = %v", err)
	}
}

func TestInterpolation(t *testing.T) {
	t.Setenv("MEDULLA_TEST_PW", "s3cret")
	secretFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secretFile, []byte("filetoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	yml := fmt.Sprintf(`
clusters:
  - name: dev
    url: http://localhost:9200
    auth: {type: basic, username: u, password: "${MEDULLA_TEST_PW}"}
  - name: dev2
    url: http://localhost:9201
    auth: {type: api_key, api_key: "${file:%s}"}
roles:
  admin: {clusters: ["*"], permissions: [admin]}
local_users:
  - {name: root, password: x, roles: [admin]}
`, secretFile)

	cfg, err := Load(writeConfig(t, yml))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Clusters[0].Auth.Password.Value(); got != "s3cret" {
		t.Errorf("env interpolation = %q", got)
	}
	if got := cfg.Clusters[1].Auth.APIKey.Value(); got != "filetoken" {
		t.Errorf("file interpolation = %q (trailing newline must be stripped)", got)
	}
}

func TestInterpolationSkipsComments(t *testing.T) {
	yml := "# reference: ${MEDULLA_UNSET_VAR} and ${file:/nope}\n" + minimalYAML + "\n  # password: ${ALSO_UNSET}\n"
	if _, err := Load(writeConfig(t, yml)); err != nil {
		t.Errorf("commented ${...} references must not be interpolated: %v", err)
	}
}

func TestInterpolationMissing(t *testing.T) {
	for _, ref := range []string{"${MEDULLA_UNSET_VAR}", "${file:/nonexistent/path}"} {
		yml := strings.Replace(minimalYAML, "hunter2hunter2", ref, 1)
		if _, err := Load(writeConfig(t, yml)); err == nil {
			t.Errorf("%s: want error, got nil", ref)
		}
	}
}

func TestProductionGuard(t *testing.T) {
	prod := minimalYAML + "\nenv: production\n"
	if _, err := Load(writeConfig(t, prod)); err == nil || !strings.Contains(err.Error(), "session.secret required") {
		t.Errorf("err = %v", err)
	}

	short := prod + "session: {secret: tooshort}\n"
	if _, err := Load(writeConfig(t, short)); err == nil || !strings.Contains(err.Error(), "at least 32 bytes") {
		t.Errorf("err = %v", err)
	}

	ok := prod + "session: {secret: 0123456789abcdef0123456789abcdef}\n"
	if _, err := Load(writeConfig(t, ok)); err != nil {
		t.Errorf("valid production config rejected: %v", err)
	}
}

func TestSecretRedaction(t *testing.T) {
	s := Secret("supersecret")
	if fmt.Sprintf("%s %v %#v", s, s, s) != "[redacted] [redacted] [redacted]" {
		t.Error("fmt leaks secret")
	}
	j, _ := json.Marshal(s)
	y, _ := yaml.Marshal(s)
	for _, out := range []string{string(j), string(y)} {
		if strings.Contains(out, "supersecret") {
			t.Errorf("marshal leaks secret: %s", out)
		}
	}
	if s.Value() != "supersecret" {
		t.Error("Value() must return the real secret")
	}
}

func TestInterpolationRejectsNewlines(t *testing.T) {
	t.Setenv("MEDULLA_EVIL", "value\nlocal_users:\n  - {name: injected, password: x, roles: [admin]}")
	yml := strings.Replace(minimalYAML, "hunter2hunter2", "${MEDULLA_EVIL}", 1)
	if _, err := Load(writeConfig(t, yml)); err == nil || !strings.Contains(err.Error(), "newlines") {
		t.Errorf("newline injection not rejected: %v", err)
	}
}
