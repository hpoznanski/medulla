package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrInvalidSession = errors.New("invalid session")

type Session struct {
	User    string    `json:"u"`
	Roles   []string  `json:"r"`
	Expires time.Time `json:"e"`
}

// Codec signs and verifies session cookies. Multiple keys enable rotation:
// tokens are signed with keys[0] and verified against every key.
type Codec struct {
	keys [][]byte
	ttl  time.Duration
}

// NewCodec builds a Codec from a comma-separated key list. An empty secret
// generates a random ephemeral key (dev only — sessions die on restart).
func NewCodec(secret string, ttl time.Duration) (*Codec, bool, error) {
	if secret == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, false, fmt.Errorf("generating session key: %w", err)
		}
		return &Codec{keys: [][]byte{key}, ttl: ttl}, true, nil
	}
	var keys [][]byte
	for _, k := range strings.Split(secret, ",") {
		if k == "" {
			return nil, false, errors.New("session secret contains empty key")
		}
		keys = append(keys, []byte(k))
	}
	return &Codec{keys: keys, ttl: ttl}, false, nil
}

func (c *Codec) Encode(user string, roles []string) (string, error) {
	payload, err := json.Marshal(Session{User: user, Roles: roles, Expires: time.Now().Add(c.ttl)})
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + c.sign(body, c.keys[0]), nil
}

func (c *Codec) Decode(token string) (Session, error) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return Session{}, ErrInvalidSession
	}
	verified := false
	for _, key := range c.keys {
		if hmac.Equal([]byte(sig), []byte(c.sign(body, key))) {
			verified = true
			break
		}
	}
	if !verified {
		return Session{}, ErrInvalidSession
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Session{}, ErrInvalidSession
	}
	var s Session
	if err := json.Unmarshal(payload, &s); err != nil {
		return Session{}, ErrInvalidSession
	}
	if time.Now().After(s.Expires) {
		return Session{}, ErrInvalidSession
	}
	return s, nil
}

func (c *Codec) sign(body string, key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
