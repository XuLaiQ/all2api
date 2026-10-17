package grok

import (
	"strings"

	"github.com/XuLaiQ/all2api/internal/ports"
)

const (
	tierAuto  = "auto"
	tierBasic = "basic"
	tierSuper = "super"
	tierHeavy = "heavy"
)

type modelSpec struct {
	ID          string
	DisplayName string
	Mode        string
	MinTier     string
}

// These IDs and Gateway modes mirror the Grok Web catalog used by the
// reference implementation. Media models are intentionally omitted until the
// shared All2API media contract is wired to Grok's separate protocols.
var catalog = []modelSpec{
	{ID: "grok-chat-fast", DisplayName: "Grok Fast", Mode: "fast", MinTier: tierBasic},
	{ID: "grok-chat-auto", DisplayName: "Grok Auto", Mode: "auto", MinTier: tierSuper},
	{ID: "grok-chat-expert", DisplayName: "Grok Expert", Mode: "expert", MinTier: tierSuper},
	{ID: "grok-chat-heavy", DisplayName: "Grok Heavy", Mode: "heavy", MinTier: tierHeavy},
}

func catalogModels(tier string) []ports.ModelDescriptor {
	tier = normalizedTier(tier)
	result := make([]ports.ModelDescriptor, 0, len(catalog))
	for _, spec := range catalog {
		if !tierSupports(tier, spec.MinTier) {
			continue
		}
		result = append(result, ports.ModelDescriptor{UpstreamID: spec.ID, DisplayName: spec.DisplayName, Capabilities: []string{"chat"}})
	}
	return result
}

func resolveModel(model string) (modelSpec, bool) {
	for _, spec := range catalog {
		if spec.ID == strings.TrimSpace(model) {
			return spec, true
		}
	}
	return modelSpec{}, false
}

func normalizedTier(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case tierSuper:
		return tierSuper
	case tierHeavy:
		return tierHeavy
	case tierBasic:
		return tierBasic
	default:
		return tierAuto
	}
}

func tierSupports(actual, minimum string) bool {
	rank := map[string]int{tierBasic: 1, tierSuper: 2, tierHeavy: 3}
	if actual == tierAuto {
		actual = tierBasic
	}
	return rank[actual] >= rank[minimum]
}
