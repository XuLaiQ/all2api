package doubao

// Text chat for Doubao, ported from doubao2api (src/core/client.py):
//   - primary path: POST /chat/completion (content_block protocol, CHUNK_DELTA
//     deltas, BLOCK_THINKING toggles, SSE_ACK / STREAM_ERROR / gateway-error);
//   - fallback path: POST /alice/message/stream_call_bot (legacy base64 payload)
//     used only when the primary path fails before producing any output.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/infrastructure/sse"
	"github.com/XuLaiQ/all2api/internal/ports"
	"github.com/google/uuid"
)

const (
	defaultBotID = "7338286299411103781"

	// Content block types used by /chat/completion.
	blockText     = 10000
	blockSearch   = 10025
	blockThinking = 10040

	// Upstream error codes (doubao2api RISK_ERROR_CODES + expired login).
	codeLoginExpired = 710012001
	codeRateLimited  = 710022002
	codeVerify       = 710022004
)

var (
	// chatHeaderTimeout bounds the wait for upstream response headers.
	chatHeaderTimeout = 60 * time.Second
	// chatIdleTimeout cancels a stream that stops sending bytes; long deep-think
	// answers keep streaming and therefore never hit a whole-request timeout.
	chatIdleTimeout = 180 * time.Second
)

// chatModels mirrors doubao2api CHAT_MODELS: model name -> need_deep_think.
var chatModels = []struct {
	ID        string
	Name      string
	DeepThink int
}{
	{"doubao", "Doubao", 0},
	{"doubao-pro", "Doubao Pro", 0},
	{"doubao-think", "Doubao Think", 1},
	{"doubao-expert", "Doubao Expert", 3},
}

// deepThinkLevel maps a requested model to need_deep_think
// (0 quick, 1 think, 3 expert). Unknown upstream model keys use quick mode.
func deepThinkLevel(model string) int {
	normalized := strings.ToLower(strings.TrimSpace(model))
	for _, item := range chatModels {
		if item.ID == normalized {
			return item.DeepThink
		}
	}
	switch {
	case strings.Contains(normalized, "expert"):
		return 3
	case strings.Contains(normalized, "think"):
		return 1
	}
	return 0
}

// staticChatModels returns the reference catalogue entries.
func staticChatModels() []ports.ModelDescriptor {
	result := make([]ports.ModelDescriptor, 0, len(chatModels))
	for _, item := range chatModels {
		result = append(result, ports.ModelDescriptor{UpstreamID: item.ID, DisplayName: item.Name, Capabilities: []string{"chat"}})
	}
	return result
}

// flattenPrompt follows doubao2api _extract_prompt: one message is sent as-is,
// otherwise every message becomes "[role]: content" joined by newlines.
func flattenPrompt(messages []ports.ChatMessage) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		if message.Content == "" {
			continue
		}
		if len(messages) == 1 {
			parts = append(parts, message.Content)
		} else {
			parts = append(parts, fmt.Sprintf("[%s]: %s", message.Role, message.Content))
		}
	}
	return strings.Join(parts, "\n")
}

func (c *Client) ChatStream(ctx context.Context, input ports.ChatInput, accountID string, emit func(ports.StreamChunk) error) error {
	if c.ResolveCredential == nil {
		return fmt.Errorf("Doubao credential is unavailable")
	}
	credentials, err := c.ResolveCredential(ctx, accountID)
	if err != nil {
		return err
	}
	if credentialLookup(credentials, "Cookie", "cookie") == "" {
		return fmt.Errorf("Doubao credential is unavailable")
	}
	prompt := flattenPrompt(input.Messages)
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("prompt is required")
	}
	state := &chatEmitter{id: "chatcmpl_" + strings.ReplaceAll(uuid.NewString(), "-", ""), createdAt: time.Now().Unix(), emit: emit}
	primaryErr := c.completionStream(ctx, credentials, prompt, deepThinkLevel(input.Model), state)
	if primaryErr == nil && !state.produced {
		primaryErr = errEmptyResponse
	}
	if primaryErr == nil || state.produced || ctx.Err() != nil || !fallbackAllowed(primaryErr) {
		return primaryErr
	}
	if aliceErr := c.aliceStream(ctx, credentials, prompt, state); aliceErr != nil {
		return primaryErr
	}
	if !state.produced {
		return primaryErr
	}
	return nil
}

var errEmptyResponse = errors.New("Doubao returned an empty response")

// fallbackAllowed rejects the alice fallback for expired credentials, which
// would fail the same way and only add latency.
func fallbackAllowed(err error) bool {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.Status == http.StatusUnauthorized {
		return false
	}
	return true
}

type chatEmitter struct {
	id        string
	createdAt int64
	emit      func(ports.StreamChunk) error
	produced  bool
}

func (e *chatEmitter) text(text string) error {
	if text == "" {
		return nil
	}
	e.produced = true
	return e.emit(ports.StreamChunk{ID: e.id, Text: text, CreatedAt: e.createdAt})
}

func (e *chatEmitter) reasoning(text string) error {
	if text == "" {
		return nil
	}
	e.produced = true
	return e.emit(ports.StreamChunk{ID: e.id, Reasoning: text, CreatedAt: e.createdAt})
}

// completionPayload ports doubao2api _build_completion_payload (text only).
func completionPayload(prompt string, deepThink int, fp string) map[string]any {
	textBlock := map[string]any{
		"block_type": blockText,
		"content": map[string]any{
			"text_block":     map[string]any{"text": prompt, "icon_url": "", "icon_url_dark": "", "summary": ""},
			"pc_event_block": "",
		},
		"block_id":      uuid.NewString(),
		"parent_id":     "",
		"meta_info":     []any{},
		"append_fields": []any{},
		"is_finish":     true,
		"patch_type":    2,
	}
	return map[string]any{
		"client_meta": map[string]any{
			"local_conversation_id": "local_" + localDigits(16),
			"conversation_id":       "",
			"bot_id":                defaultBotID,
			"last_section_id":       "",
			"last_message_index":    0,
		},
		"messages": []any{map[string]any{
			"local_message_id": uuid.NewString(),
			"content_block":    []any{textBlock},
			"message_status":   0,
		}},
		"option": map[string]any{
			"send_message_scene":       "",
			"create_time_ms":           time.Now().UnixMilli(),
			"collect_id":               "",
			"is_audio":                 false,
			"answer_with_suggest":      false,
			"tts_switch":               false,
			"need_deep_think":          deepThink,
			"click_clear_context":      false,
			"from_suggest":             false,
			"is_regen":                 false,
			"is_replace":               false,
			"disable_sse_cache":        false,
			"select_text_action":       "",
			"resend_for_regen":         false,
			"scene_type":               0,
			"unique_key":               uuid.NewString(),
			"start_seq":                0,
			"need_create_conversation": true,
			"conversation_init_option": map[string]any{"need_ack_conversation": true},
			"regen_query_id":           []any{},
			"edit_query_id":            []any{},
			"regen_instruction":        "",
			"no_replace_for_regen":     false,
			"message_from":             0,
			"shared_app_name":          "",
			"action_bar_skill_id":      0,
			"sse_recv_event_options":   map[string]any{"support_chunk_delta": true},
			"is_ai_playground":         false,
		},
		"chat_ability": map[string]any{},
		"ext": map[string]any{
			"use_deep_think":                fmt.Sprintf("%d", deepThink),
			"fp":                            fp,
			"use_submit_pipeline":           "1",
			"commerce_credit_config_enable": "0",
			"sub_conv_firstmet_type":        "1",
		},
	}
}

func localDigits(n int) string {
	var builder strings.Builder
	for i := 0; i < n; i++ {
		builder.WriteByte(byte('0' + rand.Intn(10)))
	}
	return builder.String()
}

// openStream sends a JSON POST and returns the live response body wrapped in
// an idle-timeout reader. The caller must call the returned close function.
func (c *Client) openStream(ctx context.Context, path string, body []byte, credentials map[string]string) (*http.Response, io.Reader, func(), error) {
	streamCtx, cancel := context.WithCancel(ctx)
	request, err := c.newRequest(streamCtx, http.MethodPost, path, body, credentials)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	request.Header.Set("Accept", "text/event-stream")
	client := *c.HTTPClient
	client.Timeout = 0 // streaming: bounded by header + idle timers instead
	headerTimer := time.AfterFunc(chatHeaderTimeout, cancel)
	response, err := client.Do(request)
	headerTimer.Stop()
	if err != nil {
		cancel()
		if ctx.Err() == nil && streamCtx.Err() != nil {
			err = fmt.Errorf("upstream did not respond within %s", chatHeaderTimeout)
		}
		return nil, nil, nil, ports.NewTransportError("POST "+path, err)
	}
	idle := sse.NewIdleReader(response.Body, chatIdleTimeout, cancel)
	closeFn := func() {
		idle.Stop()
		_ = response.Body.Close()
		cancel()
	}
	return response, idle, closeFn, nil
}

// streamReadError converts an idle-timer cancellation into a readable error.
func streamReadError(ctx context.Context, path string, err error) error {
	if err == nil {
		return nil
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) || errors.Is(err, errStopStream) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return ports.NewTransportError("POST "+path, fmt.Errorf("stream idle for %s", chatIdleTimeout))
	}
	return ports.NewTransportError("POST "+path, err)
}

var errStopStream = errors.New("stop stream")

func (c *Client) completionStream(ctx context.Context, credentials map[string]string, prompt string, deepThink int, out *chatEmitter) error {
	const path = "/chat/completion"
	fp := credentialLookup(credentials, "fp")
	if fp == "" {
		fp = doubaoQuery(credentials)["fp"]
	}
	body, _ := json.Marshal(completionPayload(prompt, deepThink, fp))
	response, reader, closeFn, err := c.openStream(ctx, path, body, credentials)
	if err != nil {
		return err
	}
	defer closeFn()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &HTTPError{Status: response.StatusCode, Operation: "chat"}
	}
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		raw, readErr := io.ReadAll(io.LimitReader(reader, 1<<20))
		if readErr != nil {
			return streamReadError(ctx, path, readErr)
		}
		return nonStreamError(raw)
	}
	parser := &completionParser{out: out}
	err = sse.Read(reader, parser.handle)
	if err = streamReadError(ctx, path, err); err != nil {
		return err
	}
	if !out.produced && parser.lastErr != nil {
		return parser.lastErr
	}
	return nil
}

// nonStreamError handles JSON bodies returned instead of SSE, e.g.
// {"code":710012001,"msg":"登录已过期"} or a gateway-error frame.
func nonStreamError(raw []byte) error {
	var object map[string]any
	if json.Unmarshal(bytes.TrimSpace(raw), &object) == nil {
		if code := intAny(object["code"]); code != 0 {
			return codeError(code)
		}
	}
	if bytes.Contains(raw, []byte("gateway-error")) {
		return &HTTPError{Status: http.StatusUnauthorized, Operation: "chat", Code: "gateway-error"}
	}
	return &HTTPError{Status: http.StatusBadGateway, Operation: "chat", Code: "unexpected_response"}
}

func codeError(code int) error {
	return &HTTPError{Status: samanthaStatus(fmt.Sprintf("%d", code)), Operation: "chat", Code: fmt.Sprintf("%d", code)}
}

type completionParser struct {
	out           *chatEmitter
	thinkingCount int
	inThinking    bool
	lastErr       error
}

func (p *completionParser) emit(text string) error {
	if p.inThinking {
		return p.out.reasoning(text)
	}
	return p.out.text(text)
}

func (p *completionParser) handle(event sse.Event) error {
	var object map[string]any
	if json.Unmarshal([]byte(event.Data), &object) != nil {
		return nil
	}
	switch event.Name {
	case "gateway-error":
		return &HTTPError{Status: http.StatusUnauthorized, Operation: "chat", Code: firstNonEmpty(stringAny(object["code"]), "gateway-error")}
	case "SSE_ACK":
		return nil
	}
	if _, hasCode := object["error_code"]; hasCode || event.Name == "STREAM_ERROR" {
		code := intAny(object["error_code"])
		switch code {
		case codeLoginExpired, codeRateLimited, codeVerify:
			return codeError(code)
		}
		if code != 0 {
			p.lastErr = &HTTPError{Status: http.StatusBadGateway, Operation: "chat", Code: fmt.Sprintf("%d", code)}
			return nil
		}
	}
	// CHUNK_DELTA: compact {"text": "..."}.
	if text, ok := object["text"].(string); ok {
		return p.emit(text)
	}
	blocks := completionBlocks(object)
	for _, block := range blocks {
		content, _ := block["content"].(map[string]any)
		switch intAny(block["block_type"]) {
		case blockThinking:
			p.thinkingCount++
			p.inThinking = p.thinkingCount == 1
		case blockText:
			textBlock, _ := content["text_block"].(map[string]any)
			if text, _ := textBlock["text"].(string); text != "" {
				if err := p.emit(text); err != nil {
					return err
				}
			}
		}
	}
	if len(blocks) > 0 {
		return nil
	}
	// STREAM_CHUNK variant: patch_value.content is a JSON string {"text": ...}.
	for _, patch := range listAny(object["patch_op"]) {
		value, _ := patch["patch_value"].(map[string]any)
		raw, _ := value["content"].(string)
		if raw == "" {
			continue
		}
		var content map[string]any
		if json.Unmarshal([]byte(raw), &content) != nil {
			continue
		}
		if text, _ := content["text"].(string); text != "" {
			if err := p.emit(text); err != nil {
				return err
			}
		}
	}
	return nil
}

// completionBlocks collects content blocks from patch_op[].patch_value and
// from content.content_block, matching doubao2api _iter_blocks.
func completionBlocks(object map[string]any) []map[string]any {
	result := make([]map[string]any, 0)
	for _, patch := range listAny(object["patch_op"]) {
		value, _ := patch["patch_value"].(map[string]any)
		result = append(result, listAny(value["content_block"])...)
	}
	if content, ok := object["content"].(map[string]any); ok {
		result = append(result, listAny(content["content_block"])...)
	}
	return result
}

func listAny(value any) []map[string]any {
	items, _ := value.([]any)
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			result = append(result, object)
		}
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// aliceStream is the legacy /alice/message/stream_call_bot path.
func (c *Client) aliceStream(ctx context.Context, credentials map[string]string, prompt string, out *chatEmitter) error {
	const path = "/alice/message/stream_call_bot"
	inner := map[string]any{"event_type": 1, "message": map[string]any{
		"conversation_id":  "0",
		"section_id":       "0",
		"local_message_id": uuid.NewString(),
		"content_type":     1,
		"content":          mustJSON(map[string]string{"text": prompt}),
		"reply_id":         "",
		"ext": map[string]string{
			"origin": "https://www.doubao.com", "stream": "1", "answer_with_suggest": "1", "browser_language": "zh-CN",
		},
		"local_conversation_id": "0",
		"bot_id":                firstNonEmpty(credentialLookup(credentials, "bot_id"), defaultBotID),
		"meta_infos":            []any{},
	}}
	encoded, _ := json.Marshal(inner)
	body, _ := json.Marshal(map[string]string{"payload": base64.StdEncoding.EncodeToString(encoded)})
	response, reader, closeFn, err := c.openStream(ctx, path, body, credentials)
	if err != nil {
		return err
	}
	defer closeFn()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &HTTPError{Status: response.StatusCode, Operation: "chat"}
	}
	err = sse.Read(reader, func(event sse.Event) error {
		if event.Name == "done" || strings.TrimSpace(event.Data) == "[DONE]" {
			return errStopStream
		}
		var object map[string]any
		if json.Unmarshal([]byte(event.Data), &object) != nil {
			return nil
		}
		text, err := aliceText(object)
		if err != nil {
			return err
		}
		return out.text(text)
	})
	if errors.Is(err, errStopStream) {
		return nil
	}
	return streamReadError(ctx, path, err)
}

// aliceText extracts bot text from one alice frame. content is a JSON string;
// content_type 1 is answer text, 2 suggestions, reply_id "0" is the user echo.
func aliceText(object map[string]any) (string, error) {
	if code := intAny(object["code"]); code != 0 {
		return "", codeError(code)
	}
	message, _ := object["message"].(map[string]any)
	if message == nil {
		return "", nil
	}
	if replyID, _ := message["reply_id"].(string); replyID == "0" {
		return "", nil
	}
	if intAny(message["content_type"]) != 1 {
		return "", nil
	}
	raw, _ := message["content"].(string)
	var content map[string]any
	if json.Unmarshal([]byte(raw), &content) != nil {
		return "", nil
	}
	text, _ := content["text"].(string)
	return text, nil
}
