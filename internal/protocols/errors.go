package protocols

import (
	"encoding/json"
	"fmt"
)

type RequestError struct {
	Message string
}

func (e *RequestError) Error() string { return e.Message }

func anthropicType(status int, code string) string {
	if status == 401 || code == "invalid_api_key" {
		return "authentication_error"
	}
	switch status {
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	case 400, 422:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

func AnthropicError(message string, status int, code string) map[string]any {
	return map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    anthropicType(status, code),
			"message": message,
		},
	}
}

func SSEEvent(event string, value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return []byte(fmt.Sprintf("event: %s\ndata: {}\n\n", event))
	}
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, data))
}
