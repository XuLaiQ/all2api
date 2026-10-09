package crypto

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestFernetDecryptsPythonFixture(t *testing.T) {
	key := "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	token := "gAAAAABlU_EA3dKXHoyYT5_LycPowzYjlcQtoVA4i5TNDcp05M3_gOmXaW3_Z3WHg6nC2JxBNnz1G8dMQBT_7_ZZlxW3pfA8midb6y8Y__g0DhJOYE93G2g="
	fernet := NewFernet(key)
	value, err := fernet.Decrypt(token)
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	if value != "all2api fernet fixture" {
		t.Fatalf("Decrypt() = %q", value)
	}
}

func TestFernetRoundTripAndTamperDetection(t *testing.T) {
	iv := bytes.Repeat([]byte{0x11}, 16)
	fernet := NewFernetWithSource(
		"fixture-master-key",
		bytes.NewReader(iv),
		func() time.Time { return time.Unix(1_700_000_000, 0) },
	)
	token, err := fernet.Encrypt("secret payload")
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	if !strings.HasPrefix(token, "gAAAAABlU_EA") {
		t.Fatalf("unexpected Fernet token prefix: %q", token)
	}
	value, err := fernet.Decrypt(token)
	if err != nil || value != "secret payload" {
		t.Fatalf("round trip = %q, %v", value, err)
	}
	tampered := token[:len(token)-2] + "aa"
	if _, err := fernet.Decrypt(tampered); err == nil {
		t.Fatal("Decrypt() accepted a tampered token")
	}
}
