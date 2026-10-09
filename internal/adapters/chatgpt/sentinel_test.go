package chatgpt

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestProofTokenIsValidJSONAndUsesWebConfig(t *testing.T) {
	token, err := proofToken("seed", "ff", chatGPTDefaultUserAgent, []string{"/assets/app.js"}, "prod-build")
	if err != nil {
		t.Fatalf("proofToken() error = %v", err)
	}
	if len(token) < 8 || token[:7] != "gAAAAAB" {
		t.Fatalf("proof token prefix = %q", token[:7])
	}
	decoded, err := base64.StdEncoding.DecodeString(token[7:])
	if err != nil {
		t.Fatalf("decode proof token: %v", err)
	}
	var config []any
	if err := json.Unmarshal(decoded, &config); err != nil {
		t.Fatalf("proof token JSON = %q: %v", string(decoded), err)
	}
	if len(config) != 25 {
		t.Fatalf("proof config length = %d, want 25", len(config))
	}
}
