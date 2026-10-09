package accounts

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func TestBuildProviderImportsNormalizesDoubaoCookieAndChatGPTToken(t *testing.T) {
	doubao, err := buildProviderImports("doubao", map[string]any{"cookie": "sessionid=demo"})
	if err != nil || len(doubao) != 1 || doubao[0].Kind != "cookie" || doubao[0].Credentials["cookie"] != "sessionid=demo" {
		t.Fatalf("doubao imports = %#v/%v", doubao, err)
	}
	chatgpt, err := buildProviderImports("chatgpt", map[string]any{"tokens": []any{"token-demo"}})
	if err != nil || len(chatgpt) != 1 || chatgpt[0].Kind != "token" || chatgpt[0].Credentials["access_token"] != "token-demo" {
		t.Fatalf("chatgpt imports = %#v/%v", chatgpt, err)
	}
	if chatgpt[0].NativeID == "token-demo" {
		t.Fatal("ChatGPT native id leaked token")
	}
}

func TestBuildProviderImportsPreservesChatGPTWebMetadata(t *testing.T) {
	imports, err := buildProviderImports("chatgpt", map[string]any{
		"tokens": []any{map[string]any{
			"access_token":       "token-demo",
			"refresh_token":      "refresh-demo",
			"id_token":           "id-demo",
			"proxy":              "http://127.0.0.1:7892",
			"user_agent":         "fixture-agent/1.0",
			"oai_device_id":      "device-demo",
			"oai_session_id":     "session-demo",
			"impersonate":        "chrome110",
			"chatgpt_account_id": "acct-demo",
			"fp": map[string]any{
				"sec-ch-ua":          `"Fixture";v="1"`,
				"sec_ch_ua_platform": `"FixtureOS"`,
			},
		}},
	})
	if err != nil || len(imports) != 1 {
		t.Fatalf("ChatGPT imports = %#v/%v", imports, err)
	}
	credentials := imports[0].Credentials
	for key, expected := range map[string]string{
		"access_token":       "token-demo",
		"refresh_token":      "refresh-demo",
		"id_token":           "id-demo",
		"auth_mode":          "web",
		"proxy":              "http://127.0.0.1:7892",
		"user_agent":         "fixture-agent/1.0",
		"oai_device_id":      "device-demo",
		"oai_session_id":     "session-demo",
		"impersonate":        "chrome110",
		"chatgpt_account_id": "acct-demo",
	} {
		if credentials[key] != expected {
			t.Fatalf("credential %s = %q, want %q", key, credentials[key], expected)
		}
	}
	var fingerprint map[string]any
	if err := json.Unmarshal([]byte(credentials["fp"]), &fingerprint); err != nil || fingerprint["sec-ch-ua"] == nil {
		t.Fatalf("fingerprint credential = %q/%v", credentials["fp"], err)
	}
}

type provisionRepositoryStub struct {
	imports []ports.AccountImport
}

func (p *provisionRepositoryStub) ImportAccounts(_ context.Context, imports []ports.AccountImport, _ ports.SecretBox, _ ports.AuditEvent) (ports.AccountImportResult, error) {
	p.imports = append([]ports.AccountImport(nil), imports...)
	return ports.AccountImportResult{Added: len(imports)}, nil
}

func (p *provisionRepositoryStub) SaveProvisionSession(context.Context, ports.ProvisionSession, ports.SecretBox) error {
	return nil
}

func (p *provisionRepositoryStub) LoadProvisionSession(context.Context, string, string, ports.SecretBox) (ports.ProvisionSession, error) {
	return ports.ProvisionSession{}, nil
}

func (p *provisionRepositoryStub) DeleteProvisionSession(context.Context, string, string) error {
	return nil
}

func (p *provisionRepositoryStub) GetProvisionIdempotency(context.Context, string, string, string, ports.SecretBox) (ports.ProvisionIdempotency, bool, error) {
	return ports.ProvisionIdempotency{}, false, nil
}

func (p *provisionRepositoryStub) PutProvisionIdempotency(context.Context, string, string, string, string, map[string]any, float64, ports.SecretBox) error {
	return nil
}

func TestImportAcceptsProviderTokenFlows(t *testing.T) {
	for _, item := range []struct {
		channel string
		payload map[string]any
		kind    string
	}{
		{channel: "doubao", payload: map[string]any{"cookie": "sessionid=demo"}, kind: "cookie"},
		{channel: "chatgpt", payload: map[string]any{"tokens": []any{"token-demo"}}, kind: "token"},
	} {
		t.Run(item.channel, func(t *testing.T) {
			repository := &provisionRepositoryStub{}
			service := NewProvisionService(repository, nil)
			result, err := service.Import(context.Background(), item.channel, item.payload, "admin", "127.0.0.1", "idempotency-"+item.channel)
			if err != nil {
				t.Fatalf("Import() error = %v", err)
			}
			if result.Added != 1 || len(repository.imports) != 1 || repository.imports[0].Kind != item.kind {
				t.Fatalf("Import() result/imports = %#v/%#v", result, repository.imports)
			}
		})
	}
}
