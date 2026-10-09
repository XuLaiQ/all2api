package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestOAuthClientBuildsPKCEAndExchangesTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/accounts/oauth/token" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		grantType, code, verifier, refreshToken, clientID := "", "", "", "", ""
		if request.Header.Get("Content-Type") == "application/json" {
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			grantType, _ = payload["grant_type"].(string)
			code, _ = payload["code"].(string)
			verifier, _ = payload["code_verifier"].(string)
			refreshToken, _ = payload["refresh_token"].(string)
			clientID, _ = payload["client_id"].(string)
		} else if err := request.ParseForm(); err == nil {
			grantType, code, verifier, refreshToken, clientID = request.Form.Get("grant_type"), request.Form.Get("code"), request.Form.Get("code_verifier"), request.Form.Get("refresh_token"), request.Form.Get("client_id")
		} else {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if (grantType == "authorization_code" && (code != "code-1" || verifier == "")) || (grantType == "refresh_token" && refreshToken == "") || clientID != chatGPTOAuthClientID {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"access_token":"access-1","refresh_token":"refresh-1","id_token":"id-1"}`))
	}))
	defer server.Close()

	client := NewOAuthClient(server.URL, "")
	started, err := client.Begin("person@example.test")
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	parsed, err := url.Parse(started.AuthorizeURL)
	if err != nil || parsed.Path != "/api/accounts/authorize" || parsed.Query().Get("state") != started.State || parsed.Query().Get("code_challenge") != started.Challenge || parsed.Query().Get("login_hint") != "person@example.test" || parsed.Query().Get("audience") != chatGPTOAuthAudience || parsed.Query().Get("redirect_uri") != chatGPTOAuthRedirect || parsed.Query().Get("device_id") != started.DeviceID || parsed.Query().Get("auth0Client") != chatGPTAuth0Client {
		t.Fatalf("PKCE authorize URL = %v", started.AuthorizeURL)
	}
	credentials, err := client.Exchange(context.Background(), "code-1", started.Verifier)
	if err != nil || credentials["access_token"] != "access-1" || credentials["refresh_token"] != "refresh-1" || credentials["auth_mode"] != "web" || credentials["oai_device_id"] == "" || credentials["oai_session_id"] == "" {
		t.Fatalf("Exchange() = %#v/%v", credentials, err)
	}
	if strings.Contains(started.AuthorizeURL, "access-1") {
		t.Fatal("authorize URL contains access token")
	}
	refreshed, err := client.Refresh(context.Background(), "refresh-1", client.ClientID)
	if err != nil || refreshed["access_token"] != "access-1" || refreshed["refresh_token"] != "refresh-1" || refreshed["auth_mode"] != "web" {
		t.Fatalf("Refresh() = %#v/%v", refreshed, err)
	}
}

func TestParseCallbackRejectsOAuthError(t *testing.T) {
	if _, err := ParseCallback("http://127.0.0.1:1455/auth/callback?error=access_denied"); err == nil {
		t.Fatal("ParseCallback() accepted an OAuth error")
	}
}

func TestChatGPTAccountIDReadsWebIDTokenClaim(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct_web"}}`))
	if got := chatGPTAccountID("header." + payload + ".signature"); got != "acct_web" {
		t.Fatalf("chatGPTAccountID() = %q", got)
	}
}
