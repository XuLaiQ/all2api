package grok

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

func TestCatalogFiltersByWebTier(t *testing.T) {
	if got := catalogModels("basic"); len(got) != 1 || got[0].UpstreamID != "grok-chat-fast" {
		t.Fatalf("basic models = %#v", got)
	}
	if got := catalogModels("super"); len(got) != 3 {
		t.Fatalf("super models = %#v", got)
	}
	if got := catalogModels("heavy"); len(got) != 4 {
		t.Fatalf("heavy models = %#v", got)
	}
}

func TestGatewayEndpointAndCookie(t *testing.T) {
	const userID = "497f19f8-49d4-458a-bee4-43ec3dcaf8ca"
	endpoint, origin, err := gatewayEndpoint("https://grok.com/ignored", userID)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "wss://grok.com/ws/mgw/?uid="+userID || origin != "https://grok.com" {
		t.Fatalf("endpoint=%q origin=%q", endpoint, origin)
	}
	cookie := ssoCookie("sso=token-one; ignored=value", "cf_clearance=abc; bad cookie=x")
	if cookie != "sso=token-one; sso-rw=token-one; cf_clearance=abc" {
		t.Fatalf("cookie=%q", cookie)
	}
}

func TestSessionUserIDPrefersStableSessionIdentity(t *testing.T) {
	payload := map[string]any{
		"id":      "organization-id",
		"session": map[string]any{"userId": "497f19f8-49d4-458a-bee4-43ec3dcaf8ca"},
		"user":    map[string]any{"id": "user-id"},
	}
	if got := sessionUserID(payload); got != "497f19f8-49d4-458a-bee4-43ec3dcaf8ca" {
		t.Fatalf("session user id = %q", got)
	}
}

func TestChatStreamUsesGrokGatewayProtocol(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		key, readErr := readHandshake(reader)
		if readErr != nil {
			serverDone <- readErr
			return
		}
		digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = io.WriteString(connection, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(digest[:])+"\r\n\r\n")

		initial, frameErr := readClientFrame(reader)
		if frameErr != nil {
			serverDone <- frameErr
			return
		}
		var initialPayload map[string]any
		if json.Unmarshal(initial, &initialPayload) != nil {
			serverDone <- io.ErrUnexpectedEOF
			return
		}
		event, _ := initialPayload["event"].(map[string]any)
		session, _ := event["session"].(map[string]any)
		if event["type"] != "session.create" || session["model"] != "fast" {
			serverDone <- io.ErrUnexpectedEOF
			return
		}
		if err := writeServerFrame(connection, []byte(`{"event":{"type":"session.created"},"session_id":"conversation-1"}`)); err != nil {
			serverDone <- err
			return
		}
		if err := writeServerFrame(connection, []byte(`{"event":{"type":"conversation.attached","conversation":{"id":"conversation-1"}}}`)); err != nil {
			serverDone <- err
			return
		}
		for range 2 {
			if _, err := readClientFrame(reader); err != nil {
				serverDone <- err
				return
			}
		}
		if err := writeServerFrame(connection, []byte(`{"event":{"type":"response.chunk","chunk":{"text":{"text":"hello","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`)); err != nil {
			serverDone <- err
			return
		}
		if err := writeServerFrame(connection, []byte(`{"event":{"type":"response.chunk","chunk":{"text":{"text":"thought","channel":"CHANNEL_ANALYSIS"}}}}`)); err != nil {
			serverDone <- err
			return
		}
		if err := writeServerFrame(connection, []byte(`{"event":{"type":"response.grok.output","output":{"modelResponse":{"message":"hello"}}}}`)); err != nil {
			serverDone <- err
			return
		}
		if err := writeServerFrame(connection, []byte(`{"event":{"type":"response.chunk","chunk":{"text":{"text":"hello","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`)); err != nil {
			serverDone <- err
			return
		}
		serverDone <- writeServerFrame(connection, []byte(`{"event":{"type":"response.done","response":{"id":"resp-test","status":"completed"}}}`))
	}()

	client := NewClient("http://"+listener.Addr().String(), func(context.Context, string) (map[string]string, error) {
		return map[string]string{"access_token": "token-one", "user_id": "497f19f8-49d4-458a-bee4-43ec3dcaf8ca"}, nil
	})
	client.ChatTimeout = 5 * time.Second
	result, err := client.Chat(context.Background(), ports.ChatInput{Model: "grok-chat-fast", Messages: []ports.ChatMessage{{Role: "user", Content: "hello"}}}, "sso:account")
	if err != nil {
		t.Fatal(err)
	}
	// Repeated tokens are real output; the modelResponse snapshot is ignored and
	// the analysis channel is surfaced as reasoning.
	if result.Text != "hellohello" || result.Reasoning != "thought" || result.FinishReason != "stop" || result.ID == "" {
		t.Fatalf("result = %#v", result)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestBuildOAuthUsesBuildModelsAndChatEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer fixture-access" || request.Header.Get("X-XAI-Token-Auth") == "" {
			t.Fatalf("Build auth headers were not applied")
		}
		switch request.URL.Path {
		case "/models":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"data":[{"id":"grok-4.5"}]}`))
		case "/responses":
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"build-1\",\"output\":[]}}\n\n" +
				"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"think\"}\n\n" +
				"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{ignored\"}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"build-\"}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"build-1\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2},\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"build-ok\"}]}]}}\n\n"))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := NewClientWithBuildBase("https://grok.example", server.URL, func(context.Context, string) (map[string]string, error) {
		return map[string]string{
			"access_token": "fixture-access", "auth_mode": "build", "provider": "grok_build",
			"user_id": "497f19f8-49d4-458a-bee4-43ec3dcaf8ca",
		}, nil
	})
	models, err := client.ModelsWithAccount(context.Background(), "build:account")
	if err != nil || len(models) != 1 || models[0].UpstreamID != "grok-4.5" {
		t.Fatalf("Build models=%#v err=%v", models, err)
	}
	result, err := client.Chat(context.Background(), ports.ChatInput{Model: "grok-4.5", Messages: []ports.ChatMessage{{Role: "user", Content: "hello"}}}, "build:account")
	if err != nil || result.Text != "build-ok" || result.Reasoning != "think" || result.FinishReason != "stop" || result.Usage == nil || result.Usage.CompletionTokens != 2 {
		t.Fatalf("Build result=%#v err=%v", result, err)
	}
}

func TestBuildCredentialRefreshesExpiredTokenAndPersistsRotation(t *testing.T) {
	refreshCalls := 0
	oauth := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = request.ParseForm()
		if request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") != "refresh-old" {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"error":"invalid_grant","error_description":"Invalid or unknown refresh token"}`))
			return
		}
		refreshCalls++
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"access_token":"access-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer oauth.Close()
	build := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer access-new" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = writer.Write([]byte(`{"data":[{"id":"grok-build"}]}`))
	}))
	defer build.Close()

	stored := map[string]string{
		"access_token": "access-old", "refresh_token": "refresh-old", "auth_mode": "build", "provider": "grok_build",
		"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
	}
	var mu sync.Mutex
	client := NewClientWithBuildBase("https://grok.example", build.URL, func(context.Context, string) (map[string]string, error) {
		mu.Lock()
		defer mu.Unlock()
		copied := map[string]string{}
		for key, value := range stored {
			copied[key] = value
		}
		return copied, nil
	})
	client.OAuth.TokenURL = oauth.URL
	client.PersistCredential = func(_ context.Context, _ string, credentials map[string]string) error {
		mu.Lock()
		defer mu.Unlock()
		stored = credentials
		return nil
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if models, err := client.ModelsWithAccount(context.Background(), "build:a"); err != nil || len(models) != 1 {
				t.Errorf("models=%#v err=%v", models, err)
			}
		}()
	}
	wg.Wait()
	if refreshCalls != 1 || stored["refresh_token"] != "refresh-new" || stored["access_token"] != "access-new" {
		t.Fatalf("refresh calls=%d stored refresh rotated=%v", refreshCalls, stored["refresh_token"] == "refresh-new")
	}

	// A revoked refresh token surfaces as credential_rejected.
	stored = map[string]string{"access_token": "access-old", "refresh_token": "revoked", "auth_mode": "build", "expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}
	_, err := client.ModelsWithAccount(context.Background(), "build:b")
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Kind() != "credential_rejected" || gatewayErr.Code != "refresh_token_invalid" {
		t.Fatalf("revoked refresh error = %T/%v", err, err)
	}
}

func readHandshake(reader *bufio.Reader) (string, error) {
	key := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-key:") {
			key = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
		}
		if line == "\r\n" {
			break
		}
	}
	if key == "" {
		return "", io.ErrUnexpectedEOF
	}
	return key, nil
}

func readClientFrame(reader *bufio.Reader) ([]byte, error) {
	first, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	second, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if first&0x0f != 1 || second&0x80 == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	length := int(second & 0x7f)
	if length == 126 {
		var extended uint16
		if err := binary.Read(reader, binary.BigEndian, &extended); err != nil {
			return nil, err
		}
		length = int(extended)
	}
	mask := make([]byte, 4)
	if _, err := io.ReadFull(reader, mask); err != nil {
		return nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	for index := range payload {
		payload[index] ^= mask[index%4]
	}
	return payload, nil
}

func writeServerFrame(writer io.Writer, payload []byte) error {
	if len(payload) >= 126 {
		return io.ErrShortBuffer
	}
	_, err := writer.Write(append([]byte{0x81, byte(len(payload))}, payload...))
	return err
}
