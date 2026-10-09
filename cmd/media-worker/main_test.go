package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemoveWatermarkEndpointReturnsPngResult(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 512, 512))
	for y := 0; y < 512; y++ {
		for x := 0; x < 512; x++ {
			source.Set(x, y, color.RGBA{uint8(x % 255), uint8(y % 255), 120, 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"data_base64": base64.StdEncoding.EncodeToString(encoded.Bytes())})
	request := httptest.NewRequest(http.MethodPost, "/v1/watermark/remove", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	worker := &worker{root: t.TempDir(), token: "media-token"}
	request.Header.Set("X-Worker-Token", "media-token")
	worker.removeWatermark(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		DataBase64 string `json:"data_base64"`
		MIMEType   string `json:"mime_type"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		Method     string `json:"method"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(response.DataBase64)
	if err != nil || response.MIMEType != "image/png" || response.Width != 512 || response.Height != 512 || response.Method != "go_region_repair" {
		t.Fatalf("response metadata=%#v decode_error=%v", response, err)
	}
	if _, format, err := image.Decode(bytes.NewReader(data)); err != nil || format != "png" {
		t.Fatalf("decoded result format=%q error=%v", format, err)
	}
}

func TestAssetEndpointsRequireWorkerToken(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/assets", bytes.NewReader([]byte(`{}`)))
	unauthorized := httptest.NewRecorder()
	(&worker{root: t.TempDir(), token: "media-token"}).assets(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	missing := httptest.NewRecorder()
	(&worker{root: t.TempDir()}).assets(missing, httptest.NewRequest(http.MethodPost, "/v1/assets", bytes.NewReader(nil)))
	if missing.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing token status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestHealthReportsMissingTokenAsNotConfigured(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&worker{root: t.TempDir()}).health(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["status"] != "not_configured" {
		t.Fatalf("health response = %#v", response)
	}
}
