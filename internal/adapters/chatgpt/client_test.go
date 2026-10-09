package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func TestChatGPTClientParsesCatalogueAndWebSSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><script src="/assets/app.js"></script></html>`))
			return
		}
		if r.URL.Path == "/backend-api/sentinel/chat-requirements/prepare" {
			_, _ = w.Write([]byte(`{"prepare_token":"prepare-1"}`))
			return
		}
		if r.URL.Path == "/backend-api/sentinel/chat-requirements/finalize" {
			_, _ = w.Write([]byte(`{"token":"requirements-1"}`))
			return
		}
		if r.URL.Path == "/backend-api/models" {
			if r.URL.Query().Get("history_and_training_disabled") != "false" || r.Header.Get("X-OpenAI-Target-Path") != "/backend-api/models" || r.Header.Get("OAI-Client-Build-Number") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-test","title":"Test"}]}`))
			return
		}
		if r.URL.Path == "/backend-api/conversation" {
			if r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Token") != "requirements-1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var payload map[string]any
			if json.NewDecoder(r.Body).Decode(&payload) != nil || payload["websocket_request_id"] == nil || payload["client_contextual_info"] == nil || r.Header.Get("X-OpenAI-Target-Path") != "/backend-api/conversation" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"p\":\"/message/content/parts/0\",\"o\":\"append\",\"v\":\"hel\"}\n\ndata: {\"p\":\"/message/content/parts/0\",\"o\":\"append\",\"v\":\"lo\"}\n\ndata: [DONE]\n\n"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"access_token": "token"}, nil
	})
	models, err := client.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].UpstreamID != "gpt-test" {
		t.Fatalf("Models() = %#v, %v", models, err)
	}
	result, err := client.Chat(context.Background(), ports.ChatInput{Model: "gpt-test", Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, "account")
	if err != nil || result.Text != "hello" {
		t.Fatalf("Chat() = %#v, %v", result, err)
	}
}

func TestChatGPTCredentialFingerprintUsesWebConversationOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "codex") || strings.Contains(r.URL.Path, "/v1/") {
			t.Fatalf("unexpected non-Web ChatGPT path: %s", r.URL.Path)
		}
		if r.URL.Path != "/backend-api/conversation" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		checks := map[string]string{
			"User-Agent":             "fixture-web-agent/1.0",
			"OAI-Device-Id":          "device-fixture",
			"OAI-Session-Id":         "session-fixture",
			"Sec-Ch-Ua":              `"Fixture";v="1"`,
			"Sec-Ch-Ua-Full-Version": `"1.0.0.0"`,
			"Sec-Ch-Ua-Platform":     `"FixtureOS"`,
			"X-OpenAI-Target-Path":   "/backend-api/conversation",
			"X-OpenAI-Target-Route":  "/backend-api/conversation",
			"Authorization":          "Bearer web-token",
		}
		for header, expected := range checks {
			if actual := r.Header.Get(header); actual != expected {
				w.WriteHeader(http.StatusBadRequest)
				t.Fatalf("header %s = %q, want %q", header, actual, expected)
			}
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["websocket_request_id"] == nil || payload["client_contextual_info"] == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"p\":\"/message/content/parts/0\",\"o\":\"append\",\"v\":\"hello\"}\n\ndata: {\"o\":\"patch\",\"v\":[{\"p\":\"/message/content/parts/0\",\"o\":\"append\",\"v\":\" web\"}]}\n\ndata: {\"v\":\"!\"}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{
			"access_token":       "web-token",
			"auth_mode":          "oauth",
			"chatgpt_account_id": "acct_fixture",
			"impersonate":        "chrome110",
			"fp":                 `{"user-agent":"fixture-web-agent/1.0","oai-device-id":"device-fixture","oai-session-id":"session-fixture","sec-ch-ua":"\"Fixture\";v=\"1\"","sec_ch_ua_full_version":"\"1.0.0.0\"","sec-ch-ua-platform":"\"FixtureOS\""}`,
		}, nil
	})
	client.SentinelEnabled = false
	result, err := client.Chat(context.Background(), ports.ChatInput{Model: "gpt-web", Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, "account")
	if err != nil || result.Text != "hello web!" {
		t.Fatalf("Web-only Chat() = %#v/%v", result, err)
	}
}

func TestChatGPTOAuthCredentialsStillUseWebEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<html><script src="/assets/app.js"></script></html>`))
		case "/backend-api/sentinel/chat-requirements/prepare":
			_, _ = w.Write([]byte(`{"prepare_token":"prepare-web"}`))
		case "/backend-api/sentinel/chat-requirements/finalize":
			_, _ = w.Write([]byte(`{"token":"requirements-web"}`))
		case "/backend-api/models":
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-web","title":"Web"}]}`))
		case "/backend-api/conversation":
			if r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Token") != "requirements-web" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"message\":{\"id\":\"web-oauth\",\"author\":{\"role\":\"assistant\"},\"content\":{\"parts\":[\"hello\"]}}}\n\ndata: [DONE]\n\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"access_token": "oauth-token", "id_token": "id-token", "client_id": "client-1", "auth_mode": "oauth"}, nil
	})
	models, err := client.ModelsWithAccount(context.Background(), "account")
	if err != nil || len(models) != 1 || models[0].UpstreamID != "gpt-web" {
		t.Fatalf("Web ModelsWithAccount() = %#v/%v", models, err)
	}
	result, err := client.Chat(context.Background(), ports.ChatInput{Model: "gpt-web", Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, "account")
	if err != nil || result.Text != "hello" {
		t.Fatalf("Web Chat() = %#v/%v", result, err)
	}
}

func TestChatGPTClientClassifiesConversationHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"access_token": "token"}, nil
	})
	_, err := client.Chat(context.Background(), ports.ChatInput{Model: "gpt-test", Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, "account")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnauthorized || httpErr.Kind() != "credential_rejected" {
		t.Fatalf("Chat() error = %T/%v", err, err)
	}
}

func TestChatGPTSentinelChallengeIsNotSilentlyAccepted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<html></html>`))
		case "/backend-api/sentinel/chat-requirements/prepare":
			_, _ = w.Write([]byte(`{"arkose":{"required":true}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"access_token": "token"}, nil
	})
	_, err := client.Chat(context.Background(), ports.ChatInput{Model: "gpt-test", Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, "account")
	var sentinel *SentinelError
	if !errors.As(err, &sentinel) || sentinel.Reason != "arkose" || sentinel.Kind() != "upstream_protocol_error" {
		t.Fatalf("sentinel error = %T/%v", err, err)
	}
}

func TestChatGPTSearchUsesPrepareSentinelConversationAndRedactsSources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<html><script src="/assets/app.js"></script></html>`))
		case "/backend-api/sentinel/chat-requirements/prepare":
			_, _ = w.Write([]byte(`{"prepare_token":"prepare-1"}`))
		case "/backend-api/sentinel/chat-requirements/finalize":
			_, _ = w.Write([]byte(`{"token":"requirements-1"}`))
		case "/backend-api/f/conversation/prepare":
			_, _ = w.Write([]byte(`{"conduit_token":"conduit-1"}`))
		case "/backend-api/f/conversation", "/backend-api/conversation":
			if r.Header.Get("X-Conduit-Token") != "conduit-1" || r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Token") != "requirements-1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"message\":{\"conversation_id\":\"conv-1\"}}\n\ndata: [DONE]\n\n"))
		case "/backend-api/conversation/conv-1":
			_, _ = w.Write([]byte(`{"mapping":{"1":{"message":{"author":{"role":"assistant"},"content":{"parts":["answer"]},"metadata":{"url":"https://source.example/a","title":"Source"}}}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"access_token": "token"}, nil
	})
	result, err := client.CapabilityWithAccount(context.Background(), ports.CapabilityInput{Capability: "search", Model: "gpt-test", Prompt: "latest"}, "account")
	if err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("search capability = %#v/%v", result, err)
	}
	body, ok := result.Body.(map[string]any)
	if !ok || body["answer"] != "answer" {
		t.Fatalf("search body = %#v", result.Body)
	}
	sources, ok := body["sources"].([]map[string]string)
	if !ok || len(sources) != 1 || sources[0]["url"] != "https://source.example/a" {
		t.Fatalf("search sources = %#v", body["sources"])
	}
}

func TestChatGPTImageContractUploadsUsesSentinelAndDownloadsResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<html></html>`))
		case "/backend-api/sentinel/chat-requirements/prepare":
			_, _ = w.Write([]byte(`{"prepare_token":"prepare-1"}`))
		case "/backend-api/sentinel/chat-requirements/finalize":
			_, _ = w.Write([]byte(`{"token":"requirements-1"}`))
		case "/backend-api/files":
			_, _ = w.Write([]byte(`{"upload_url":"http://` + r.Host + `/upload/file-1","file_id":"file-1"}`))
		case "/upload/file-1":
			w.WriteHeader(http.StatusCreated)
		case "/backend-api/files/file-1/uploaded":
			w.WriteHeader(http.StatusOK)
		case "/backend-api/f/conversation/prepare":
			_, _ = w.Write([]byte(`{"conduit_token":"conduit-1"}`))
		case "/backend-api/f/conversation", "/backend-api/conversation":
			if r.Header.Get("X-Conduit-Token") != "conduit-1" || r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Token") != "requirements-1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"message\":{\"conversation_id\":\"conv-image\",\"content\":{\"parts\":[\"file-service://result-1\"]}}}\n\ndata: [DONE]\n\n"))
		case "/backend-api/files/download/result-1":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("image-result"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"access_token": "token"}, nil
	})
	input := ports.CapabilityInput{Capability: "image", Model: "gpt-image", Prompt: "draw a line", Images: []string{"data:image/png;base64,aW1hZ2U="}}
	result, err := client.CapabilityWithAccount(context.Background(), input, "account")
	if err != nil || result.StatusCode != http.StatusOK {
		var sentinel *SentinelError
		if errors.As(err, &sentinel) {
			t.Fatalf("image capability = %#v sentinel_reason=%s", result, sentinel.Reason)
		}
		t.Fatalf("image capability = %#v/%v", result, err)
	}
	body, ok := result.Body.(map[string]any)
	if !ok || body["created"] == nil {
		t.Fatalf("image body = %#v", result.Body)
	}
	items, ok := body["data"].([]map[string]string)
	if !ok || len(items) != 1 || items[0]["b64_json"] == "" {
		t.Fatalf("image data = %#v", body["data"])
	}
}

func TestChatGPTEditableCapabilityValidatesKindBeforeUpstreamCall(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", func(context.Context, string) (map[string]string, error) {
		return map[string]string{"access_token": "token"}, nil
	})
	_, err := client.CapabilityWithAccount(context.Background(), ports.CapabilityInput{Capability: "editable", Kind: "pdf", Model: "gpt-test", Prompt: "x"}, "account")
	if err == nil || !strings.Contains(err.Error(), "editable file kind") {
		t.Fatalf("editable validation error = %v", err)
	}
}
