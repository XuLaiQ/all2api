package doubao

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQRClientMirrorsDoubaoHTTPLoginShape(t *testing.T) {
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Add("Set-Cookie", "ttwid=fixture; Path=/")
			w.WriteHeader(200)
			return
		}
		if r.URL.Path == "/passport/safe/csrf_token/" {
			_, _ = w.Write([]byte(`{"data":{"passport_csrf_token":"csrf"}}`))
			return
		}
		if r.URL.Path == "/passport/web/get_qrcode/" {
			_, _ = w.Write([]byte(`{"data":{"error_code":0,"token":"qr-token","qrcode":"aW1hZ2U="}}`))
			return
		}
		if r.URL.Path == "/passport/web/check_qrconnect/" {
			polls++
			status := "new"
			if polls == 2 {
				status = "scanned"
			}
			if polls >= 3 {
				status = "confirmed"
			}
			payload := `{"data":{"error_code":0,"status":"` + status + `"}}`
			if status == "confirmed" {
				payload = `{"data":{"error_code":0,"status":"confirmed","redirect_url":"/redirect"}}`
			}
			_, _ = w.Write([]byte(payload))
			return
		}
		if r.URL.Path == "/redirect" {
			w.Header().Add("Set-Cookie", "sessionid=session; Path=/")
			w.Header().Add("Location", "/final")
			w.WriteHeader(http.StatusFound)
			return
		}
		if r.URL.Path == "/final" {
			w.Header().Add("Set-Cookie", "msToken=token; Path=/")
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client := NewQRClient(server.URL)
	started, err := client.Start(context.Background(), "account")
	if err != nil || started.Token != "qr-token" || started.QRImageBase64 != "aW1hZ2U=" {
		t.Fatalf("Start() = %#v, %v", started, err)
	}
	state := map[string]any{"csrf_token": started.CSRFToken, "qr_token": started.Token, "cookies": started.Cookies}
	for _, expected := range []string{"waiting_scan", "scanned", "succeeded"} {
		result, pollErr := client.Poll(context.Background(), state)
		if pollErr != nil || result.Status != expected {
			t.Fatalf("Poll(%s) = %#v, %v", expected, result, pollErr)
		}
		if result.Status == "succeeded" && result.Credentials["Cookie"] == "" {
			t.Fatal("successful QR poll did not collect Cookie")
		}
	}
}
