package auth

import (
	"strings"
	"testing"
	"time"
)

// FuzzUnsign pins the session cookie boundary. The value is entirely
// attacker-controlled, so two things must hold for any input: it must not
// panic, and anything accepted must be exactly what Sign produces for the
// payload returned — a token the key holder could have issued. A broken HMAC
// comparison or a decode-before-verify slip fails this.
func FuzzUnsign(f *testing.F) {
	c, _, err := NewCodec("0123456789abcdef0123456789abcdef", time.Hour)
	if err != nil {
		f.Fatal(err)
	}

	valid := c.Sign([]byte(`{"u":"root","r":["admin"]}`))
	for _, seed := range []string{
		valid, valid + "x", strings.ToUpper(valid), "", ".", "..", "a.b",
		"eyJ1IjoieCJ9.", ".sig", strings.Repeat("a", 512), "\x00.\x00",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, token string) {
		payload, err := c.Unsign(token)
		if err != nil {
			return
		}
		if got := c.Sign(payload); got != token {
			t.Fatalf("accepted a token it could not have issued:\n got token %q\n re-signed %q", token, got)
		}
	})
}

// FuzzDecodeRejectsForged is the same boundary one level up: a session must
// never materialise with a user or roles from an unverified token.
func FuzzDecodeRejectsForged(f *testing.F) {
	signer, _, err := NewCodec("0123456789abcdef0123456789abcdef", time.Hour)
	if err != nil {
		f.Fatal(err)
	}
	// A codec with a different key must reject everything the signer issues.
	other, _, err := NewCodec("ffffffffffffffffffffffffffffffff", time.Hour)
	if err != nil {
		f.Fatal(err)
	}

	for _, seed := range []string{"root", "", "admin", strings.Repeat("u", 300)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, user string) {
		token, err := signer.Encode(user, []string{"admin"})
		if err != nil {
			return
		}
		if _, err := other.Decode(token); err == nil {
			t.Fatalf("session signed with one key verified under another: user %q", user)
		}
	})
}
