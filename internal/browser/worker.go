package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/XuLaiQ/all2api/internal/browser/cdp"
)

type Config struct {
	PlatformBase          string
	LoginPath             string
	ProfileRoot           string
	Executable            string
	Headless              bool
	MaxContexts           int
	OperationTimeout      time.Duration
	LaunchTimeout         time.Duration
	SessionTTL            time.Duration
	QRSelector            string
	QRCodeAttribute       string
	AuthenticatedSelector string
}

type Challenge struct {
	SessionID     string    `json:"session_id"`
	AccountID     string    `json:"account_id"`
	Status        string    `json:"status"`
	QRCode        string    `json:"qr_code,omitempty"`
	QRImageBase64 string    `json:"qr_image_base64,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type Event struct {
	Challenge
	Message       string `json:"message,omitempty"`
	URL           string `json:"url,omitempty"`
	Title         string `json:"title,omitempty"`
	Authenticated bool   `json:"authenticated"`
}

type Credentials struct {
	Cookie    string
	UserAgent string
	Origin    string
	Referer   string
}

type Session struct {
	ID          string
	AccountID   string
	ProfilePath string
	CreatedAt   time.Time
	Process     *exec.Cmd
	BrowserPort int
	Page        *cdp.WebSocket
	Mu          sync.Mutex
}

type Worker struct {
	Config    Config
	Mu        sync.Mutex
	Sessions  map[string]*Session
	Failures  int
	LastError string
}

func New(config Config) *Worker {
	if config.MaxContexts < 1 {
		config.MaxContexts = 4
	}
	if config.OperationTimeout <= 0 {
		config.OperationTimeout = 30 * time.Second
	}
	if config.LaunchTimeout <= 0 {
		config.LaunchTimeout = 30 * time.Second
	}
	if config.SessionTTL <= 0 {
		config.SessionTTL = 5 * time.Minute
	}
	if config.PlatformBase == "" {
		config.PlatformBase = "https://www.doubao.com"
	}
	if config.LoginPath == "" {
		config.LoginPath = "/"
	}
	if config.QRSelector == "" {
		config.QRSelector = `img[src*="qr"], canvas[data-qr]`
	}
	if config.QRCodeAttribute == "" {
		config.QRCodeAttribute = "data-code"
	}
	if config.AuthenticatedSelector == "" {
		config.AuthenticatedSelector = `[data-testid="user-avatar"], [data-authenticated="true"]`
	}
	return &Worker{Config: config, Sessions: map[string]*Session{}}
}

func (w *Worker) Health() map[string]any {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	status := "ready"
	if _, err := w.executablePath(); err != nil {
		status = "not_configured"
	}
	return map[string]any{"status": status, "service": "doubao-browser-worker", "runtime": "go", "browser": "chromium-cdp", "active_sessions": len(w.Sessions), "max_contexts": w.Config.MaxContexts, "failure_count": w.Failures, "last_error": w.LastError}
}

func (w *Worker) Start(ctx context.Context, accountID, profilePath string) (Challenge, error) {
	w.Mu.Lock()
	if len(w.Sessions) >= w.Config.MaxContexts {
		w.Mu.Unlock()
		return Challenge{}, errors.New("browser worker context limit reached")
	}
	w.Mu.Unlock()
	profile, err := w.profilePath(profilePath)
	if err != nil {
		return Challenge{}, err
	}
	executable, err := w.executablePath()
	if err != nil {
		return Challenge{}, err
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return Challenge{}, errors.New("browser profile could not be prepared")
	}
	port, err := freePort()
	if err != nil {
		return Challenge{}, errors.New("browser debug port could not be allocated")
	}
	args := []string{"--remote-debugging-address=127.0.0.1", fmt.Sprintf("--remote-debugging-port=%d", port), "--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check", "--disable-dev-shm-usage", "--disable-gpu"}
	args = append(args, "--no-sandbox", "--disable-setuid-sandbox")
	if w.Config.Headless {
		args = append(args, "--headless=new")
	}
	process := exec.Command(executable, args...)
	process.Stdout = io.Discard
	process.Stderr = io.Discard
	if err := process.Start(); err != nil {
		return Challenge{}, errors.New("browser process could not be started")
	}
	cleanup := func() { _ = process.Process.Kill(); _, _ = process.Process.Wait() }
	wsURL, err := waitDebugger(ctx, port, w.Config.LaunchTimeout)
	if err != nil {
		cleanup()
		return Challenge{}, err
	}
	page, err := cdp.Dial(ctx, wsURL)
	if err != nil {
		cleanup()
		return Challenge{}, err
	}
	sessionID := "db_" + randomID()
	session := &Session{ID: sessionID, AccountID: accountID, ProfilePath: profile, CreatedAt: time.Now(), Process: process, BrowserPort: port, Page: page}
	if _, err := page.Call(ctx, "Page.enable", map[string]any{}); err != nil {
		session.Close()
		return Challenge{}, err
	}
	if _, err := page.Call(ctx, "Network.enable", map[string]any{}); err != nil {
		session.Close()
		return Challenge{}, err
	}
	pageURL := strings.TrimRight(w.Config.PlatformBase, "/") + "/" + strings.TrimLeft(w.Config.LoginPath, "/")
	if _, err := page.Call(ctx, "Page.navigate", map[string]any{"url": pageURL}); err != nil {
		session.Close()
		return Challenge{}, err
	}
	w.Mu.Lock()
	if len(w.Sessions) >= w.Config.MaxContexts {
		w.Mu.Unlock()
		_ = session.Close()
		return Challenge{}, errors.New("browser worker context limit reached")
	}
	w.Sessions[sessionID] = session
	w.Mu.Unlock()
	event, err := w.inspect(ctx, session)
	if err != nil {
		_ = w.Cancel(sessionID)
		return Challenge{}, err
	}
	return event.Challenge, nil
}

func (w *Worker) Poll(ctx context.Context, sessionID string) (Event, error) {
	session, err := w.get(sessionID)
	if err != nil {
		return Event{}, err
	}
	if time.Since(session.CreatedAt) >= w.Config.SessionTTL {
		_ = w.Cancel(sessionID)
		return Event{Challenge: Challenge{SessionID: sessionID, AccountID: session.AccountID, Status: "expired"}}, nil
	}
	return w.inspect(ctx, session)
}

func (w *Worker) Complete(ctx context.Context, sessionID string) (Event, Credentials, error) {
	session, err := w.get(sessionID)
	if err != nil {
		return Event{}, Credentials{}, err
	}
	event, err := w.inspect(ctx, session)
	if err != nil {
		return Event{}, Credentials{}, err
	}
	if event.Status != "succeeded" {
		return event, Credentials{}, nil
	}
	credentials, err := w.credentials(ctx, session)
	if err != nil {
		return Event{}, Credentials{}, err
	}
	_ = w.Cancel(sessionID)
	return event, credentials, nil
}

func (w *Worker) Cancel(sessionID string) error {
	w.Mu.Lock()
	session := w.Sessions[sessionID]
	delete(w.Sessions, sessionID)
	w.Mu.Unlock()
	if session == nil {
		return nil
	}
	return session.Close()
}

func (s *Session) Close() error {
	if s.Page != nil {
		_ = s.Page.Close()
	}
	if s.Process != nil && s.Process.Process != nil {
		_ = s.Process.Process.Kill()
		_, _ = s.Process.Process.Wait()
	}
	return nil
}

func (w *Worker) inspect(ctx context.Context, session *Session) (Event, error) {
	value, err := w.evaluate(ctx, session, fmt.Sprintf(`(()=>{const q=document.querySelector(%q);const a=document.querySelector(%q);return {qr:q?(q.getAttribute(%q)||""):"",authenticated:!!a,url:location.href,title:document.title};})()`, w.Config.QRSelector, w.Config.AuthenticatedSelector, w.Config.QRCodeAttribute))
	if err != nil {
		return Event{}, err
	}
	var state struct {
		QR            string `json:"qr"`
		Authenticated bool   `json:"authenticated"`
		URL           string `json:"url"`
		Title         string `json:"title"`
	}
	if json.Unmarshal(value, &state) != nil {
		return Event{}, errors.New("browser page state is invalid")
	}
	status := "waiting_scan"
	if state.Authenticated || w.cookiePresent(ctx, session) {
		status = "succeeded"
	} else if state.QR == "" {
		status = "scanned"
	}
	challenge := Challenge{SessionID: session.ID, AccountID: session.AccountID, Status: status, QRCode: state.QR, ExpiresAt: session.CreatedAt.Add(w.Config.SessionTTL)}
	if challenge.QRCode == "" && status == "waiting_scan" {
		if image, screenshotErr := w.screenshot(ctx, session); screenshotErr == nil {
			challenge.QRImageBase64 = image
		}
	}
	return Event{Challenge: challenge, URL: state.URL, Title: state.Title, Authenticated: state.Authenticated}, nil
}

func (w *Worker) credentials(ctx context.Context, session *Session) (Credentials, error) {
	result, err := session.Page.Call(ctx, "Network.getAllCookies", map[string]any{})
	if err != nil {
		return Credentials{}, err
	}
	var payload struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if json.Unmarshal(result, &payload) != nil {
		return Credentials{}, errors.New("browser cookies are invalid")
	}
	parts := make([]string, 0, len(payload.Cookies))
	for _, cookie := range payload.Cookies {
		if cookie.Name != "" {
			parts = append(parts, cookie.Name+"="+cookie.Value)
		}
	}
	if len(parts) == 0 {
		return Credentials{}, errors.New("browser session has no cookies")
	}
	return Credentials{Cookie: strings.Join(parts, "; "), UserAgent: "Mozilla/5.0", Origin: w.Config.PlatformBase, Referer: strings.TrimRight(w.Config.PlatformBase, "/") + "/"}, nil
}

func (w *Worker) cookiePresent(ctx context.Context, session *Session) bool {
	result, err := session.Page.Call(ctx, "Network.getAllCookies", map[string]any{})
	if err != nil {
		return false
	}
	var payload struct {
		Cookies []struct {
			Name string `json:"name"`
		} `json:"cookies"`
	}
	if json.Unmarshal(result, &payload) != nil {
		return false
	}
	for _, cookie := range payload.Cookies {
		switch strings.ToLower(cookie.Name) {
		case "sessionid", "sessionid_ss", "sessionid_secure":
			return true
		}
	}
	return false
}

func (w *Worker) screenshot(ctx context.Context, session *Session) (string, error) {
	result, err := session.Page.Call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return "", err
	}
	var payload struct {
		Data string `json:"data"`
	}
	if json.Unmarshal(result, &payload) != nil || payload.Data == "" {
		return "", errors.New("browser screenshot is unavailable")
	}
	return payload.Data, nil
}

func (w *Worker) evaluate(ctx context.Context, session *Session, expression string) (json.RawMessage, error) {
	result, err := session.Page.Call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true})
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	if json.Unmarshal(result, &envelope) != nil || len(envelope.Result.Value) == 0 {
		return nil, errors.New("browser script result is invalid")
	}
	return envelope.Result.Value, nil
}

func (w *Worker) get(id string) (*Session, error) {
	w.Mu.Lock()
	defer w.Mu.Unlock()
	session := w.Sessions[id]
	if session == nil {
		return nil, errors.New("browser session not found")
	}
	return session, nil
}

func (w *Worker) profilePath(value string) (string, error) {
	root, err := filepath.Abs(w.Config.ProfileRoot)
	if err != nil {
		return "", errors.New("browser profile root is invalid")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", errors.New("browser profile root is unavailable")
	}
	if strings.TrimSpace(value) == "" {
		value = filepath.Join(root, "session-"+randomID())
	}
	path, err := filepath.Abs(value)
	if err != nil {
		return "", errors.New("browser profile path is invalid")
	}
	prefix := strings.TrimRight(root, string(os.PathSeparator)) + string(os.PathSeparator)
	if path != root && !strings.HasPrefix(path, prefix) {
		return "", errors.New("browser profile path escapes configured root")
	}
	return path, nil
}

func (w *Worker) executablePath() (string, error) {
	if w.Config.Executable != "" {
		if _, err := os.Stat(w.Config.Executable); err == nil {
			return w.Config.Executable, nil
		}
		return "", errors.New("configured browser executable is unavailable")
	}
	for _, candidate := range []string{"chromium", "chromium-browser", "google-chrome", "msedge"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("Chromium executable is not configured")
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func waitDebugger(ctx context.Context, port int, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	listURL := fmt.Sprintf("http://127.0.0.1:%d/json/list", port)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			var payload []struct {
				Type                 string `json:"type"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}
			_ = json.NewDecoder(response.Body).Decode(&payload)
			response.Body.Close()
			for _, target := range payload {
				if target.Type == "page" && target.WebSocketDebuggerURL != "" {
					return target.WebSocketDebuggerURL, nil
				}
			}
			newTargetURL := fmt.Sprintf("http://127.0.0.1:%d/json/new?about:blank", port)
			newRequest, _ := http.NewRequestWithContext(ctx, http.MethodPut, newTargetURL, nil)
			if newResponse, newErr := http.DefaultClient.Do(newRequest); newErr == nil {
				var target struct {
					WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
				}
				_ = json.NewDecoder(newResponse.Body).Decode(&target)
				newResponse.Body.Close()
				if target.WebSocketDebuggerURL != "" {
					return target.WebSocketDebuggerURL, nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", errors.New("browser debugger did not become ready")
}

func randomID() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }

func NewProfilePath(root, accountID string) string { return filepath.Join(root, accountID, "browser") }

func DecodeDataURL(value string) (string, []byte, bool) {
	if !strings.HasPrefix(value, "data:image/") {
		return "", nil, false
	}
	parts := strings.SplitN(value, ",", 2)
	if len(parts) != 2 || !strings.Contains(parts[0], ";base64") {
		return "", nil, false
	}
	mime := strings.TrimPrefix(strings.SplitN(parts[0], ";", 2)[0], "data:")
	data, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", nil, false
	}
	return mime, data, len(data) > 0
}
