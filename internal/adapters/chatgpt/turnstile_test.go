package chatgpt

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestSolveTurnstileTokenEvaluatesEncodedInstructionList(t *testing.T) {
	tokens := []any{[]any{float64(2), float64(100), "fixture"}, []any{float64(3), float64(100)}}
	encoded, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}
	key := "turnstile-key"
	obfuscated := base64.StdEncoding.EncodeToString([]byte(xorString(string(encoded), key)))
	result, err := solveTurnstileToken(obfuscated, key)
	if err != nil || result != base64.StdEncoding.EncodeToString([]byte("fixture")) {
		t.Fatalf("solveTurnstileToken() = %q/%v", result, err)
	}
}
