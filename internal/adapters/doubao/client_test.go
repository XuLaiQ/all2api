package doubao

import (
	"context"
	"encoding/base64"
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
		if r.Header.Get("Cookie") != "sessionid=x" || r.URL.Path != "/chat/completion" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: CHUNK_DELTA\ndata: {\"text\":\"hel\"}\n\nevent: CHUNK_DELTA\ndata: {\"text\":\"lo\"}\n\n"))
	}))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"cookie": "sessionid=x"}, nil
	})
	models, err := client.Models(context.Background())
	if err != nil || len(models) != 1+len(chatModels) || models[0].UpstreamID != "model-a" {
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
	if err != nil || len(models) != 1+len(chatModels) || models[0].UpstreamID != "doubao-test" || models[len(models)-1].UpstreamID != "doubao-expert" {
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
			_, _ = w.Write([]byte(samanthaFrame(2001, map[string]any{"message": map[string]any{"content_type": 2010, "content": mustJSON(map[string]any{"data": []any{
				map[string]any{"image_thumb": map[string]any{"url": "https://img.example/thumb.png"}, "image_ori": map[string]any{"url": "https://img.example/marked.png"}, "image_ori_raw": map[string]any{"url": "https://img.example/raw.png", "width": 1024}},
				map[string]any{"image": map[string]any{"image_ori": map[string]any{"url": "https://img.example/only-marked.png"}}},
			}})}})))
			// Non-CMPL events must be ignored even when they look like results.
			_, _ = w.Write([]byte(samanthaFrame(2002, map[string]any{"message": map[string]any{"content_type": 2010, "content": mustJSON(map[string]any{"data": []any{map[string]any{"image_ori_raw": "https://img.example/ignored.png"}}})}})))
			return
		}
		_, _ = w.Write([]byte(samanthaFrame(2001, map[string]any{"message": map[string]any{"content_type": 2021, "content": mustJSON(map[string]any{"data": []any{map[string]any{"video_url": "https://video.example/a.mp4"}}})}})))
	}))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{"cookie": "sessionid=x"}, nil
	})
	image, err := client.CapabilityWithAccount(context.Background(), ports.CapabilityInput{Capability: "image", Prompt: "sunset", Ratio: "1:1"}, "account")
	if err != nil || image.StatusCode != http.StatusOK {
		t.Fatalf("image capability = %#v/%v", image, err)
	}
	images := image.Body.(map[string]any)["data"].([]map[string]any)
	if len(images) != 2 || images[0]["url"] != "https://img.example/raw.png" || images[0]["watermark_free"] != true || images[0]["width"] != 1024 ||
		images[1]["url"] != "https://img.example/only-marked.png" || images[1]["watermark_free"] != nil {
		t.Fatalf("images = %#v", images)
	}
	video, err := client.CapabilityWithAccount(context.Background(), ports.CapabilityInput{Capability: "video", Prompt: "ocean", Duration: intPointer(5)}, "account")
	if err != nil || video.StatusCode != http.StatusOK {
		t.Fatalf("video capability = %#v/%v", video, err)
	}
}

func samanthaFrame(eventType int, eventData map[string]any) string {
	return "data: " + mustJSON(map[string]any{"event_type": eventType, "event_data": mustJSON(eventData)}) + "\n\n"
}

func TestDoubaoVideoPollsAsyncTaskAndDecodesVideoModel(t *testing.T) {
	var pollBody map[string]any
	var pollHasFP bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch r.URL.Path {
		case "/samantha/chat/completion":
			_, _ = w.Write([]byte(samanthaFrame(2001, map[string]any{"message": map[string]any{"content_type": 2001, "content": mustJSON(map[string]any{"text": "生成中"})}})))
			_, _ = w.Write([]byte(samanthaFrame(2001, map[string]any{"fin_reason": map[string]any{"reason": 1, "async_task": map[string]any{"id": "task-9"}}})))
		case "/samantha/chat/async/stream":
			pollHasFP = r.URL.Query().Has("fp")
			_ = json.NewDecoder(r.Body).Decode(&pollBody)
			videoModel := mustJSON(map[string]any{"video_list": map[string]any{"video_1": map[string]any{"main_url": base64.StdEncoding.EncodeToString([]byte("https://video.example/clean.mp4"))}}})
			_, _ = w.Write([]byte(samanthaFrame(2001, map[string]any{"message": map[string]any{"content_type": 2021, "content": mustJSON(map[string]any{"data": []any{map[string]any{"video_model": videoModel}}})}})))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	result, err := newTestClient(server.URL).CapabilityWithAccount(context.Background(), ports.CapabilityInput{Capability: "video", Prompt: "ocean"}, "account")
	if err != nil {
		t.Fatalf("video error = %v", err)
	}
	videos := result.Body.(map[string]any)["data"].([]map[string]any)
	if len(videos) != 1 || videos[0]["video_url"] != "https://video.example/clean.mp4" {
		t.Fatalf("videos = %#v", videos)
	}
	if pollBody["task_id"] != "task-9" || pollBody["event_id"] != float64(0) || pollHasFP {
		t.Fatalf("poll body = %#v fp=%v", pollBody, pollHasFP)
	}
}

func TestDoubaoVideoOverloadAndErrorEvents(t *testing.T) {
	reply := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(reply))
	}))
	defer server.Close()
	client := newTestClient(server.URL)
	reply = samanthaFrame(2001, map[string]any{"message": map[string]any{"content_type": 2001, "content": mustJSON(map[string]any{"text": "当前服务过载，请稍后重试"})}})
	_, err := client.CapabilityWithAccount(context.Background(), ports.CapabilityInput{Capability: "video", Prompt: "ocean"}, "account")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("overload error = %T/%v", err, err)
	}
	reply = samanthaFrame(2005, map[string]any{"message": "blocked"})
	_, err = client.CapabilityWithAccount(context.Background(), ports.CapabilityInput{Capability: "image", Prompt: "x"}, "account")
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadGateway || httpErr.Code != "2005" {
		t.Fatalf("2005 error = %T/%v", err, err)
	}
	reply = "event: gateway-error\ndata: {\"code\":4011}\n\n"
	_, err = client.CapabilityWithAccount(context.Background(), ports.CapabilityInput{Capability: "image", Prompt: "x"}, "account")
	if !errors.As(err, &httpErr) || httpErr.Kind() != "credential_rejected" {
		t.Fatalf("gateway error = %T/%v", err, err)
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
