package security

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHashAndIssueAPIKey(t *testing.T) {
	input := "sk-a2a-fixture"
	digest := sha256.Sum256([]byte(input))
	if got, want := HashAPIKey(input), hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("HashAPIKey() = %q, want %q", got, want)
	}
	key, err := IssueAPIKey()
	if err != nil {
		t.Fatalf("IssueAPIKey() error = %v", err)
	}
	if !strings.HasPrefix(key, "sk-a2a-") || len(key) < 40 {
		t.Fatalf("unexpected API key format: %q", key)
	}
}

func TestSessionCookieSignatureAndLifecycle(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	manager := NewSessionManagerWithClock("session-secret-that-is-long-enough", 1, 1, func() time.Time { return now })
	cookie, expiresAt, err := manager.Create("admin", "admin")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if expiresAt != now.Unix()+86400 {
		t.Fatalf("expires_at = %d", expiresAt)
	}
	parts := strings.Split(cookie, ".")
	if len(parts) != 2 || !EqualSecret(parts[1], manager.sign(parts[0])) {
		t.Fatalf("cookie signature is not HMAC-compatible: %q", cookie)
	}
	var payload map[string]any
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(raw, &payload) != nil {
		t.Fatalf("decode cookie payload: %v", err)
	}
	if payload["username"] != "admin" || payload["role"] != "admin" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
	if _, record, ok := manager.Read(cookie); !ok || record.Username != "admin" {
		t.Fatal("Read() did not accept the issued cookie")
	}
	if _, _, ok := manager.Read(cookie + "x"); ok {
		t.Fatal("Read() accepted a tampered cookie")
	}
	manager.RevokeUser("admin")
	if _, _, ok := manager.Read(cookie); ok {
		t.Fatal("Read() accepted a revoked user session")
	}
}

func TestSessionIdleTimeout(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	manager := NewSessionManagerWithClock("session-secret-that-is-long-enough", 1, 1, func() time.Time { return now })
	cookie, _, err := manager.Create("admin", "admin")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	now = now.Add(time.Hour + time.Second)
	if _, _, ok := manager.Read(cookie); ok {
		t.Fatal("Read() accepted an idle session")
	}
}

func TestPasswordVerifierAndLoginLimiter(t *testing.T) {
	verifier := NewPasswordVerifierWithSalt("correct horse battery staple", []byte("fixed-salt-123456"))
	if !verifier.Verify("correct horse battery staple") || verifier.Verify("wrong") {
		t.Fatal("password verifier result is incorrect")
	}
	limiter := NewLoginLimiter(2)
	now := time.Unix(1_700_000_000, 0)
	if limiter.Begin("127.0.0.1", "admin", now) != 0 || limiter.Begin("127.0.0.1", "admin", now) != 0 {
		t.Fatal("limiter blocked attempts too early")
	}
	if wait := limiter.Begin("127.0.0.1", "admin", now); wait <= 0 {
		t.Fatal("limiter did not block the third attempt")
	}
	limiter.Clear("127.0.0.1", "admin")
	if limiter.Begin("127.0.0.1", "admin", now) != 0 {
		t.Fatal("limiter did not clear attempts")
	}
}

func TestOriginAndProxyRules(t *testing.T) {
	if !SameOrigin("https://console.example:443", "console.example:443") || SameOrigin("https://evil.example", "console.example:443") {
		t.Fatal("SameOrigin() returned an incorrect result")
	}
	trusted := []string{"127.0.0.1", "10.0.0.0/8"}
	if got := ClientIP("127.0.0.1", "10.1.2.3, 192.0.2.10", true, trusted); got != "192.0.2.10" {
		t.Fatalf("ClientIP() = %q", got)
	}
	if got := ClientIP("192.0.2.1", "10.1.2.3", true, trusted); got != "192.0.2.1" {
		t.Fatalf("untrusted ClientIP() = %q", got)
	}
}
