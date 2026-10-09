package doubao

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const doubaoAID = "497858"

type QRClient struct {
	BaseURL    string
	HTTPClient *http.Client
	UserAgent  string
}
type QRStart struct {
	CSRFToken     string
	Token         string
	QRCode        string
	QRImageBase64 string
	Cookies       map[string]string
	SessionID     string
}

type ProvisionClient interface {
	Start(context.Context, string) (QRStart, error)
	Poll(context.Context, map[string]any) (QRPoll, error)
}
type QRPoll struct {
	Status        string
	QRCode        string
	QRImageBase64 string
	Credentials   map[string]string
}

func NewQRClient(baseURL string) *QRClient {
	return &QRClient{BaseURL: strings.TrimRight(baseURL, "/"), HTTPClient: &http.Client{Timeout: 30 * time.Second}, UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/148.0.0.0 Safari/537.36"}
}

func (c *QRClient) Start(ctx context.Context, accountID string) (QRStart, error) {
	cookies := map[string]string{}
	if _, _, err := c.request(ctx, http.MethodGet, "/", cookies, "", nil, true); err != nil {
		return QRStart{}, err
	}
	csrf := cookies["passport_csrf_token"]
	if csrf == "" {
		body, _, err := c.request(ctx, http.MethodGet, "/passport/safe/csrf_token/", cookies, "", map[string]string{"aid": doubaoAID}, false)
		if err != nil {
			return QRStart{}, err
		}
		var payload map[string]any
		if json.Unmarshal(body, &payload) == nil {
			if data, ok := payload["data"].(map[string]any); ok {
				csrf, _ = data["passport_csrf_token"].(string)
			}
		}
	}
	body, status, err := c.request(ctx, http.MethodGet, "/passport/web/get_qrcode/", cookies, csrf, map[string]string{"next": c.BaseURL, "aid": doubaoAID}, false)
	if err != nil {
		return QRStart{}, err
	}
	if status < 200 || status >= 300 {
		return QRStart{}, fmt.Errorf("Doubao QR endpoint returned %d", status)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return QRStart{}, err
	}
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		return QRStart{}, fmt.Errorf("Doubao QR response is invalid")
	}
	code := intValue(data["error_code"])
	if code != 0 {
		return QRStart{}, fmt.Errorf("Doubao QR generation failed")
	}
	token, _ := data["token"].(string)
	raw := stringValue(data["qrcode"])
	qr, image := decodeQR(raw)
	if token == "" {
		return QRStart{}, fmt.Errorf("Doubao QR response has no token")
	}
	return QRStart{CSRFToken: csrf, Token: token, QRCode: qr, QRImageBase64: image, Cookies: cookies}, nil
}

func (c *QRClient) Poll(ctx context.Context, state map[string]any) (QRPoll, error) {
	cookies := stringMap(state["cookies"])
	csrf, _ := state["csrf_token"].(string)
	token, _ := state["qr_token"].(string)
	body, status, err := c.request(ctx, http.MethodGet, "/passport/web/check_qrconnect/", cookies, csrf, map[string]string{"next": c.BaseURL, "token": token, "aid": doubaoAID}, false)
	if err != nil {
		return QRPoll{}, err
	}
	if status < 200 || status >= 300 {
		return QRPoll{}, fmt.Errorf("Doubao QR poll returned %d", status)
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return QRPoll{}, fmt.Errorf("Doubao QR poll response is invalid")
	}
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		return QRPoll{Status: "waiting_scan"}, nil
	}
	code := intValue(data["error_code"])
	if code != 0 {
		description := strings.ToLower(stringValue(data["description"]))
		if strings.Contains(description, "expired") || strings.Contains(description, "过期") {
			return QRPoll{Status: "expired"}, nil
		}
		return QRPoll{Status: "waiting_scan"}, nil
	}
	statusName := strings.ToLower(stringValue(data["status"]))
	if statusName == "new" || statusName == "" {
		return QRPoll{Status: "waiting_scan"}, nil
	}
	if statusName == "scanned" {
		return QRPoll{Status: "scanned"}, nil
	}
	if statusName == "expired" {
		return QRPoll{Status: "expired"}, nil
	}
	if statusName != "confirmed" {
		return QRPoll{Status: "waiting_scan"}, nil
	}
	if redirect := stringValue(data["redirect_url"]); redirect != "" {
		if _, _, err := c.request(ctx, http.MethodGet, redirect, cookies, csrf, nil, true); err != nil {
			return QRPoll{}, err
		}
	}
	credential := map[string]string{"Cookie": cookieHeader(cookies), "User-Agent": c.UserAgent, "Origin": c.BaseURL, "Referer": c.BaseURL + "/chat/login"}
	if cookies["msToken"] != "" {
		credential["msToken"] = cookies["msToken"]
	}
	for _, key := range []string{"device_id", "web_id", "fp"} {
		if value, ok := state[key].(string); ok {
			credential[key] = value
		}
	}
	return QRPoll{Status: "succeeded", Credentials: credential}, nil
}

func (c *QRClient) request(ctx context.Context, method, path string, cookies map[string]string, csrf string, params map[string]string, follow bool) ([]byte, int, error) {
	current := path
	for i := 0; i < 10; i++ {
		target := current
		if !strings.HasPrefix(target, "http") {
			target = c.BaseURL + target
		}
		parsed, err := url.Parse(target)
		if err != nil {
			return nil, 0, err
		}
		query := parsed.Query()
		for key, value := range params {
			query.Set(key, value)
		}
		parsed.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, method, parsed.String(), nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("User-Agent", c.UserAgent)
		req.Header.Set("Accept", "application/json,text/plain,*/*")
		req.Header.Set("Referer", c.BaseURL+"/chat/login")
		req.Header.Set("Origin", c.BaseURL)
		if csrf != "" {
			req.Header.Set("x-tt-passport-csrf-token", csrf)
		}
		if header := cookieHeader(cookies); header != "" {
			req.Header.Set("Cookie", header)
		}
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			return nil, 0, err
		}
		for _, cookie := range resp.Cookies() {
			cookies[cookie.Name] = cookie.Value
		}
		if follow && resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location := resp.Header.Get("Location")
			resp.Body.Close()
			if location == "" {
				return nil, resp.StatusCode, fmt.Errorf("Doubao redirect has no location")
			}
			current = location
			params = nil
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		return data, resp.StatusCode, readErr
	}
	return nil, 0, fmt.Errorf("Doubao redirect loop")
}

func decodeQR(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	if strings.HasPrefix(value, "data:") && strings.Contains(value, ",") {
		value = strings.SplitN(value, ",", 2)[1]
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err == nil && len(decoded) > 0 {
		return "", base64.StdEncoding.EncodeToString(decoded)
	}
	return value, ""
}
func cookieHeader(cookies map[string]string) string {
	parts := make([]string, 0, len(cookies))
	for key, value := range cookies {
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, "; ")
}
func stringMap(value any) map[string]string {
	result := map[string]string{}
	if values, ok := value.(map[string]any); ok {
		for key, value := range values {
			if text, ok := value.(string); ok {
				result[key] = text
			}
		}
	}
	return result
}
func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}
func intValue(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	}
	return 0
}
