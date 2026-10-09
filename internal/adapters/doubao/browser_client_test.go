package doubao

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBrowserClientUsesProtectedWorkerSessionProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Worker-Token") != "worker-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/sessions":
			_, _ = writer.Write([]byte(`{"session_id":"db_1","account_id":"account-1","status":"waiting_scan","qr_code":"qr-code"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/sessions/db_1":
			_, _ = writer.Write([]byte(`{"session_id":"db_1","account_id":"account-1","status":"succeeded"}`))
		case request.Method == http.MethodPost && request.URL.Path == "/v1/sessions/db_1":
			_, _ = writer.Write([]byte(`{"event":{"status":"succeeded"},"credentials":{"Cookie":"sessionid=fixture","UserAgent":"Mozilla/5.0","Origin":"https://www.doubao.com","Referer":"https://www.doubao.com/"}}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewBrowserClient(server.URL, "worker-token", t.TempDir())
	started, err := client.Start(context.Background(), "account-1")
	if err != nil || started.SessionID != "db_1" || started.QRCode != "qr-code" {
		t.Fatalf("Start() = %#v/%v", started, err)
	}
	polled, err := client.Poll(context.Background(), map[string]any{"worker_session_id": started.SessionID})
	if err != nil || polled.Status != "succeeded" || polled.Credentials["Cookie"] != "sessionid=fixture" {
		t.Fatalf("Poll() = %#v/%v", polled, err)
	}
}
