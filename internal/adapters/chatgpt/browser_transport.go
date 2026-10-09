package chatgpt

import (
	stdhttp "net/http"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// HTTPDoer is the small transport boundary shared by the Web client and OAuth
// client. It lets local contract tests keep using net/http while production
// Web requests use the browser TLS/HTTP2 profile.
type HTTPDoer interface {
	Do(*stdhttp.Request) (*stdhttp.Response, error)
}

type browserHTTPClient struct {
	client tlsclient.HttpClient
}

func (c *browserHTTPClient) Do(request *stdhttp.Request) (*stdhttp.Response, error) {
	fRequest, err := fhttp.NewRequestWithContext(request.Context(), request.Method, request.URL.String(), request.Body)
	if err != nil {
		return nil, err
	}
	fRequest.Host = request.Host
	fRequest.Header = make(fhttp.Header, len(request.Header)+1)
	for key, values := range request.Header {
		fRequest.Header[strings.ToLower(key)] = append([]string(nil), values...)
	}
	// Match the insertion order used by the reference curl_cffi session. The
	// TLS client keeps this order for HTTP/1.1 and uses the Chrome profile's
	// pseudo-header order for HTTP/2.
	fRequest.Header[fhttp.HeaderOrderKey] = []string{
		"user-agent", "origin", "referer", "accept-language", "cache-control", "pragma", "priority", "sec-ch-ua", "sec-ch-ua-arch",
		"sec-ch-ua-bitness", "sec-ch-ua-full-version", "sec-ch-ua-full-version-list",
		"sec-ch-ua-mobile", "sec-ch-ua-model", "sec-ch-ua-platform",
		"sec-ch-ua-platform-version", "sec-fetch-dest", "sec-fetch-mode",
		"sec-fetch-site", "oai-device-id",
		"oai-session-id", "oai-language", "oai-client-version",
		"oai-client-build-number", "authorization", "x-openai-target-path", "x-openai-target-route", "accept", "content-type",
		"openai-sentinel-chat-requirements-token", "openai-sentinel-proof-token",
		"openai-sentinel-turnstile-token", "openai-sentinel-so-token", "x-conduit-token",
	}

	fResponse, err := c.client.Do(fRequest)
	if err != nil {
		return nil, err
	}
	response := &stdhttp.Response{
		Status:           fResponse.Status,
		StatusCode:       fResponse.StatusCode,
		Proto:            fResponse.Proto,
		ProtoMajor:       fResponse.ProtoMajor,
		ProtoMinor:       fResponse.ProtoMinor,
		Header:           make(stdhttp.Header, len(fResponse.Header)),
		Body:             fResponse.Body,
		ContentLength:    fResponse.ContentLength,
		TransferEncoding: append([]string(nil), fResponse.TransferEncoding...),
		Close:            fResponse.Close,
		Uncompressed:     fResponse.Uncompressed,
		Request:          request,
	}
	for key, values := range fResponse.Header {
		response.Header[key] = append([]string(nil), values...)
	}
	return response, nil
}

func newChatGPTHTTPClient(baseURL, proxy string, timeout time.Duration) (HTTPDoer, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if !strings.HasPrefix(strings.ToLower(baseURL), "https://") {
		return &stdhttp.Client{Timeout: timeout}, nil
	}
	options := []tlsclient.HttpClientOption{
		tlsclient.WithTimeoutMilliseconds(int(timeout / time.Millisecond)),
		tlsclient.WithClientProfile(profiles.Chrome_110),
		tlsclient.WithRandomTLSExtensionOrder(),
		tlsclient.WithCookieJar(tlsclient.NewCookieJar()),
	}
	if proxy != "" {
		options = append(options, tlsclient.WithProxyUrl(proxy))
	}
	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, err
	}
	return &browserHTTPClient{client: client}, nil
}

func cloneHTTPClient(client HTTPDoer, baseURL, proxy string, timeout time.Duration) HTTPDoer {
	if next, err := newChatGPTHTTPClient(baseURL, proxy, timeout); err == nil {
		return next
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if client != nil {
		return client
	}
	return &stdhttp.Client{Timeout: timeout}
}

var _ HTTPDoer = (*stdhttp.Client)(nil)
