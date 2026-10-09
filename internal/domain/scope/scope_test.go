package scope

import "testing"

func TestValidateNormalizesEmptyModelsAndRejectsCrossChannelScope(t *testing.T) {
	channels, models, err := Validate([]string{"wb"}, nil, []string{"wb", "chatgpt"})
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(channels) != 1 || channels[0] != "wb" || len(models) != 1 || models[0] != "*" {
		t.Fatalf("unexpected normalized scope: channels=%#v models=%#v", channels, models)
	}
	if _, _, err := Validate([]string{"wb"}, []string{"chatgpt/model"}, []string{"wb", "chatgpt"}); err == nil {
		t.Fatal("Validate() accepted a model outside the channel scope")
	}
	if _, _, err := Validate([]string{"wb"}, []string{"wb/model-a", "wb/model-a"}, []string{"wb", "chatgpt"}); err == nil {
		t.Fatal("Validate() accepted duplicate model scopes")
	}
}

func TestDecisionMatchesCanonicalAndLegacyModels(t *testing.T) {
	channels := `[]`
	models := `["wb/model-a"]`
	if got := Decision(channels, models, "wb", "model-a"); got != "allowed" {
		t.Fatalf("canonical decision = %q", got)
	}
	if got := Decision(channels, models, "wb", "model-b"); got != "model_not_allowed" {
		t.Fatalf("denied decision = %q", got)
	}
	if got := Decision(`["wb"]`, `["*"]`, "chatgpt", "model-a"); got != "channel_not_allowed" {
		t.Fatalf("channel decision = %q", got)
	}
}

func TestDecodeKeyScopeRejectsMalformedStorage(t *testing.T) {
	if _, _, ok := DecodeKeyScope("not-json", `[")`); ok {
		t.Fatal("DecodeKeyScope() accepted malformed JSON")
	}
	if _, _, ok := DecodeKeyScope(`[]`, `["*", "wb/model"]`); ok {
		t.Fatal("DecodeKeyScope() accepted a mixed wildcard scope")
	}
}
