package config

import (
	"os"
	"strings"
	"testing"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// FuzzInterpolateNoInjection pins the YAML-injection guard: an interpolated
// value is spliced into config text, so a value carrying a newline could
// introduce config keys the operator never wrote. Interpolation must either
// reject the value or leave the document's line structure untouched.
func FuzzInterpolateNoInjection(f *testing.F) {
	for _, seed := range []string{
		"plain", "", "with\nnewline", "with\r\ncrlf", "trailing\n", "\n\nleading",
		"admin: {clusters: [\"*\"]}", "${NESTED}", "a: b\nc: d", "\r", "\x00",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		// Exercised through ${file:...} rather than ${ENV}: a file can hold
		// bytes an environment variable cannot (os.Setenv rejects NUL), so
		// this covers strictly more of the input space, and it is the branch
		// the docs point at for multiline secrets.
		path := t.TempDir() + "/secret"
		if err := writeFile(path, value); err != nil {
			t.Skip()
		}
		doc := "listen: \":8080\"\nsecret: ${file:" + path + "}\nenv: production\n"
		wantLines := strings.Count(doc, "\n")

		out, err := interpolate(doc)
		if err != nil {
			return // rejected, which is the guard doing its job
		}
		if got := strings.Count(out, "\n"); got != wantLines {
			t.Fatalf("value %q changed the document from %d lines to %d:\n%s",
				value, wantLines, got, out)
		}
		if strings.Contains(out, "\r") {
			t.Fatalf("value %q introduced a carriage return:\n%q", value, out)
		}
	})
}

// FuzzInterpolateEnvNoInjection covers the ${ENV} branch, which has its own
// trimming. NUL is filtered because os.Setenv refuses it, so it cannot reach
// this code path in production either.
func FuzzInterpolateEnvNoInjection(f *testing.F) {
	for _, seed := range []string{"plain", "", "with\nnewline", "trailing\n", "a: b\nc: d"} {
		f.Add(seed)
	}
	const doc = "listen: \":8080\"\nsecret: ${MEDULLA_FUZZ}\n"
	wantLines := strings.Count(doc, "\n")

	f.Fuzz(func(t *testing.T, value string) {
		if strings.ContainsRune(value, 0) {
			t.Skip()
		}
		t.Setenv("MEDULLA_FUZZ", value)

		out, err := interpolate(doc)
		if err != nil {
			return
		}
		if got := strings.Count(out, "\n"); got != wantLines {
			t.Fatalf("value %q changed the document from %d lines to %d:\n%s",
				value, wantLines, got, out)
		}
	})
}

// FuzzLoadNeverPanics feeds arbitrary YAML through the whole load path. A
// malformed config must be an error, never a crash — this runs at startup
// before anything is serving, but a panic loop on a bad ConfigMap is a much
// worse failure mode than a clear message.
func FuzzLoadNeverPanics(f *testing.F) {
	for _, seed := range []string{
		"", "clusters: []", "clusters:\n  - name: a\n    url: http://x:9200\n",
		"roles:\n  r: {clusters: [\"*\"], permissions: [admin]}\n",
		"session:\n  ttl: notaduration\n", "\t", "a:\n- b\n  c", "listen: [1,2]",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		path := t.TempDir() + "/config.yaml"
		if err := writeFile(path, doc); err != nil {
			t.Skip()
		}
		_, _ = Load(path) // must return, panic is the failure
	})
}
