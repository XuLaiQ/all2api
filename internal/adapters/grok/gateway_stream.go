package grok

// Grok Web gateway (/ws/mgw/) event parsing.
//
// Ported from grok2api (MIT License, Copyright (c) 2026 Chenyme):
// backend/internal/infra/provider/web/gateway.go (runGatewayStream,
// parseGatewayEvent, appendGatewayDelta) and chat.go (cleanChatToken,
// collectCardAttachment, webResponseError). Search tool cards and xAI
// annotations are not ported; citations are rendered inline as [[n]](url).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
	"github.com/bogdanfinn/websocket"
)

const (
	gatewayMaxFrameBytes = 16 << 20
	gatewayWriteTimeout  = 15 * time.Second
)

var grokRenderPattern = regexp.MustCompile(`(?s)<grok:render\s+card_id="([^"]+)"\s+card_type="([^"]+)"\s+type="([^"]+)"[^>]*>.*?</grok:render>`)

// webUserAgent prefers the browser UA captured with the account, because
// Cloudflare cookies (cf_clearance) are bound to the UA that obtained them.
func (c *Client) webUserAgent(credentials map[string]string) string {
	if value := firstCredentialValue(credentials, "user_agent", "User-Agent", "user-agent", "userAgent"); value != "" {
		return value
	}
	if strings.TrimSpace(c.UserAgent) != "" {
		return c.UserAgent
	}
	return defaultUserAgent
}

// gatewayParser turns gateway events into text/reasoning deltas.
type gatewayParser struct {
	upstreamText  strings.Builder
	cardCache     map[string]map[string]any
	citationIndex map[string]int
	lastCitation  int
	images        map[string]bool
}

type gatewayDelta struct {
	Text      string
	Reasoning string
}

func (p *gatewayParser) parse(event map[string]any) (gatewayDelta, error) {
	typeName, _ := event["type"].(string)
	switch typeName {
	case "response.chunk":
		chunk, _ := event["chunk"].(map[string]any)
		if chunk == nil {
			return gatewayDelta{}, nil
		}
		if cite, _ := chunk["render_citation"].(map[string]any); cite != nil {
			return gatewayDelta{Text: p.citation(firstString(cite, "url"))}, nil
		}
		text, _ := chunk["text"].(map[string]any)
		delta, _ := text["text"].(string)
		channel, _ := text["channel"].(string)
		return p.appendDelta(channel, delta), nil
	case "response.output_text.delta":
		delta, _ := event["delta"].(string)
		return p.appendDelta("CHANNEL_ASSISTANT_RESPONSE", delta), nil
	case "response.output_text.done":
		// Only a fallback: the full text repeats what was already streamed.
		text, _ := event["text"].(string)
		if p.upstreamText.Len() == 0 && text != "" {
			return p.appendDelta("CHANNEL_ASSISTANT_RESPONSE", text), nil
		}
	case "response.grok.output":
		output, _ := event["output"].(map[string]any)
		if streamError, _ := output["stream_error"].(map[string]any); streamError != nil {
			return gatewayDelta{}, webStreamError(streamError)
		}
		if rawURL := p.collectCardAttachment(output["card_attachment"]); rawURL != "" {
			rawURL = absoluteAssetURL(rawURL)
			if p.images == nil {
				p.images = map[string]bool{}
			}
			if !p.images[rawURL] {
				p.images[rawURL] = true
				return gatewayDelta{Text: "\n\n![image](" + rawURL + ")\n\n"}, nil
			}
		}
	}
	return gatewayDelta{}, nil
}

// appendDelta routes a token by channel: analysis/reasoning channels become
// reasoning, the assistant channel (or none) becomes visible text, and any
// other channel (tool chatter, follow-ups) is dropped. Repeated tokens are
// legitimate and must not be deduplicated.
func (p *gatewayParser) appendDelta(channel, delta string) gatewayDelta {
	if delta == "" {
		return gatewayDelta{}
	}
	normalized := strings.ToUpper(strings.TrimSpace(channel))
	if strings.Contains(normalized, "ANALYSIS") || strings.Contains(normalized, "REASONING") {
		return gatewayDelta{Reasoning: delta}
	}
	if normalized != "" && normalized != "CHANNEL_ASSISTANT_RESPONSE" {
		return gatewayDelta{}
	}
	p.upstreamText.WriteString(delta)
	return gatewayDelta{Text: p.cleanToken(delta)}
}

// cleanToken replaces <grok:render> markup with markdown (searched images,
// inline citations) or removes it (generated images are emitted from cards).
func (p *gatewayParser) cleanToken(token string) string {
	if !strings.Contains(token, "<grok:render") {
		p.lastCitation = 0
		return token
	}
	matches := grokRenderPattern.FindAllStringSubmatchIndex(token, -1)
	if len(matches) == 0 {
		return token
	}
	var builder strings.Builder
	cursor := 0
	for _, match := range matches {
		if prefix := token[cursor:match[0]]; prefix != "" {
			builder.WriteString(prefix)
			p.lastCitation = 0
		}
		builder.WriteString(p.renderCard(token[match[2]:match[3]], token[match[6]:match[7]]))
		cursor = match[1]
	}
	if suffix := token[cursor:]; suffix != "" {
		builder.WriteString(suffix)
		p.lastCitation = 0
	}
	return builder.String()
}

func (p *gatewayParser) renderCard(cardID, renderType string) string {
	card := p.cardCache[cardID]
	if card == nil {
		return ""
	}
	switch renderType {
	case "render_searched_image":
		image, _ := card["image"].(map[string]any)
		thumbnail := firstString(image, "thumbnail", "original")
		if thumbnail == "" {
			return ""
		}
		title := firstString(image, "title")
		if title == "" {
			title = "image"
		}
		if link := firstString(image, "link"); link != "" {
			return fmt.Sprintf("[![%s](%s)](%s)", title, thumbnail, link)
		}
		return fmt.Sprintf("![%s](%s)", title, thumbnail)
	case "render_inline_citation":
		return p.citation(firstString(card, "url"))
	}
	return ""
}

// citation numbers each distinct source once and collapses adjacent repeats.
func (p *gatewayParser) citation(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ""
	}
	normalized := parsed.String()
	if p.citationIndex == nil {
		p.citationIndex = map[string]int{}
	}
	index, ok := p.citationIndex[normalized]
	if !ok {
		index = len(p.citationIndex) + 1
		p.citationIndex[normalized] = index
	}
	if p.lastCitation == index {
		return ""
	}
	p.lastCitation = index
	return fmt.Sprintf("[[%d]](%s)", index, normalized)
}

func (p *gatewayParser) collectCardAttachment(value any) string {
	if values, ok := value.([]any); ok {
		first := ""
		for _, item := range values {
			if rawURL := p.collectCardAttachment(item); first == "" && rawURL != "" {
				first = rawURL
			}
		}
		return first
	}
	data := cardAttachmentData(value)
	if data == nil {
		return ""
	}
	if id := firstString(data, "id"); id != "" {
		if p.cardCache == nil {
			p.cardCache = map[string]map[string]any{}
		}
		p.cardCache[id] = data
	}
	return imageURLFromCardData(data)
}

func cardAttachmentData(value any) map[string]any {
	card, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if raw, ok := card["jsonData"].(map[string]any); ok {
		return raw
	}
	if raw, _ := card["jsonData"].(string); raw != "" {
		var data map[string]any
		if json.Unmarshal([]byte(raw), &data) == nil {
			return data
		}
	}
	if card["image_chunk"] != nil || card["imageChunk"] != nil {
		return card
	}
	return nil
}

// imageURLFromCardData returns the final image URL once generation reaches
// 100% and the image was not moderated.
func imageURLFromCardData(data map[string]any) string {
	chunk, _ := data["image_chunk"].(map[string]any)
	if chunk == nil {
		chunk, _ = data["imageChunk"].(map[string]any)
	}
	if chunk == nil {
		return ""
	}
	if moderated, _ := chunk["moderated"].(bool); moderated || intNumber(chunk["progress"]) < 100 {
		return ""
	}
	return firstString(chunk, "imageUrl", "image_url")
}

func absoluteAssetURL(value string) string {
	if strings.HasPrefix(value, "https://") {
		return value
	}
	return "https://assets.grok.com/" + strings.TrimPrefix(value, "/")
}

// webStreamError classifies output.stream_error like webResponseError.
func webStreamError(value map[string]any) error {
	message := firstString(value, "message")
	if message == "" {
		message = "Grok Web stream error"
	}
	lower := strings.ToLower(message)
	switch {
	case intNumber(value["code"]) == 7 || strings.Contains(lower, "anti-bot"):
		return &GatewayError{Status: http.StatusForbidden, Code: "anti_bot_rejected", Message: message}
	case strings.Contains(lower, "usage limit") || strings.Contains(lower, "usage quota"):
		return &GatewayError{Status: http.StatusTooManyRequests, Code: "usage_limit", Message: message}
	}
	return &GatewayError{Status: http.StatusBadGateway, Code: "stream_error", Message: message}
}

func readGatewayStream(ctx context.Context, sender *gatewaySender, prompt, responseID, initialEventID string, idle time.Duration, emit func(ports.StreamChunk) error) error {
	created := false
	attached := false
	turnSent := false
	currentSessionID := ""
	createdAt := time.Now().Unix()
	parser := &gatewayParser{}
	for {
		if idle > 0 {
			_ = sender.connection.SetReadDeadline(time.Now().Add(idle))
		}
		messageType, data, err := sender.connection.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ports.NewTransportError("Grok websocket stream", ctx.Err())
			}
			if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
				return ports.NewTransportError("Grok websocket stream", fmt.Errorf("no gateway frame for %s", idle))
			}
			return ports.NewTransportError("Grok websocket stream", err)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		var root map[string]any
		if err := json.Unmarshal(data, &root); err != nil {
			continue
		}
		event, _ := root["event"].(map[string]any)
		if event == nil {
			continue
		}
		typeName, _ := event["type"].(string)
		switch typeName {
		case "session.created":
			// Ignore acknowledgements of other client events.
			if clientEventID := firstString(event, "client_event_id"); clientEventID != "" && clientEventID != initialEventID {
				continue
			}
			created = true
			if currentSessionID == "" {
				currentSessionID = firstString(root, "session_id")
			}
		case "conversation.attached":
			attached = true
			conversation, _ := event["conversation"].(map[string]any)
			conversationID := firstString(conversation, "id")
			if conversationID == "" {
				return fmt.Errorf("Grok Gateway returned an empty conversation id")
			}
			if currentSessionID == "" {
				currentSessionID = conversationID
			} else if currentSessionID != conversationID {
				return fmt.Errorf("Grok Gateway returned inconsistent conversation ids")
			}
		case "response.done":
			if response, ok := event["response"].(map[string]any); ok {
				if value, _ := response["id"].(string); value != "" {
					responseID = value
				}
				if status, _ := response["status"].(string); status != "" && status != "completed" {
					return fmt.Errorf("Grok response status is %s", status)
				}
			}
			return emit(ports.StreamChunk{ID: responseID, CreatedAt: createdAt, FinishReason: "stop"})
		case "error":
			return gatewayError(event)
		case "session.ended":
			return fmt.Errorf("Grok session ended before response completed")
		default:
			delta, err := parser.parse(event)
			if err != nil {
				return err
			}
			if delta.Text != "" || delta.Reasoning != "" {
				if err := emit(ports.StreamChunk{ID: responseID, Text: delta.Text, Reasoning: delta.Reasoning, CreatedAt: createdAt}); err != nil {
					return err
				}
			}
		}
		if created && attached && !turnSent {
			turnSent = true
			item, response := gatewayTurnEvents(currentSessionID, prompt)
			if err := sender.write(item); err != nil {
				return ports.NewTransportError("Grok conversation.item.create", err)
			}
			if err := sender.write(response); err != nil {
				return ports.NewTransportError("Grok response.create", err)
			}
		}
	}
}
