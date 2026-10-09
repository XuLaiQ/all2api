package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const fernetVersion byte = 0x80

var ErrInvalidFernetToken = errors.New("invalid Fernet token")

type Fernet struct {
	signingKey    []byte
	encryptionKey []byte
	random        io.Reader
	clock         func() time.Time
}

var _ interface {
	Encrypt(string) (string, error)
	Decrypt(string) (string, error)
} = (*Fernet)(nil)

func NewFernet(masterKey string) *Fernet {
	key := deriveKey(masterKey)
	return &Fernet{
		signingKey:    append([]byte(nil), key[:16]...),
		encryptionKey: append([]byte(nil), key[16:]...),
		random:        rand.Reader,
		clock:         time.Now,
	}
}

func NewFernetWithSource(masterKey string, random io.Reader, clock func() time.Time) *Fernet {
	fernet := NewFernet(masterKey)
	if random != nil {
		fernet.random = random
	}
	if clock != nil {
		fernet.clock = clock
	}
	return fernet
}

func (f *Fernet) Encrypt(value string) (string, error) {
	block, err := aes.NewCipher(f.encryptionKey)
	if err != nil {
		return "", fmt.Errorf("create Fernet cipher: %w", err)
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(f.random, iv); err != nil {
		return "", fmt.Errorf("generate Fernet IV: %w", err)
	}
	padded := pkcs7Pad([]byte(value), aes.BlockSize)
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)

	token := make([]byte, 0, 1+8+len(iv)+len(ciphertext)+sha256.Size)
	token = append(token, fernetVersion)
	var timestamp [8]byte
	binary.BigEndian.PutUint64(timestamp[:], uint64(f.clock().Unix()))
	token = append(token, timestamp[:]...)
	token = append(token, iv...)
	token = append(token, ciphertext...)
	token = append(token, f.sign(token)...)
	return base64.URLEncoding.EncodeToString(token), nil
}

func (f *Fernet) Decrypt(value string) (string, error) {
	decoded, err := decodeBase64URL(value)
	if err != nil || len(decoded) < 1+8+aes.BlockSize+aes.BlockSize+sha256.Size {
		return "", ErrInvalidFernetToken
	}
	if decoded[0] != fernetVersion {
		return "", ErrInvalidFernetToken
	}
	messageEnd := len(decoded) - sha256.Size
	expected := f.sign(decoded[:messageEnd])
	if subtle.ConstantTimeCompare(expected, decoded[messageEnd:]) != 1 {
		return "", ErrInvalidFernetToken
	}
	ivStart := 1 + 8
	iv := decoded[ivStart : ivStart+aes.BlockSize]
	ciphertext := decoded[ivStart+aes.BlockSize : messageEnd]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return "", ErrInvalidFernetToken
	}
	block, err := aes.NewCipher(f.encryptionKey)
	if err != nil {
		return "", ErrInvalidFernetToken
	}
	plaintext := make([]byte, len(ciphertext))
	decrypted := cipher.NewCBCDecrypter(block, iv)
	decrypted.CryptBlocks(plaintext, ciphertext)
	plaintext, ok := pkcs7Unpad(plaintext, aes.BlockSize)
	if !ok {
		return "", ErrInvalidFernetToken
	}
	return string(plaintext), nil
}

func deriveKey(masterKey string) []byte {
	raw := strings.TrimSpace(masterKey)
	if raw != "" {
		if decoded, err := decodeBase64URL(raw); err == nil && len(decoded) == 32 {
			return decoded
		}
	}
	seed := raw
	if seed == "" {
		seed = "all2api-development-credential-key"
	}
	digest := sha256.Sum256([]byte(seed))
	return digest[:]
}

func (f *Fernet) sign(value []byte) []byte {
	mac := hmac.New(sha256.New, f.signingKey)
	_, _ = mac.Write(value)
	return mac.Sum(nil)
}

func decodeBase64URL(value string) ([]byte, error) {
	if decoded, err := base64.URLEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(value, "="))
}

func pkcs7Pad(value []byte, blockSize int) []byte {
	padding := blockSize - len(value)%blockSize
	result := append([]byte(nil), value...)
	for index := 0; index < padding; index++ {
		result = append(result, byte(padding))
	}
	return result
}

func pkcs7Unpad(value []byte, blockSize int) ([]byte, bool) {
	if len(value) == 0 || len(value)%blockSize != 0 {
		return nil, false
	}
	padding := int(value[len(value)-1])
	if padding == 0 || padding > blockSize || padding > len(value) {
		return nil, false
	}
	for _, item := range value[len(value)-padding:] {
		if int(item) != padding {
			return nil, false
		}
	}
	return value[:len(value)-padding], true
}
