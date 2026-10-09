package chatgpt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func (c *Client) CapabilityWithAccount(ctx context.Context, input ports.CapabilityInput, accountID string) (ports.CapabilityResult, error) {
	if input.Capability != "search" && input.Capability != "image" && input.Capability != "editable" {
		return ports.CapabilityResult{}, fmt.Errorf("ChatGPT capability is not supported")
	}
	if c.ResolveCredential == nil {
		return ports.CapabilityResult{}, fmt.Errorf("ChatGPT credential is unavailable")
	}
	credentials, err := c.ResolveCredential(ctx, accountID)
	if err != nil {
		return ports.CapabilityResult{}, err
	}
	token := strings.TrimSpace(credentials["access_token"])
	if token == "" {
		token = strings.TrimSpace(credentials["token"])
	}
	if token == "" {
		return ports.CapabilityResult{}, fmt.Errorf("ChatGPT credential is unavailable")
	}
	client := c.withCredentials(credentials)
	var result map[string]any
	if input.Capability == "image" {
		result, err = client.imageCapability(ctx, input, token, credentials)
	} else if input.Capability == "editable" {
		result, err = client.editableCapability(ctx, input, token, credentials)
	} else {
		result, err = client.search(ctx, input.Prompt, input.Model, token, credentials)
	}
	if err != nil {
		return ports.CapabilityResult{}, err
	}
	return ports.CapabilityResult{StatusCode: http.StatusOK, Body: result}, nil
}

func (c *Client) search(ctx context.Context, prompt, model, token string, credentials map[string]string) (map[string]any, error) {
	if strings.TrimSpace(prompt) == "" || strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("search prompt and model are required")
	}
	prepare := map[string]any{
		"action": "next", "fork_from_shared_post": false, "parent_message_id": "client-created-root",
		"model": model, "client_prepare_state": "success", "timezone_offset_min": -480,
		"timezone": "Asia/Shanghai", "conversation_mode": map[string]string{"kind": "primary_assistant"},
		"system_hints": []string{"search"}, "partial_query": map[string]any{
			"id": fmt.Sprintf("query_%d", time.Now().UnixNano()), "author": map[string]string{"role": "user"},
			"content": map[string]any{"content_type": "text", "parts": []string{prompt}},
		}, "supports_buffering": true, "supported_encodings": []string{"v1"},
		"client_contextual_info": map[string]string{"app_name": "chatgpt.com"},
	}
	encoded, _ := json.Marshal(prepare)
	body, status, err := c.requestWithHeaders(ctx, http.MethodPost, "/backend-api/f/conversation/prepare", encoded, token, nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &HTTPError{Status: status, Operation: "search_prepare"}
	}
	var prepared map[string]any
	if json.Unmarshal(body, &prepared) != nil {
		return nil, &SentinelError{Reason: "search_prepare_decode"}
	}
	conduit := stringValue(prepared["conduit_token"])
	if conduit == "" {
		return nil, &SentinelError{Reason: "search_conduit"}
	}
	requirements, err := c.Requirements(ctx, credentials, token)
	if err != nil {
		return nil, err
	}
	extra := map[string]string{"X-Conduit-Token": conduit, "OpenAI-Sentinel-Chat-Requirements-Token": requirements.Token}
	if requirements.Proof != "" {
		extra["OpenAI-Sentinel-Proof-Token"] = requirements.Proof
	}
	if requirements.Turnstile != "" {
		extra["OpenAI-Sentinel-Turnstile-Token"] = requirements.Turnstile
	}
	if requirements.SOToken != "" {
		extra["OpenAI-Sentinel-SO-Token"] = requirements.SOToken
	}
	conversation := map[string]any{
		"action": "next", "messages": []any{map[string]any{
			"id": fmt.Sprintf("message_%d", time.Now().UnixNano()), "author": map[string]string{"role": "user"}, "create_time": float64(time.Now().Unix()),
			"content": map[string]any{"content_type": "text", "parts": []string{prompt}}, "metadata": map[string]any{"system_hints": []string{"search"}, "serialization_metadata": map[string]any{"custom_symbol_offsets": []any{}}},
		}}, "parent_message_id": "client-created-root", "model": model, "client_prepare_state": "success",
		"timezone_offset_min": -480, "timezone": "Asia/Shanghai", "conversation_mode": map[string]string{"kind": "primary_assistant"},
		"enable_message_followups": true, "system_hints": []string{}, "supports_buffering": true, "supported_encodings": []string{"v1"},
		"force_use_search": true, "client_reported_search_source": "conversation_composer_web_icon", "client_contextual_info": map[string]string{"app_name": "chatgpt.com"},
	}
	encoded, _ = json.Marshal(conversation)
	body, status, err = c.requestWithHeaders(ctx, http.MethodPost, "/backend-api/f/conversation", encoded, token, extra)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &HTTPError{Status: status, Operation: "search"}
	}
	conversationID := parseConversationID(body)
	if conversationID == "" {
		return nil, &SentinelError{Reason: "search_conversation_id"}
	}
	var document map[string]any
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		documentBody, documentStatus, documentErr := c.request(ctx, http.MethodGet, "/backend-api/conversation/"+conversationID, nil, token)
		if documentErr != nil {
			return nil, documentErr
		}
		if documentStatus < 200 || documentStatus >= 300 {
			continue
		}
		if json.Unmarshal(documentBody, &document) != nil {
			continue
		}
		result := searchResult(conversationID, document)
		if result["answer"] != "" {
			return result, nil
		}
	}
	return searchResult(conversationID, document), nil
}

func parseConversationID(body []byte) string {
	for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var value map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &value) != nil {
			continue
		}
		if message, ok := value["message"].(map[string]any); ok {
			if conversationID := stringValue(message["conversation_id"]); conversationID != "" {
				return conversationID
			}
		}
		if conversationID := stringValue(value["conversation_id"]); conversationID != "" {
			return conversationID
		}
	}
	return ""
}

func searchResult(conversationID string, document map[string]any) map[string]any {
	answer := ""
	sources := make([]map[string]string, 0)
	seen := map[string]bool{}
	if mapping, ok := document["mapping"].(map[string]any); ok {
		for _, raw := range mapping {
			node, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			message, ok := node["message"].(map[string]any)
			if !ok {
				continue
			}
			author, _ := message["author"].(map[string]any)
			if stringValue(author["role"]) != "assistant" {
				continue
			}
			answer = searchText(message)
			walkSearchSources(message, &sources, seen)
		}
	}
	return map[string]any{"conversation_id": conversationID, "status": "", "answer": answer, "sources": sources}
}

func searchText(message map[string]any) string {
	content, _ := message["content"].(map[string]any)
	if parts, ok := content["parts"].([]any); ok {
		values := make([]string, 0, len(parts))
		for _, part := range parts {
			if text, ok := part.(string); ok {
				values = append(values, text)
			}
		}
		return strings.TrimSpace(strings.Join(values, "\n"))
	}
	return stringValue(content["text"])
}

func walkSearchSources(value any, sources *[]map[string]string, seen map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		url := stringValue(typed["url"])
		if url == "" {
			url = stringValue(typed["link"])
		}
		if (strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")) && !seen[url] {
			seen[url] = true
			*sources = append(*sources, map[string]string{"title": stringValue(typed["title"]), "url": url, "snippet": stringValue(typed["snippet"])})
		}
		for _, child := range typed {
			walkSearchSources(child, sources, seen)
		}
	case []any:
		for _, child := range typed {
			walkSearchSources(child, sources, seen)
		}
	}
}
