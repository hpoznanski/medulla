package auth

import (
	"strings"
	"testing"
	"time"
)

func TestCodecRoundTrip(t *testing.T) {
	c, ephemeral, err := NewCodec("0123456789abcdef0123456789abcdef", time.Hour)
	if err != nil || ephemeral {
		t.Fatalf("err=%v ephemeral=%v", err, ephemeral)
	}
	token, err := c.Encode("alice", []string{"operator", "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := c.Decode(token)
	if err != nil {
		t.Fatal(err)
	}
	if sess.User != "alice" || len(sess.Roles) != 2 {
		t.Errorf("got %+v", sess)
	}
}

func TestCodecRejectsTampering(t *testing.T) {
	c, _, _ := NewCodec("0123456789abcdef0123456789abcdef", time.Hour)
	token, _ := c.Encode("alice", []string{"viewer"})

	tests := map[string]string{
		"no separator":     strings.ReplaceAll(token, ".", ""),
		"tampered payload": "x" + token,
		"tampered sig":     token[:len(token)-1] + "x",
		"empty":            "",
		"garbage":          "not.a.token",
	}
	for name, bad := range tests {
		if _, err := c.Decode(bad); err == nil {
			t.Errorf("%s: accepted invalid token", name)
		}
	}
}

func TestCodecRejectsExpired(t *testing.T) {
	c, _, _ := NewCodec("0123456789abcdef0123456789abcdef", -time.Minute)
	token, _ := c.Encode("alice", nil)
	if _, err := c.Decode(token); err == nil {
		t.Error("accepted expired session")
	}
}

func TestCodecKeyRotation(t *testing.T) {
	oldCodec, _, _ := NewCodec("oldkey-0123456789abcdef012345678", time.Hour)
	oldToken, _ := oldCodec.Encode("alice", nil)

	rotated, _, _ := NewCodec("newkey-0123456789abcdef012345678,oldkey-0123456789abcdef012345678", time.Hour)
	if _, err := rotated.Decode(oldToken); err != nil {
		t.Error("old-key token rejected during rotation")
	}
	newToken, _ := rotated.Encode("alice", nil)
	newOnly, _, _ := NewCodec("newkey-0123456789abcdef012345678", time.Hour)
	if _, err := newOnly.Decode(newToken); err != nil {
		t.Error("rotated codec must sign with first key")
	}
}

func TestCodecEphemeral(t *testing.T) {
	c1, ephemeral, err := NewCodec("", time.Hour)
	if err != nil || !ephemeral {
		t.Fatalf("err=%v ephemeral=%v", err, ephemeral)
	}
	c2, _, _ := NewCodec("", time.Hour)
	token, _ := c1.Encode("alice", nil)
	if _, err := c2.Decode(token); err == nil {
		t.Error("distinct ephemeral codecs must not verify each other's tokens")
	}
}

func TestCodecEmptyKeyRejected(t *testing.T) {
	if _, _, err := NewCodec("valid-key-0123456789abcdef01234,", time.Hour); err == nil {
		t.Error("trailing empty key accepted")
	}
}
