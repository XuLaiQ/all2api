package chatgpt

import (
	"strings"
	"testing"
)

func feed(parser *conversationParser, payloads ...string) string {
	var builder strings.Builder
	for _, payload := range payloads {
		delta, _ := parser.handle(payload)
		builder.WriteString(delta)
	}
	return builder.String()
}

func TestConversationParserDeltaEncoding(t *testing.T) {
	parser := newConversationParser(nil)
	got := feed(parser,
		`{"p":"","o":"add","v":{"message":{"id":"m1","author":{"role":"assistant"},"content":{"parts":[""]}}}}`,
		`{"p":"/message/content/parts/0","o":"append","v":"Hel"}`,
		`{"v":"lo"}`,
		`{"v":"lo"}`, // repeated tokens are real output
		`{"p":"/message/status","o":"replace","v":"in_progress"}`,
		`{"v":"ignored: continues the status path"}`,
		// Bare operation list without "o":"patch".
		`{"v":[{"p":"/message/content/parts/0","o":"append","v":" world"},{"p":"/message/status","o":"replace","v":"finished_successfully"}]}`,
		`{"o":"patch","v":[{"p":"/message/content/parts/0","o":"append","v":"!"}]}`,
		// Hidden reasoning / tool messages never leak.
		`{"message":{"author":{"role":"assistant"},"channel":"analysis","content":{"parts":["secret"]}}}`,
		`{"message":{"author":{"role":"assistant"},"recipient":"web","content":{"parts":["search()"]}}}`,
	)
	if got != "Hellolo world!" || parser.messageID != "m1" {
		t.Fatalf("text = %q id = %q", got, parser.messageID)
	}
	if _, done := parser.handle("[DONE]"); !done {
		t.Fatal("[DONE] must end the stream")
	}
}

func TestConversationParserStripsReplayedHistory(t *testing.T) {
	parser := newConversationParser([]string{"First answer."})
	got := feed(parser,
		// Upstream replays the assistant history before answering.
		`{"message":{"author":{"role":"assistant"},"content":{"parts":["First answer."]}}}`,
		`{"message":{"author":{"role":"assistant"},"content":{"parts":["First answer.Second"]}}}`,
		`{"p":"/message/content/parts/0","o":"append","v":" answer"}`,
	)
	if got != "Second answer" {
		t.Fatalf("text = %q", got)
	}
}

func TestSanitizeOutputTextAnnotations(t *testing.T) {
	cases := map[string]string{
		"See \ue200url\ue202OpenAI\ue202https://openai.com\ue201 now": "See OpenAI (https://openai.com) now",
		"Fact \ue200cite\ue202turn0search1\ue201.":                    "Fact.",
		"Name \ue200entity\ue202turn0\ue202Paris\ue201 is":            "Name Paris is",
		"Pending \ue200cite\ue202turn0":                               "Pending ",
		"plain text":                                                  "plain text",
		"a \ue200cite\ue202source1\ue201 b":                           "a  b",
	}
	for input, want := range cases {
		if got := sanitizeOutputText(input); got != want {
			t.Fatalf("sanitizeOutputText(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestConversationParserNeverRepeatsTextWhenAnnotationRewritesTail(t *testing.T) {
	parser := newConversationParser(nil)
	got := feed(parser,
		`{"p":"/message/content/parts/0","o":"append","v":"Fact "}`,
		`{"v":"\ue200cite\ue202turn0search1"}`,
		`{"v":"\ue201. Next"}`,
	)
	// "Fact " was already sent; the cleaned tail must not duplicate it.
	if strings.Count(got, "Fact") != 1 || !strings.HasSuffix(got, ". Next") {
		t.Fatalf("text = %q", got)
	}
}

func TestConversationParserStripsWritingBlockFences(t *testing.T) {
	parser := newConversationParser(nil)
	got := feed(parser,
		`{"p":"/message/content/parts/0","o":"append","v":"Here:\n\n:"}`,
		`{"v":"::writing{variant=\"document\" title=\"Ocean\" id=\"1\"}\nThe ocean"}`,
		`{"v":" is vast.\n:"}`,
		`{"v":"::"}`,
	)
	if got != "Here:\n\nThe ocean is vast.\n" {
		t.Fatalf("text = %q", got)
	}
	for input, want := range map[string]string{"a\n::: note\nb": "a\n::: note\nb", "Note:": "Note:", "x\n:::\ny": "x\ny"} {
		if got := stripWritingFences(input); got != want {
			t.Fatalf("stripWritingFences(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeThinkingEffort(t *testing.T) {
	cases := map[string]string{"": "", "auto": "", "none": "", "low": "low", "HIGH": "high", "xhigh": "extended", "extended": "extended", "max": "max", "weird": ""}
	for input, want := range cases {
		if got := normalizeThinkingEffort(input); got != want {
			t.Fatalf("normalizeThinkingEffort(%q) = %q, want %q", input, got, want)
		}
	}
}
