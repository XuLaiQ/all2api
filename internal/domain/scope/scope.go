package scope

import (
	"encoding/json"
	"fmt"
	"strings"
)

type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func NormalizeModels(models []string) []string {
	if len(models) == 0 {
		return []string{"*"}
	}
	seen := make(map[string]struct{}, len(models))
	result := make([]string, 0, len(models))
	for _, model := range models {
		value := strings.TrimSpace(model)
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func Validate(channels, models, knownChannels []string) ([]string, []string, error) {
	known := make(map[string]struct{}, len(knownChannels))
	for _, channel := range knownChannels {
		known[channel] = struct{}{}
	}

	normalizedChannels := make([]string, 0, len(channels))
	channelSeen := make(map[string]struct{}, len(channels))
	for _, channel := range channels {
		value := strings.TrimSpace(channel)
		if _, ok := channelSeen[value]; ok {
			return nil, nil, &ValidationError{Message: "channels must be unique"}
		}
		channelSeen[value] = struct{}{}
		if value == "" {
			return nil, nil, &ValidationError{Message: "channels contains an unknown channel"}
		}
		if _, ok := known[value]; !ok {
			return nil, nil, &ValidationError{Message: "channels contains an unknown channel"}
		}
		normalizedChannels = append(normalizedChannels, value)
	}

	normalizedModels := make([]string, 0, len(models))
	modelSeen := make(map[string]struct{}, len(normalizedModels))
	if len(models) == 0 {
		normalizedModels = []string{"*"}
		modelSeen["*"] = struct{}{}
	}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if _, ok := modelSeen[model]; ok {
			return nil, nil, &ValidationError{Message: "models must be a non-empty unique list"}
		}
		modelSeen[model] = struct{}{}
		normalizedModels = append(normalizedModels, model)
		if model == "" || len(model) > 320 {
			return nil, nil, &ValidationError{Message: "models contains an invalid model scope"}
		}
	}
	if _, wildcard := modelSeen["*"]; wildcard && len(normalizedModels) != 1 {
		return nil, nil, &ValidationError{Message: "wildcard model scope cannot be combined with model ids"}
	}

	for _, model := range normalizedModels {
		if model == "*" {
			continue
		}
		prefix, target, ok := strings.Cut(model, "/")
		if !ok {
			continue
		}
		if prefix == "" || target == "" {
			return nil, nil, &ValidationError{Message: "models contains an invalid model scope"}
		}
		if _, ok := known[prefix]; !ok {
			return nil, nil, &ValidationError{Message: fmt.Sprintf("models contains an unknown channel: %s", prefix)}
		}
		if len(normalizedChannels) > 0 && !contains(normalizedChannels, prefix) {
			return nil, nil, &ValidationError{Message: fmt.Sprintf("model scope channel %q is not included in channels", prefix)}
		}
	}
	return normalizedChannels, normalizedModels, nil
}

func DecodeKeyScope(channelsJSON, modelsJSON string) ([]string, []string, bool) {
	channels, channelsOK := decodeStringList(channelsJSON)
	models, modelsOK := decodeStringList(modelsJSON)
	if !channelsOK || !modelsOK {
		return nil, nil, false
	}
	normalizedModels := NormalizeModels(models)
	if len(normalizedModels) == 0 || len(normalizedModels) > 128 {
		return nil, nil, false
	}
	for _, value := range channels {
		if strings.TrimSpace(value) == "" {
			return nil, nil, false
		}
	}
	for _, value := range normalizedModels {
		if value == "" || len(value) > 320 {
			return nil, nil, false
		}
	}
	if contains(normalizedModels, "*") && len(normalizedModels) != 1 {
		return nil, nil, false
	}
	return channels, normalizedModels, true
}

func decodeStringList(value string) ([]string, bool) {
	if !strings.HasPrefix(strings.TrimSpace(value), "[") {
		return nil, false
	}
	var result []string
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil, false
	}
	return result, true
}

func ChannelAllowed(channels []string, channel string) bool {
	return len(channels) == 0 || contains(channels, channel)
}

func ModelAllowed(models []string, channel, upstreamModel string) bool {
	fullModel := channel + "/" + upstreamModel
	for _, model := range models {
		if model == "*" || model == fullModel || model == upstreamModel {
			return true
		}
		prefix, target, ok := strings.Cut(model, "/")
		if ok && target == "*" && prefix == channel {
			return true
		}
	}
	return false
}

func Decision(channelsJSON, modelsJSON, channel, upstreamModel string) string {
	channels, models, ok := DecodeKeyScope(channelsJSON, modelsJSON)
	if !ok {
		return "invalid_scope"
	}
	if !ChannelAllowed(channels, channel) {
		return "channel_not_allowed"
	}
	if !ModelAllowed(models, channel, upstreamModel) {
		return "model_not_allowed"
	}
	return "allowed"
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
