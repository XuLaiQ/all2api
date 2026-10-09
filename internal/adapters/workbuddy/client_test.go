package workbuddy

import (
	"context"
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
