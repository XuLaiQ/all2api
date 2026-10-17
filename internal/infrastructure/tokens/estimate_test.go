package tokens

import (
	"testing"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func TestCountTextMatchesKnownTiktokenValues(t *testing.T) {
	// "hello world" is 2 tokens in both cl100k_base and o200k_base; the other
	// rows pin the fallback path for non-OpenAI models.
	cases := []struct {
		model string
		text  string
		want  int
	}{
		{"gpt-4", "hello world", 2},          // cl100k_base
		{"chatgpt/gpt-4o", "hello world", 2}, // o200k_base, channel prefix stripped
		{"doubao/doubao-pro", "", 0},         // unknown model falls back to o200k_base
		{"wb/unknown-model", "你好，世界", 3},     // o200k_base
	}
	for _, tc := range cases {
		if got := CountText(tc.text, tc.model); got != tc.want {
			t.Errorf("CountText(%q, %q) = %d, want %d", tc.text, tc.model, got, tc.want)
		}
	}
}

func TestEstimateAddsChatFormatOverhead(t *testing.T) {
	messages := []ports.ChatMessage{{Role: "user", Content: "hello world"}}
	prompt, completion := Estimate(messages, "hello world", "gpt-4o")
	// 3 per message + "user"(1) + "hello world"(2) + 3 reply priming.
	if prompt != 9 || completion != 2 {
		t.Fatalf("Estimate() = (%d, %d), want (9, 2)", prompt, completion)
	}
	if prompt, completion := Estimate(nil, "", "grok/grok-3"); prompt != 3 || completion != 0 {
		t.Fatalf("empty Estimate() = (%d, %d), want (3, 0)", prompt, completion)
	}
}
