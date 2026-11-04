package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func TestClientModelsAndChatUseNativeHTTPContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v3/config" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"models":[{"id":"model-a","name":"Model A"}]}`))
			return
		}
		if request.URL.Path == "/v2/chat/completions" {
			if request.Header.Get("Authorization") != "Bearer access-token" {
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = writer.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"access_token": "access-token"}, nil
	})
	models, err := client.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].UpstreamID != "model-a" {
		t.Fatalf("Models() = %#v, %v", models, err)
	}
	result, err := client.Chat(context.Background(), ports.ChatInput{Model: "model-a", Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, "account-1")
	if err != nil || result.Text != "hello" || result.Usage == nil {
		t.Fatalf("Chat() = %#v, %v", result, err)
	}
}

func TestParseModelsRejectsDisabledEntries(t *testing.T) {
	models := parseModels([]byte(`{"data":[{"id":"a"},{"id":"b","disabled":true},"c"]}`))
	if len(models) != 2 || !strings.Contains(models[0].UpstreamID+models[1].UpstreamID, "a") {
		t.Fatalf("parseModels() = %#v", models)
	}
}

func TestRuntimeHeadersCarryAccountContext(t *testing.T) {
	client := NewClient("https://copilot.example", nil)
	headers := client.runtimeHeaders(map[string]string{"realm": "global", "uid": "user-1", "domain": "www.workbuddy.ai", "enterprise_id": "enterprise-1", "device_token": "device-1"}, "access-1")
	for key, expected := range map[string]string{"Authorization": "Bearer access-1", "X-User-Id": "user-1", "X-Domain": "www.workbuddy.ai", "X-Enterprise-Id": "enterprise-1", "X-Tenant-Id": "enterprise-1", "X-Device-Token": "device-1", "X-CodeBuddy-Request": "1", "Origin": "https://www.workbuddy.ai"} {
		if headers[key] != expected {
			t.Fatalf("header %s = %q, want %q", key, headers[key], expected)
		}
	}
}

func TestClientRefreshUsesNativeTokenRotationContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v2/plugin/auth/token/refresh" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if request.Header.Get("X-Refresh-Token") != "refresh-1" || request.Header.Get("Authorization") != "Bearer access-1" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = writer.Write([]byte(`{"code":0,"data":{"accessToken":"access-2","refreshToken":"refresh-2"}}`))
	}))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"access_token": "access-1", "refresh_token": "refresh-1", "realm": "cn"}, nil
	})
	updated, err := client.Refresh(context.Background(), "cn:user-1")
	if err != nil || updated["access_token"] != "access-2" || updated["refresh_token"] != "refresh-2" {
		t.Fatalf("Refresh() = %#v/%v", updated, err)
	}
}

// TestGlobalChatUsesConsolePathWithSystemFallback locks the global-realm fix:
// global accounts must hit /console/chat/completions with a leading system
// message (upstream rejects /v2 with 11-128 "request illegal").
func TestGlobalChatUsesConsolePathWithSystemFallback(t *testing.T) {
	var gotPath, gotFirstRole string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		var payload struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(request.Body).Decode(&payload)
		if len(payload.Messages) > 0 {
			gotFirstRole = payload.Messages[0].Role
		}
		_, _ = writer.Write([]byte(`{"id":"c1","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	client := NewClient("https://copilot.tencent.com", func(context.Context, string) (map[string]string, error) {
		return map[string]string{"access_token": "tok", "realm": "global", "domain": "www.workbuddy.ai", "uid": "u1"}, nil
	})
	client.GlobalBaseURL = server.URL
	if _, err := client.Chat(context.Background(), ports.ChatInput{Model: "m", Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, "acct"); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if gotPath != "/console/chat/completions" {
		t.Fatalf("global chat path = %q, want /console/chat/completions", gotPath)
	}
	if gotFirstRole != "system" {
		t.Fatalf("global chat first role = %q, want system", gotFirstRole)
	}
}

// TestGlobalChatFallsBackToV2OnNotFound verifies the 404/405 fallback keeps the
// channel usable if the console path disappears upstream.
func TestGlobalChatFallsBackToV2OnNotFound(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		if request.URL.Path == "/console/chat/completions" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = writer.Write([]byte(`{"id":"c2","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	client := NewClient("https://copilot.tencent.com", func(context.Context, string) (map[string]string, error) {
		return map[string]string{"access_token": "tok", "realm": "global", "uid": "u1"}, nil
	})
	client.GlobalBaseURL = server.URL
	if _, err := client.Chat(context.Background(), ports.ChatInput{Model: "m", Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, "acct"); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if len(paths) != 2 || paths[1] != "/v2/chat/completions" {
		t.Fatalf("paths = %v, want [/console/... /v2/...]", paths)
	}
}
