package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

const (
	SessionCookieName = "a2a_session"
	SessionVersion    = 1
	maxCookieLength   = 2048
)

type SessionManager struct {
	secret      []byte
	sessionDays int
	idleHours   int
	now         func() time.Time

	mu       sync.Mutex
	sessions map[string]ports.SessionRecord
	versions map[string]int
}

var _ ports.SessionManager = (*SessionManager)(nil)

func NewSessionManager(secret string, sessionDays, idleHours int) *SessionManager {
	return NewSessionManagerWithClock(secret, sessionDays, idleHours, time.Now)
}

func NewSessionManagerWithClock(secret string, sessionDays, idleHours int, now func() time.Time) *SessionManager {
	return &SessionManager{
		secret:      []byte(secret),
		sessionDays: sessionDays,
		idleHours:   idleHours,
		now:         now,
		sessions:    make(map[string]ports.SessionRecord),
		versions:    make(map[string]int),
	}
}

func (s *SessionManager) Create(username, role string) (string, int64, error) {
	if username == "" || role == "" {
		return "", 0, fmt.Errorf("session identity must not be empty")
	}
	now := s.now().Unix()
	expiresAt := now + int64(s.sessionDays)*86400
	sessionID, err := randomToken(32)
	if err != nil {
		return "", 0, err
	}
	s.mu.Lock()
	version := s.versions[username]
	if version == 0 {
		version = SessionVersion
	}
	s.sessions[sessionID] = ports.SessionRecord{
		Username:       username,
		Role:           role,
		SessionVersion: version,
		ExpiresAt:      expiresAt,
		LastSeenAt:     now,
	}
	for id, record := range s.sessions {
		if record.ExpiresAt <= now {
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()

	payload, err := json.Marshal(struct {
		SID      string `json:"sid"`
		Username string `json:"username"`
		Role     string `json:"role"`
		Version  int    `json:"sv"`
		Expires  int64  `json:"exp"`
	}{sessionID, username, role, version, expiresAt})
	if err != nil {
		return "", 0, fmt.Errorf("encode session: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + s.sign(encoded), expiresAt, nil
}

func (s *SessionManager) Read(cookie string) (string, ports.SessionRecord, bool) {
	if len(cookie) == 0 || len(cookie) > maxCookieLength {
		return "", ports.SessionRecord{}, false
	}
	payload, signature, ok := strings.Cut(cookie, ".")
	if !ok || payload == "" || signature == "" || strings.Contains(signature, ".") || !isBase64URL(payload) || !isBase64URL(signature) || !EqualSecret(signature, s.sign(payload)) {
		return "", ports.SessionRecord{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", ports.SessionRecord{}, false
	}
	var value struct {
		SID      string `json:"sid"`
		Username string `json:"username"`
		Role     string `json:"role"`
		Version  int    `json:"sv"`
		Expires  int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &value) != nil || value.SID == "" || value.Username == "" || value.Role == "" || value.Version <= 0 || value.Expires <= 0 {
		return "", ports.SessionRecord{}, false
	}

	now := s.now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.sessions[value.SID]
	expectedVersion := s.versions[value.Username]
	if expectedVersion == 0 {
		expectedVersion = SessionVersion
	}
	if !exists || value.Expires <= now || now-record.LastSeenAt > int64(s.idleHours)*3600 || value.Version != expectedVersion || record.Username != value.Username || record.Role != value.Role || record.SessionVersion != value.Version || record.ExpiresAt != value.Expires {
		delete(s.sessions, value.SID)
		return "", ports.SessionRecord{}, false
	}
	record.LastSeenAt = now
	s.sessions[value.SID] = record
	return value.SID, record, true
}

func (s *SessionManager) Revoke(cookie string) {
	sessionID, _, ok := s.Read(cookie)
	if !ok {
		return
	}
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

func (s *SessionManager) RevokeUser(username string) {
	s.mu.Lock()
	s.versions[username]++
	if s.versions[username] == 1 {
		s.versions[username] = SessionVersion + 1
	}
	for sessionID, record := range s.sessions {
		if record.Username == username {
			delete(s.sessions, sessionID)
		}
	}
	s.mu.Unlock()
}

func (s *SessionManager) sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func randomToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func isBase64URL(value string) bool {
	for _, char := range value {
		if (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}
