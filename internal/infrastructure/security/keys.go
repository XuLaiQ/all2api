package security

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

type KeyMaterial struct{}

func NewKeyMaterial() KeyMaterial { return KeyMaterial{} }

func (KeyMaterial) Hash(value string) string { return HashAPIKey(value) }

func (KeyMaterial) Issue() (string, error) { return IssueAPIKey() }

func (KeyMaterial) Equal(left, right string) bool { return EqualSecret(left, right) }

func HashAPIKey(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func IssueAPIKey() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("generate API key: random source unavailable")
	}
	return "sk-a2a-" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func EqualSecret(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
