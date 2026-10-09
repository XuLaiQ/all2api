package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/browser"
)

type BrowserClient struct {
	BaseURL     string
	Token       string
	ProfileRoot string
	HTTPClient  *http.Client
}

func NewBrowserClient(baseURL, token, profileRoot string) *BrowserClient {
	return &BrowserClient{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, ProfileRoot: profileRoot, HTTPClient: &http.Client{Timeout: 60 * time.Second}}
}

func (c *BrowserClient) Start(ctx context.Context, accountID string) (QRStart, error) {
	if strings.TrimSpace(c.BaseURL) == "" || strings.TrimSpace(c.Token) == "" {
		return QRStart{}, errors.New("Doubao browser worker is not configured")
	}
	profile := browser.NewProfilePath(c.ProfileRoot, accountID)
	var response struct {
		SessionID     string `json:"session_id"`
		AccountID     string `json:"account_id"`
		Status        string `json:"status"`
		QRCode        string `json:"qr_code"`
		QRImageBase64 string `json:"qr_image_base64"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/sessions", map[string]any{"account_id": accountID, "profile_path": profile}, &response); err != nil {
		return QRStart{}, err
	}
	if response.SessionID == "" {
		return QRStart{}, errors.New("browser worker returned no session")
	}
	return QRStart{SessionID: response.SessionID, QRCode: response.QRCode, QRImageBase64: response.QRImageBase64}, nil
}

func (c *BrowserClient) Poll(ctx context.Context, state map[string]any) (QRPoll, error) {
	sessionID, _ := state["worker_session_id"].(string)
	if sessionID == "" {
		return QRPoll{}, errors.New("browser worker session is missing")
	}
	var event struct {
		Status        string `json:"status"`
		QRCode        string `json:"qr_code"`
		QRImageBase64 string `json:"qr_image_base64"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/sessions/"+urlPath(sessionID), nil, &event); err != nil {
		return QRPoll{}, err
	}
	result := QRPoll{Status: event.Status, QRCode: event.QRCode, QRImageBase64: event.QRImageBase64}
	if event.Status == "succeeded" {
		var completed struct {
			Credentials struct {
				Cookie    string `json:"Cookie"`
				UserAgent string `json:"UserAgent"`
				Origin    string `json:"Origin"`
				Referer   string `json:"Referer"`
			} `json:"credentials"`
		}
		if err := c.call(ctx, http.MethodPost, "/v1/sessions/"+urlPath(sessionID), nil, &completed); err != nil {
			return QRPoll{}, err
		}
		result.Credentials = map[string]string{"Cookie": completed.Credentials.Cookie, "User-Agent": completed.Credentials.UserAgent, "Origin": completed.Credentials.Origin, "Referer": completed.Credentials.Referer}
		if result.Credentials["Cookie"] == "" {
			return QRPoll{}, errors.New("browser worker returned no cookies")
		}
	}
	return result, nil
}

func (c *BrowserClient) Cancel(ctx context.Context, state map[string]any) error {
	sessionID, _ := state["worker_session_id"].(string)
	if sessionID == "" {
		return nil
	}
	return c.call(ctx, http.MethodDelete, "/v1/sessions/"+urlPath(sessionID), nil, nil)
}

func (c *BrowserClient) call(ctx context.Context, method, endpoint string, input any, output any) error {
	var body *strings.Reader
	if input == nil {
		body = strings.NewReader("")
	} else {
		encoded, err := json.Marshal(input)
		if err != nil {
			return errors.New("browser worker request is invalid")
		}
		body = strings.NewReader(string(encoded))
	}
	request, err := http.NewRequestWithContext(ctx, method, c.BaseURL+endpoint, body)
	if err != nil {
		return errors.New("browser worker request could not be created")
	}
	request.Header.Set("X-Worker-Token", c.Token)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return errors.New("browser worker is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("browser worker returned HTTP %d", response.StatusCode)
	}
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return errors.New("browser worker response is invalid")
	}
	return nil
}

func urlPath(value string) string {
	return strings.NewReplacer("/", "%2F", "?", "%3F", "#", "%23").Replace(value)
}
