package chatgpt

// Streaming parser for /backend-api/conversation, ported from chatgpt2api
// (MIT License, Copyright (c) 2026 kunkun): services/protocol/conversation.py
// (assistant_raw_text, apply_text_patch, strip_history, sanitize_output_text,
// iter_conversation_payloads) and openai_backend_api.py
// (_normalize_thinking_effort, stream_conversation timeout 300 s).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/XuLaiQ/all2api/internal/infrastructure/sse"
	"github.com/XuLaiQ/all2api/internal/ports"
)

const (
	conversationPath = "/backend-api/conversation"
	textPartPath     = "/message/content/parts/0"

	// Private-use markers ChatGPT Web wraps rich annotations in.
	annotationStart = '\ue200'
	annotationEnd   = '\ue201'
	annotationSep   = "\ue202"
)

var (
	// conversationStreamTimeout matches the reference stream timeout.
	conversationStreamTimeout = 300 * time.Second
	// conversationIdleTimeout aborts a stream that stops sending bytes.
	conversationIdleTimeout = 120 * time.Second
)

// normalizeThinkingEffort mirrors _normalize_thinking_effort.
func normalizeThinkingEffort(value string) string {
	switch normalized := strings.ToLower(strings.TrimSpace(value)); normalized {
	case "low", "medium", "high", "standard", "max":
		return normalized
	case "xhigh", "extended":
		return "extended"
	}
	return ""
}

func (c *Client) streamHTTPClient() HTTPDoer {
	if c.StreamHTTPClient != nil {
		return c.StreamHTTPClient
	}
	if c.streamProxy != "" {
		if client, err := newChatGPTHTTPClient(c.BaseURL, c.streamProxy, conversationStreamTimeout); err == nil {
			return client
		}
	}
	return c.HTTPClient
}

// streamConversation POSTs the conversation and forwards deltas as SSE
// frames arrive instead of buffering the whole answer.
func (c *Client) streamConversation(ctx context.Context, body []byte, token string, extra map[string]string, parser *conversationParser, emit func(ports.StreamChunk) error) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	request, err := c.buildRequest(streamCtx, http.MethodPost, conversationPath, body, token, extra)
	if err != nil {
		return err
	}
	response, err := c.streamHTTPClient().Do(request)
	if err != nil {
		return ports.NewTransportError("POST "+conversationPath, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &HTTPError{Status: response.StatusCode, Operation: "conversation"}
	}
	idle := sse.NewIdleReader(response.Body, conversationIdleTimeout, cancel)
	defer idle.Stop()
	created := time.Now().Unix()
	err = sse.Read(idle, func(event sse.Event) error {
		delta, done := parser.handle(event.Data)
		if delta != "" {
			if err := emit(ports.StreamChunk{ID: parser.messageID, Text: delta, CreatedAt: created}); err != nil {
				return err
			}
		}
		if done {
			return errConversationDone
		}
		return nil
	})
	if err == nil || errors.Is(err, errConversationDone) {
		if parser.blocked && parser.text == "" {
			return &HTTPError{Status: http.StatusBadRequest, Operation: "conversation moderation"}
		}
		return nil
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) || ctx.Err() != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if streamCtx.Err() != nil {
		return ports.NewTransportError("POST "+conversationPath, fmt.Errorf("stream idle for %s", conversationIdleTimeout))
	}
	return ports.NewTransportError("POST "+conversationPath, err)
}

var errConversationDone = errors.New("conversation done")

// conversationParser keeps the raw and sanitized assistant text and turns
// every upstream payload into a monotonic text delta.
type conversationParser struct {
	historyText     string
	historyMessages []string
	historyIndex    int
	rawText         string
	text            string
	lastPath        string
	messageID       string
	blocked         bool
}

func newConversationParser(historyMessages []string) *conversationParser {
	return &conversationParser{historyText: strings.Join(historyMessages, ""), historyMessages: historyMessages}
}

// handle consumes one SSE data payload and returns the new visible text.
func (p *conversationParser) handle(payload string) (string, bool) {
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "v1" {
		return "", false
	}
	if payload == "[DONE]" {
		return "", true
	}
	var event map[string]any
	if json.Unmarshal([]byte(payload), &event) != nil {
		return "", false
	}
	if event["type"] == "moderation" {
		if moderation, _ := event["moderation_response"].(map[string]any); moderation != nil && moderation["blocked"] == true {
			p.blocked = true
		}
	}
	// Replayed history: the upstream may echo assistant turns we sent.
	if p.historyIndex < len(p.historyMessages) {
		if text, ok := p.eventAssistantText(event); ok && text == p.historyMessages[p.historyIndex] {
			p.historyIndex++
			p.rawText, p.text = "", ""
			return "", false
		}
	}
	p.rawText = p.assistantRawText(event, p.rawText)
	next := sanitizeOutputText(p.rawText)
	if next == p.text {
		return "", false
	}
	// Annotation cleanup can rewrite the tail; only emit past the common
	// prefix so already-sent text is never duplicated.
	common := commonPrefixLength(p.text, next)
	p.text = next
	return next[common:], false
}

func commonPrefixLength(left, right string) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	index := 0
	for index < limit && left[index] == right[index] {
		index++
	}
	for index > 0 && index < len(right) && !utf8.RuneStart(right[index]) {
		index--
	}
	return index
}

func visibleAssistantMessage(candidate any) map[string]any {
	object, _ := candidate.(map[string]any)
	message, _ := object["message"].(map[string]any)
	if message == nil {
		return nil
	}
	author, _ := message["author"].(map[string]any)
	if role, _ := author["role"].(string); strings.ToLower(strings.TrimSpace(role)) != "assistant" {
		return nil
	}
	metadata, _ := message["metadata"].(map[string]any)
	if hidden, _ := metadata["is_visually_hidden_from_conversation"].(bool); hidden {
		return nil
	}
	if recipient, _ := message["recipient"].(string); recipient != "" && strings.ToLower(strings.TrimSpace(recipient)) != "all" {
		return nil
	}
	if channel, _ := message["channel"].(string); channel != "" && strings.ToLower(strings.TrimSpace(channel)) != "final" {
		return nil
	}
	return message
}

func messageText(message map[string]any) string {
	content, _ := message["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	var builder strings.Builder
	for _, part := range parts {
		if text, ok := part.(string); ok {
			builder.WriteString(text)
		}
	}
	if builder.Len() > 0 {
		return builder.String()
	}
	// content_type "code" keeps its text in "text".
	text, _ := content["text"].(string)
	return text
}

func (p *conversationParser) eventAssistantText(event map[string]any) (string, bool) {
	for _, candidate := range []any{event, event["v"]} {
		if message := visibleAssistantMessage(candidate); message != nil {
			if id, _ := message["id"].(string); id != "" {
				p.messageID = id
			}
			return stripHistory(messageText(message), p.historyText), true
		}
	}
	return "", false
}

func (p *conversationParser) assistantRawText(event map[string]any, current string) string {
	for _, candidate := range []any{event, event["v"]} {
		message := visibleAssistantMessage(candidate)
		if message == nil {
			continue
		}
		if id, _ := message["id"].(string); id != "" {
			p.messageID = id
		}
		if text := messageText(message); text != "" {
			return stripHistory(text, p.historyText)
		}
	}
	return p.applyTextPatch(event, current)
}

// applyTextPatch applies the delta-encoding v1 operations that touch the
// first text part. A bare {"v": "..."} continues the previous operation, so
// it is applied only while the last addressed path is the text part.
func (p *conversationParser) applyTextPatch(event map[string]any, current string) string {
	path, hasPath := event["p"].(string)
	if hasPath {
		p.lastPath = path
	}
	if path == textPartPath {
		return p.applyPatchOperation(event, current)
	}
	operations := event["v"]
	if text, ok := operations.(string); ok && current != "" && !hasPath && event["o"] == nil {
		if p.lastPath == "" || p.lastPath == textPartPath {
			return current + text
		}
		return current
	}
	items, ok := operations.([]any)
	if !ok {
		return current
	}
	// Both {"o":"patch","v":[...]} and a bare operation list.
	text := current
	for _, item := range items {
		if operation, ok := item.(map[string]any); ok {
			text = p.applyTextPatch(operation, text)
		}
	}
	return text
}

func (p *conversationParser) applyPatchOperation(operation map[string]any, current string) string {
	value, _ := operation["v"].(string)
	switch operation["o"] {
	case "append":
		return current + value
	case "replace":
		return stripHistory(value, p.historyText)
	}
	return current
}

// stripHistory removes replayed assistant history from the start of text.
func stripHistory(text, history string) string {
	for history != "" && strings.HasPrefix(text, history) {
		text = text[len(history):]
	}
	return text
}

// sanitizeOutputText rewrites \ue200 kind \ue202 data... \ue201 annotations:
// links keep "label (url)", entity labels stay readable, internal citation
// pointers (turnN..., source...) are removed, and an unfinished trailing
// annotation is hidden until it completes.
func sanitizeOutputText(text string) string {
	return stripWritingFences(stripAnnotations(text))
}

// writingFence matches ChatGPT Web "writing block" directives such as
// `:::writing{variant="document" title="..." id="1"}` and the closing `:::`.
// The UI renders them as a canvas card; API clients would see raw markup.
var writingFence = regexp.MustCompile(`^\s*:::(?:[A-Za-z][\w-]*(?:\{[^}\n]*\})?)?\s*$`)

// stripWritingFences removes directive fence lines and hides a trailing,
// still-incomplete fence so streamed text never has to be retracted.
func stripWritingFences(text string) string {
	if !strings.Contains(text, ":::") && !strings.HasSuffix(text, ":") {
		return text
	}
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for index, line := range lines {
		last := index == len(lines)-1
		trimmed := strings.TrimSpace(line)
		if last && trimmed != "" && (strings.HasPrefix(":::", trimmed) || (strings.HasPrefix(trimmed, ":::") && !strings.Contains(trimmed, "}"))) {
			// Partial fence (":", "::", ":::wri..."): wait for the rest.
			kept = append(kept, "")
			continue
		}
		if trimmed != "" && writingFence.MatchString(line) {
			if !last {
				continue // drop the line together with its newline
			}
			kept = append(kept, "")
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func stripAnnotations(text string) string {
	if !strings.ContainsRune(text, annotationStart) {
		return text
	}
	var builder strings.Builder
	rest := text
	for {
		start := strings.IndexRune(rest, annotationStart)
		if start < 0 {
			builder.WriteString(rest)
			break
		}
		afterStart := rest[start+utf8.RuneLen(annotationStart):]
		end := strings.IndexRune(afterStart, annotationEnd)
		if end < 0 {
			builder.WriteString(rest[:start]) // drop the unfinished annotation
			break
		}
		prefix := rest[:start]
		replacement := annotationText(afterStart[:end])
		rest = afterStart[end+utf8.RuneLen(annotationEnd):]
		if replacement == "" && startsWithPunctuation(rest) {
			prefix = strings.TrimRightFunc(prefix, unicode.IsSpace)
		}
		builder.WriteString(prefix)
		builder.WriteString(replacement)
	}
	return builder.String()
}

func startsWithPunctuation(text string) bool {
	return text != "" && strings.ContainsRune(".,;:!?", rune(text[0]))
}

func annotationText(payload string) string {
	parts := strings.Split(payload, annotationSep)
	for index := range parts {
		parts[index] = strings.TrimSpace(parts[index])
	}
	kind := strings.ToLower(parts[0])
	data := parts[1:]
	if kind == "url" {
		label, link := "", ""
		if len(data) > 0 {
			label = data[0]
		}
		if len(data) > 1 {
			link = data[1]
		}
		if label != "" && (strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://")) {
			return label + " (" + link + ")"
		}
		if label != "" {
			return label
		}
		return link
	}
	for _, part := range data {
		if part != "" && !isInternalAnnotationPart(part) {
			return part
		}
	}
	return ""
}

func isInternalAnnotationPart(part string) bool {
	lower := strings.ToLower(strings.TrimSpace(part))
	return lower == "" || strings.HasPrefix(lower, "turn") || strings.HasPrefix(lower, "source")
}
