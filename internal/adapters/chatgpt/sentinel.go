package chatgpt

import (
	"context"
	"crypto/rand"
	"crypto/sha3"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
	"github.com/google/uuid"
)

var buildMarker = regexp.MustCompile(`(?:data-build|c/)(?:=["']|/)?([^"'/_> ]+)`)

type SentinelError struct{ Reason string }

func (e *SentinelError) Error() string   { return "ChatGPT sentinel requirements are unavailable" }
func (e *SentinelError) StatusCode() int { return http.StatusBadGateway }
func (e *SentinelError) Kind() string    { return "upstream_protocol_error" }

type SentinelRequirements struct {
	Token     string
	Proof     string
	Turnstile string
	SOToken   string
}

func (c *Client) Requirements(ctx context.Context, credentials map[string]string, accessToken string) (SentinelRequirements, error) {
	userAgent := credentialString(credentials, "user_agent", "User-Agent", "user-agent")
	if userAgent == "" {
		userAgent = c.UserAgent
	}
	if userAgent == "" {
		userAgent = chatGPTDefaultUserAgent
	}
	scripts, build, err := c.bootstrap(ctx, accessToken, userAgent)
	if err != nil {
		return SentinelRequirements{}, err
	}
	pToken := strings.TrimSpace(credentials["sentinel_p"])
	if pToken == "" {
		pToken = requirementsToken(scripts, build, userAgent, time.Now())
	}
	prepared, err := c.sentinelJSON(ctx, http.MethodPost, "/backend-api/sentinel/chat-requirements/prepare", accessToken, userAgent, map[string]any{"p": pToken})
	if err != nil {
		return SentinelRequirements{}, err
	}
	if required, _ := prepared["arkose"].(map[string]any); boolValue(required["required"]) {
		return SentinelRequirements{}, &SentinelError{Reason: "arkose"}
	}
	turnstile := ""
	if required, _ := prepared["turnstile"].(map[string]any); boolValue(required["required"]) {
		dx := sentinelString(required["dx"])
		if dx != "" {
			turnstile, err = solveTurnstileToken(dx, pToken)
			if err != nil {
				return SentinelRequirements{}, &SentinelError{Reason: "turnstile_decode"}
			}
		}
	}
	proof := ""
	if info, _ := prepared["proofofwork"].(map[string]any); boolValue(info["required"]) {
		var solveErr error
		proof, solveErr = proofToken(sentinelString(info["seed"]), sentinelString(info["difficulty"]), userAgent, scripts, build)
		if solveErr != nil {
			return SentinelRequirements{}, solveErr
		}
	}
	finalized, err := c.sentinelJSON(ctx, http.MethodPost, "/backend-api/sentinel/chat-requirements/finalize", accessToken, userAgent, map[string]any{
		"prepare_token": sentinelString(prepared["prepare_token"]), "proof_token": proof, "turnstile_token": turnstile,
	})
	if err != nil {
		return SentinelRequirements{}, err
	}
	token := sentinelString(finalized["token"])
	if token == "" {
		return SentinelRequirements{}, &SentinelError{Reason: "missing_token"}
	}
	return SentinelRequirements{Token: token, Proof: proof, Turnstile: turnstile, SOToken: sentinelString(finalized["so_token"])}, nil
}

func (c *Client) bootstrap(ctx context.Context, token, userAgent string) ([]string, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/", nil)
	if err != nil {
		return nil, "", &SentinelError{Reason: "request"}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "text/html")
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Origin", c.BaseURL)
	request.Header.Set("Referer", c.BaseURL+"/")
	c.setWebHeaders(request, "/")
	request.Header.Del("X-OpenAI-Target-Path")
	request.Header.Del("X-OpenAI-Target-Route")
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	request.Header.Set("Sec-Fetch-Dest", "document")
	request.Header.Set("Sec-Fetch-Mode", "navigate")
	request.Header.Set("Sec-Fetch-Site", "none")
	request.Header.Set("Sec-Fetch-User", "?1")
	request.Header.Set("Upgrade-Insecure-Requests", "1")
	request.Header.Set("User-Agent", userAgent)
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return nil, "", portsTransportError(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode >= 400 {
			return nil, "", &HTTPError{Status: response.StatusCode, Operation: "bootstrap"}
		}
		return nil, "", &SentinelError{Reason: "bootstrap"}
	}
	text := string(body)
	sources := make([]string, 0)
	for _, match := range regexp.MustCompile(`<script[^>]+src=["']([^"']+)["']`).FindAllStringSubmatch(text, -1) {
		if len(match) > 1 {
			sources = append(sources, match[1])
		}
	}
	build := ""
	if match := regexp.MustCompile(`c/[^/]*/_`).FindString(text); match != "" {
		build = match
	} else if match := buildMarker.FindStringSubmatch(text); len(match) > 1 {
		build = match[1]
	}
	return sources, build, nil
}

func (c *Client) sentinelJSON(ctx context.Context, method, path, token, userAgent string, payload map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, &SentinelError{Reason: "encode"}
	}
	request, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, strings.NewReader(string(encoded)))
	if err != nil {
		return nil, &SentinelError{Reason: "request"}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Origin", c.BaseURL)
	request.Header.Set("Referer", c.BaseURL+"/")
	c.setWebHeaders(request, path)
	request.Header.Set("User-Agent", userAgent)
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return nil, portsTransportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &HTTPError{Status: response.StatusCode, Operation: "sentinel"}
	}
	var value map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&value); err != nil {
		return nil, &SentinelError{Reason: "decode"}
	}
	return value, nil
}

func requirementsToken(sources []string, build, userAgent string, now time.Time) string {
	config := buildRequirementsConfig(sources, build, userAgent, now)
	encoded, _ := json.Marshal(config)
	return "gAAAAAC" + base64.StdEncoding.EncodeToString(encoded)
}

func buildRequirementsConfig(sources []string, build, userAgent string, now time.Time) []any {
	if len(sources) == 0 {
		sources = []string{"https://chatgpt.com/backend-api/sentinel/sdk.js"}
	}
	resolutions := [][2]int{{1920, 1080}, {1440, 900}, {2560, 1440}, {3840, 2160}}
	resolution := resolutions[randomIndex(len(resolutions))]
	eastern := now.In(time.FixedZone("EST", -5*60*60))
	dateText := eastern.Format("Mon Jan 02 2006 15:04:05") + " GMT-0500 (Eastern Standard Time)"
	navigatorKeys := []string{
		"registerProtocolHandler−function registerProtocolHandler() { [native code] }",
		"storage−[object StorageManager]", "locks−[object LockManager]", "appCodeName−Mozilla",
		"permissions−[object Permissions]", "share−function share() { [native code] }", "webdriver−false",
		"managed−[object NavigatorManagedData]", "canShare−function canShare() { [native code] }",
		"vendor−Google Inc.", "mediaDevices−[object MediaDevices]", "vibrate−function vibrate() { [native code] }",
		"storageBuckets−[object StorageBucketManager]", "mediaCapabilities−[object MediaCapabilities]",
		"cookieEnabled−true", "virtualKeyboard−[object VirtualKeyboard]", "product−Gecko",
		"presentation−[object Presentation]", "onLine−true", "mimeTypes−[object MimeTypeArray]",
		"credentials−[object CredentialsContainer]", "serviceWorker−[object ServiceWorkerContainer]",
		"keyboard−[object Keyboard]", "gpu−[object GPU]", "doNotTrack", "serial−[object Serial]",
		"pdfViewerEnabled−true", "language−zh-CN", "geolocation−[object Geolocation]",
		"userAgentData−[object NavigatorUAData]", "getUserMedia−function getUserMedia() { [native code] }",
		"sendBeacon−function sendBeacon() { [native code] }", "hardwareConcurrency−32",
		"windowControlsOverlay−[object WindowControlsOverlay]",
	}
	documentKeys := []string{"__reactContainer$fzelfjyxej8", "_reactListening5dehydibo78", "location"}
	windowKeys := []string{"0", "window", "self", "document", "name", "location", "customElements", "history", "navigation", "innerWidth", "innerHeight", "scrollX", "scrollY", "visualViewport", "screenX", "screenY", "outerWidth", "outerHeight", "devicePixelRatio", "screen", "chrome", "navigator", "onresize", "performance", "crypto", "indexedDB", "sessionStorage", "localStorage", "scheduler", "alert", "atob", "btoa", "fetch", "matchMedia", "postMessage", "queueMicrotask", "requestAnimationFrame", "setInterval", "setTimeout", "caches", "__NEXT_DATA__", "__BUILD_MANIFEST", "__NEXT_PRELOADREADY"}
	performanceNow := sentinelPerformanceMilliseconds()
	wallNow := float64(now.UnixNano()) / float64(time.Millisecond)
	scriptSource := sources[randomIndex(len(sources))]
	return []any{
		resolution[0] + resolution[1], dateText, 4294705152, 1, userAgent, scriptSource, build,
		"en-US", "en-US,es-US,en,es", randomFraction(),
		chooseString(navigatorKeys), chooseString(documentKeys), chooseString(windowKeys),
		performanceNow, uuid.NewString(), "", []int{8, 16, 24, 32}[randomIndex(4)],
		wallNow - performanceNow, 0, 0, 0, 0, 0, 0, 0,
	}
}

func randomFraction() float64 {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 0.5
	}
	var value uint64
	for _, item := range raw {
		value = (value << 8) | uint64(item)
	}
	return float64(value) / float64(^uint64(0))
}

func randomIndex(size int) int {
	if size <= 1 {
		return 0
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 0
	}
	var value uint64
	for _, item := range raw {
		value = (value << 8) | uint64(item)
	}
	return int(value % uint64(size))
}

func chooseString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[randomIndex(len(values))]
}

func proofToken(seed, difficulty, userAgent string, sources []string, build string) (string, error) {
	target, err := hex.DecodeString(strings.TrimSpace(difficulty))
	if err != nil {
		return "", &SentinelError{Reason: "proof_difficulty"}
	}
	if len(target) == 0 {
		return "", nil
	}
	config := buildRequirementsConfig(sources, build, userAgent, time.Now())
	static1 := compactJSON(config[:3])
	static1 = append(static1[:len(static1)-1], ',')
	static2 := append([]byte{','}, compactJSON(config[4:9])[1:len(compactJSON(config[4:9]))-1]...)
	static2 = append(static2, ',')
	encodedTail := compactJSON(config[10:])
	static3 := append([]byte{','}, encodedTail[1:]...)
	for counter := 0; counter < 500000; counter++ {
		body := append([]byte{}, static1...)
		body = append(body, []byte(fmt.Sprintf("%d", counter))...)
		body = append(body, static2...)
		body = append(body, []byte(fmt.Sprintf("%d", counter>>1))...)
		body = append(body, static3...)
		encoded := base64.StdEncoding.EncodeToString(body)
		digest := sha3.Sum512(append([]byte(seed), []byte(encoded)...))
		if lessEqual(digest[:len(target)], target) {
			return "gAAAAAB" + encoded, nil
		}
	}
	return "", &SentinelError{Reason: "proof_timeout"}
}

func compactJSON(value any) []byte { encoded, _ := json.Marshal(value); return encoded }
func lessEqual(left, right []byte) bool {
	for index := range left {
		if left[index] < right[index] {
			return true
		}
		if left[index] > right[index] {
			return false
		}
	}
	return true
}
func newToken() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}
func boolValue(value any) bool { result, _ := value.(bool); return result }
func sentinelString(value any) string {
	if result, ok := value.(string); ok {
		return strings.TrimSpace(result)
	}
	return ""
}
func portsTransportError(err error) error { return ports.NewTransportError("ChatGPT sentinel", err) }
