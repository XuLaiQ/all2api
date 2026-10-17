package chatgpt

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func fakeJWT(exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return "eyJhbGciOiJub25lIn0." + payload + ".sig"
}

func TestCredentialsForRefreshesExpiringOAuthToken(t *testing.T) {
	fresh := fakeJWT(time.Now().Add(10 * 24 * time.Hour))
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-1" || r.Form.Get("client_id") != "app_x" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		calls++
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"refresh-2","id_token":"id-2"}`, fresh)
	}))
	defer server.Close()
	stored := map[string]string{"access_token": fakeJWT(time.Now().Add(time.Minute)), "refresh_token": "refresh-1", "client_id": "app_x"}
	client := NewClient("http://chatgpt.invalid", func(context.Context, string) (map[string]string, error) {
		copied := map[string]string{}
		for key, value := range stored {
			copied[key] = value
		}
		return copied, nil
	})
	oauth := NewOAuthClient(server.URL, "")
	oauth.TokenEndpoint = server.URL
	client.EnableTokenRefresh(oauth, func(_ context.Context, _ string, credentials map[string]string) error {
		stored = credentials
		return nil
	})
	credentials, err := client.credentialsFor(context.Background(), "oauth:a")
	if err != nil || credentials["access_token"] != fresh || stored["refresh_token"] != "refresh-2" || stored["expires_at"] == "" || calls != 1 {
		t.Fatalf("credentials refreshed=%v calls=%d err=%v", credentials["access_token"] == fresh, calls, err)
	}
	// A still-valid token is used without another refresh.
	if _, err := client.credentialsFor(context.Background(), "oauth:a"); err != nil || calls != 1 {
		t.Fatalf("second call refreshed again: calls=%d err=%v", calls, err)
	}
}
