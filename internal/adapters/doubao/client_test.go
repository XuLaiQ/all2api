package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/XuLaiQ/all2api/internal/ports"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDoubaoClientUsesCookieAndParsesResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alice/slot/action_bar_v3/brief_list" {
			_, _ = w.Write([]byte(`{"data":[{"model_item_key":"model-a"}]}`))
			return
		}
		if r.Header.Get("Cookie") != "sessionid=x" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte("data: {\"content\":\"hello\"}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"cookie": "sessionid=x"}, nil
	})
	models, err := client.Models(context.Background())
	if err != nil || len(models) != 1 {
		t.Fatalf("Models()=%#v,%v", models, err)
	}
	result, err := client.Chat(context.Background(), ports.ChatInput{Model: "model-a", Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, "account")
	if err != nil || result.Text != "hello" {
		t.Fatalf("Chat()=%#v,%v", result, err)
	}
}

func TestDoubaoCatalogueUsesNativePostAndChatPageFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alice/slot/action_bar_v3/brief_list" {
			if r.Method != http.MethodPost || r.URL.Query().Get("aid") != "582478" || r.Header.Get("Cookie") != "sessionid=x; passport_csrf_token=csrf-1" || r.Header.Get("x-tt-passport-csrf-token") != "csrf-1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path == "/chat/" {
			_, _ = w.Write([]byte(`<!doctype html><script>window._ROUTER_DATA = {"modeSelectData":{"mode_list":{"item_list":[{"mode_id":"mode-1","model_list":{"item_list":[{"model_item_key":"doubao-test","name":"Test"}]}}]}}};</script>`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"cookie": "sessionid=x; passport_csrf_token=csrf-1"}, nil
	})
	models, err := client.ModelsWithAccount(context.Background(), "account")
	if err != nil || len(models) != 1 || models[0].UpstreamID != "doubao-test" {
		t.Fatalf("ModelsWithAccount()=%#v,%v", models, err)
	}
}

func TestDoubaoClientClassifiesChatHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"cookie": "sessionid=x"}, nil
	})
	_, err := client.Chat(context.Background(), ports.ChatInput{Model: "model-a", Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, "account")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusTooManyRequests || httpErr.Kind() != "upstream_rate_limited" {
		t.Fatalf("Chat() error = %T/%v", err, err)
	}
}

func TestDoubaoClientMapsSamanthaImageAndVideoCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/samantha/chat/completion" || r.URL.Query().Get("aid") != "582478" || r.Header.Get("Cookie") != "sessionid=x" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var payload map[string]any
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		messages, _ := payload["messages"].([]any)
		message, _ := messages[0].(map[string]any)
		contentType := int(message["content_type"].(float64))
		w.Header().Set("Content-Type", "text/event-stream")
		if contentType == 2009 {
			_, _ = w.Write([]byte("data: {\"event_data\":\"{\\\"message\\\":{\\\"content_type\\\":2010,\\\"content\\\":{\\\"data\\\":[{\\\"image_ori_raw\\\":\\\"https://img.example/a.png\\\"}]}}}\"}\n\ndata: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"event_data\":\"{\\\"message\\\":{\\\"content_type\\\":2021,\\\"content\\\":{\\\"data\\\":[{\\\"video_url\\\":\\\"https://video.example/a.mp4\\\"}]}}}\"}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"cookie": "sessionid=x"}, nil
	})
	image, err := client.CapabilityWithAccount(context.Background(), ports.CapabilityInput{Capability: "image", Prompt: "sunset", Ratio: "1:1"}, "account")
	if err != nil || image.StatusCode != http.StatusOK {
		t.Fatalf("image capability = %#v/%v", image, err)
	}
	video, err := client.CapabilityWithAccount(context.Background(), ports.CapabilityInput{Capability: "video", Prompt: "ocean", Duration: intPointer(5)}, "account")
	if err != nil || video.StatusCode != http.StatusOK {
		t.Fatalf("video capability = %#v/%v", video, err)
	}
}

func TestDoubaoSamanthaErrorUsesTypedRateLimitClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"event_data\":\"{\\\"code\\\":\\\"710022002\\\",\\\"message\\\":\\\"limited\\\"}\"}\n\n"))
	}))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"cookie": "sessionid=x"}, nil
	})
	_, err := client.CapabilityWithAccount(context.Background(), ports.CapabilityInput{Capability: "image", Prompt: "x"}, "account")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusTooManyRequests || httpErr.Kind() != "upstream_rate_limited" {
		t.Fatalf("capability error = %T/%v", err, err)
	}
}

func intPointer(value int) *int { return &value }
