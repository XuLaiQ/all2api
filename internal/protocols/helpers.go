package protocols

import (
	"fmt"
	"math"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func positiveInt(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, number > 0
	case float64:
		if number >= 1 && number <= math.MaxInt && math.Trunc(number) == number {
			return int(number), true
		}
	}
	return 0, false
}

func optionalBool(payload map[string]any, field string) (bool, error) {
	value, ok := payload[field]
	if !ok {
		return false, nil
	}
	result, ok := value.(bool)
	if !ok {
		return false, &RequestError{Message: field + " must be a boolean"}
	}
	return result, nil
}

func optionalNumber(payload map[string]any, field string) (*float64, error) {
	value, ok := payload[field]
	if !ok {
		return nil, nil
	}
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return nil, &RequestError{Message: field + " must be numeric"}
	}
	return &number, nil
}

func normalizeTools(value any) ([]ports.ToolDefinition, error) {
	// This declaration is kept in the protocol package so both Anthropic and
	// future OpenAI protocol normalizers share the same tool validation.
	tools, ok := value.([]any)
	if !ok {
		return nil, &RequestError{Message: "tools must be an array"}
	}
	result := make([]ports.ToolDefinition, 0, len(tools))
	seen := make(map[string]struct{}, len(tools))
	for index, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			return nil, &RequestError{Message: fmt.Sprintf("tools[%d] must be an object", index)}
		}
		name, ok := tool["name"].(string)
		if !ok || name == "" {
			return nil, &RequestError{Message: fmt.Sprintf("tools[%d].name must be non-empty", index)}
		}
		if _, exists := seen[name]; exists {
			return nil, &RequestError{Message: "tool names must be unique"}
		}
		seen[name] = struct{}{}
		schema, ok := tool["input_schema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			return nil, &RequestError{Message: fmt.Sprintf("tools[%d].input_schema must describe an object", index)}
		}
		description, _ := tool["description"].(string)
		result = append(result, ports.ToolDefinition{Name: name, Description: description, Parameters: schema})
	}
	return result, nil
}

func normalizeToolChoice(value any, tools []ports.ToolDefinition) (any, error) {
	choice, ok := value.(map[string]any)
	if !ok {
		return nil, &RequestError{Message: "tool_choice must be an object"}
	}
	typeName, _ := choice["type"].(string)
	switch typeName {
	case "auto", "none":
		return typeName, nil
	case "any":
		return "required", nil
	case "tool":
		name, ok := choice["name"].(string)
		if !ok || name == "" {
			return nil, &RequestError{Message: "tool_choice.name is required"}
		}
		for _, tool := range tools {
			if tool.Name == name {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}, nil
			}
		}
		return nil, &RequestError{Message: "tool_choice.name must match a declared tool"}
	default:
		return nil, &RequestError{Message: "tool_choice.type must be auto, any, none, or tool"}
	}
}
