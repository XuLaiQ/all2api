package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
)

const PasswordHashIterations = 310_000

type PasswordVerifier struct {
	salt       []byte
	digest     []byte
	iterations int
}

func NewPasswordVerifier(password string) (*PasswordVerifier, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate password salt: %w", err)
	}
	return NewPasswordVerifierWithSalt(password, salt), nil
}

func NewPasswordVerifierWithSalt(password string, salt []byte) *PasswordVerifier {
	copySalt := append([]byte(nil), salt...)
	return &PasswordVerifier{
		salt:       copySalt,
		digest:     pbkdf2SHA256([]byte(password), copySalt, PasswordHashIterations, sha256.Size),
		iterations: PasswordHashIterations,
	}
}

func (p *PasswordVerifier) Verify(password string) bool {
	if p == nil {
		return false
	}
	supplied := pbkdf2SHA256([]byte(password), p.salt, p.iterations, len(p.digest))
	return subtle.ConstantTimeCompare(supplied, p.digest) == 1
}

func pbkdf2SHA256(password, salt []byte, iterations, keyLength int) []byte {
	result := make([]byte, 0, keyLength)
	for block := uint32(1); len(result) < keyLength; block++ {
		mac := hmac.New(sha256.New, password)
		_, _ = mac.Write(salt)
		var counter [4]byte
		binary.BigEndian.PutUint32(counter[:], block)
		_, _ = mac.Write(counter[:])
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for iteration := 1; iteration < iterations; iteration++ {
			mac = hmac.New(sha256.New, password)
			_, _ = mac.Write(u)
			u = mac.Sum(nil)
			for index := range t {
				t[index] ^= u[index]
			}
		}
		result = append(result, t...)
	}
	return result[:keyLength]
}
