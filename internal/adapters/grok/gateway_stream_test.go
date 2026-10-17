package grok

import (
	"errors"
	"net/http"
	"testing"
)

func TestGatewayParserChannelsRenderAndCards(t *testing.T) {
	parser := &gatewayParser{}
	chunk := func(channel, text string) map[string]any {
		return map[string]any{"type": "response.chunk", "chunk": map[string]any{"text": map[string]any{"channel": channel, "text": text}}}
	}
	var text, reasoning string
	apply := func(event map[string]any) {
		delta, err := parser.parse(event)
		if err != nil {
			t.Fatalf("parse(%v) error = %v", event, err)
		}
		text += delta.Text
		reasoning += delta.Reasoning
	}
	apply(chunk("CHANNEL_ANALYSIS", "plan"))
	apply(chunk("CHANNEL_REASONING_SUMMARY", "!"))
	apply(chunk("CHANNEL_FOLLOW_UP", "dropped"))
	apply(chunk("", "a"))
	apply(chunk("CHANNEL_ASSISTANT_RESPONSE", "a"))
	// Card attachment registers a searched image and a finished generation.
	apply(map[string]any{"type": "response.grok.output", "output": map[string]any{"card_attachment": []any{
		map[string]any{"jsonData": `{"id":"card-1","image":{"title":"cat","thumbnail":"https://img.test/t.png","link":"https://src.test/cat"}}`},
		map[string]any{"jsonData": map[string]any{"id": "card-2", "image_chunk": map[string]any{"progress": float64(100), "imageUrl": "users/u/generated/1.png"}}},
	}}})
	apply(chunk("", ` see <grok:render card_id="card-1" card_type="image" type="render_searched_image"><x/></grok:render> done`))
	apply(map[string]any{"type": "response.chunk", "chunk": map[string]any{"render_citation": map[string]any{"url": "https://news.test/a"}}})
	apply(map[string]any{"type": "response.chunk", "chunk": map[string]any{"render_citation": map[string]any{"url": "https://news.test/a"}}})
	apply(map[string]any{"type": "response.output_text.done", "text": "ignored full text"})

	want := "aa\n\n![image](https://assets.grok.com/users/u/generated/1.png)\n\n see [![cat](https://img.test/t.png)](https://src.test/cat) done[[1]](https://news.test/a)"
	if text != want {
		t.Fatalf("text = %q\nwant  %q", text, want)
	}
	if reasoning != "plan!" {
		t.Fatalf("reasoning = %q", reasoning)
	}
}

func TestGatewayParserOutputTextDoneFallbackAndStreamError(t *testing.T) {
	parser := &gatewayParser{}
	delta, err := parser.parse(map[string]any{"type": "response.output_text.done", "text": "full"})
	if err != nil || delta.Text != "full" {
		t.Fatalf("done fallback = %#v/%v", delta, err)
	}
	_, err = parser.parse(map[string]any{"type": "response.grok.output", "output": map[string]any{"stream_error": map[string]any{"code": float64(7), "message": "blocked"}}})
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.StatusCode() != http.StatusForbidden {
		t.Fatalf("stream error = %T/%v", err, err)
	}
	_, err = parser.parse(map[string]any{"type": "response.grok.output", "output": map[string]any{"stream_error": map[string]any{"message": "You have reached your usage limit"}}})
	if !errors.As(err, &gatewayErr) || gatewayErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("usage error = %T/%v", err, err)
	}
}

func TestWebUserAgentPrefersImportedBrowserUA(t *testing.T) {
	client := NewClient("https://grok.com", nil)
	if got := client.webUserAgent(map[string]string{"user_agent": "Imported/1.0"}); got != "Imported/1.0" {
		t.Fatalf("webUserAgent = %q", got)
	}
	if got := client.webUserAgent(nil); got != defaultUserAgent {
		t.Fatalf("default webUserAgent = %q", got)
	}
}
