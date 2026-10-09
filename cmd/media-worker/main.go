package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	mediainfra "github.com/XuLaiQ/all2api/internal/infrastructure/media"
)

const maxAssetBytes = 100 * 1024 * 1024

var safeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

type worker struct {
	root  string
	token string
}

func main() {
	root := os.Getenv("A2A_MEDIA_WORKER_ROOT")
	if root == "" {
		root = filepath.Join("data", "media-worker")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		panic("media worker storage root is unavailable")
	}
	w := &worker{root: root, token: os.Getenv("A2A_MEDIA_WORKER_TOKEN")}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", w.health)
	mux.HandleFunc("/v1/assets", w.assets)
	mux.HandleFunc("/v1/assets/", w.asset)
	mux.HandleFunc("/v1/watermark/remove", w.removeWatermark)
	port := envInt("A2A_MEDIA_WORKER_PORT", 8892)
	if err := http.ListenAndServe(":"+strconv.Itoa(port), mux); err != nil {
		panic("media worker server stopped unexpectedly")
	}
}

func (w *worker) removeWatermark(writer http.ResponseWriter, request *http.Request) {
	if !w.authorized(writer, request) {
		return
	}
	if request.Method != http.MethodPost {
		writeJSON(writer, http.StatusMethodNotAllowed, errorBody("request_error", "method not allowed"))
		return
	}
	var body struct {
		Data string `json:"data_base64"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 140<<20))
	if err := decoder.Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorBody("invalid_request", "data_base64 is required"))
		return
	}
	data, err := base64.StdEncoding.DecodeString(body.Data)
	if err != nil || len(data) == 0 {
		writeJSON(writer, http.StatusUnprocessableEntity, errorBody("invalid_request", "data_base64 is invalid"))
		return
	}
	if len(data) > maxAssetBytes {
		writeJSON(writer, http.StatusRequestEntityTooLarge, errorBody("asset_too_large", "image exceeds the 100 MB limit"))
		return
	}
	repaired, width, height, err := mediainfra.RemoveWatermark(data)
	if err != nil {
		writeJSON(writer, http.StatusUnprocessableEntity, errorBody("invalid_image", "image could not be decoded"))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data_base64": base64.StdEncoding.EncodeToString(repaired), "mime_type": "image/png", "width": width, "height": height, "method": "go_region_repair"})
}

func (w *worker) health(writer http.ResponseWriter, _ *http.Request) {
	status := "ok"
	if w.token == "" {
		status = "not_configured"
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": status, "service": "media-worker", "runtime": "go", "max_asset_bytes": maxAssetBytes, "capabilities": []string{"asset_store", "watermark_remove"}})
}

func (w *worker) assets(writer http.ResponseWriter, request *http.Request) {
	if !w.authorized(writer, request) {
		return
	}
	if request.Method != http.MethodPost {
		writeJSON(writer, http.StatusMethodNotAllowed, errorBody("request_error", "method not allowed"))
		return
	}
	var body struct {
		Filename string `json:"filename"`
		MIMEType string `json:"mime_type"`
		Data     string `json:"data_base64"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 140<<20))
	if err := decoder.Decode(&body); err != nil {
		writeJSON(writer, 400, errorBody("invalid_request", "asset body is invalid"))
		return
	}
	data, err := base64.StdEncoding.DecodeString(body.Data)
	if err != nil || len(data) == 0 {
		writeJSON(writer, 422, errorBody("invalid_request", "data_base64 is invalid"))
		return
	}
	if len(data) > maxAssetBytes {
		writeJSON(writer, 413, errorBody("asset_too_large", "media asset exceeds the 100 MB limit"))
		return
	}
	id, err := assetID()
	if err != nil {
		writeJSON(writer, 500, errorBody("internal_error", "could not allocate asset id"))
		return
	}
	name := safeName.ReplaceAllString(filepath.Base(body.Filename), "-")
	if name == "" || name == "." {
		name = "asset.bin"
	}
	folder := filepath.Join(w.root, id)
	if err := os.MkdirAll(folder, 0o750); err != nil {
		writeJSON(writer, 500, errorBody("internal_error", "could not create asset directory"))
		return
	}
	path := filepath.Join(folder, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		writeJSON(writer, 500, errorBody("internal_error", "could not store asset"))
		return
	}
	writeJSON(writer, 201, map[string]any{"id": id, "filename": name, "mime_type": body.MIMEType, "size_bytes": len(data), "storage_path": filepath.ToSlash(filepath.Join(id, name))})
}

func (w *worker) asset(writer http.ResponseWriter, request *http.Request) {
	if !w.authorized(writer, request) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(request.URL.Path, "/v1/assets/"), "/")
	parts := strings.Split(id, "/")
	if len(parts) != 2 {
		writeJSON(writer, 404, errorBody("not_found", "asset not found"))
		return
	}
	path := filepath.Join(w.root, parts[0], parts[1])
	root, _ := filepath.Abs(w.root)
	resolved, _ := filepath.Abs(path)
	if resolved == root || !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		writeJSON(writer, 404, errorBody("not_found", "asset not found"))
		return
	}
	switch request.Method {
	case http.MethodGet:
		http.ServeFile(writer, request, resolved)
	case http.MethodDelete:
		if err := os.Remove(resolved); err != nil && !os.IsNotExist(err) {
			writeJSON(writer, 500, errorBody("internal_error", "could not delete asset"))
			return
		}
		_ = os.Remove(filepath.Dir(resolved))
		writer.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(writer, 405, errorBody("request_error", "method not allowed"))
	}
}

func (w *worker) authorized(writer http.ResponseWriter, request *http.Request) bool {
	if w.token == "" {
		writeJSON(writer, http.StatusServiceUnavailable, errorBody("worker_token_not_configured", "media worker token is not configured"))
		return false
	}
	if request.Header.Get("X-Worker-Token") != w.token {
		writeJSON(writer, http.StatusUnauthorized, errorBody("unauthorized", "valid worker token required"))
		return false
	}
	return true
}

func assetID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "asset_" + hex.EncodeToString(raw[:]), nil
}
func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
func errorBody(code, message string) map[string]any {
	return map[string]any{"error": map[string]string{"code": code, "message": message}}
}
func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value < 1 || value > 65535 {
		return fallback
	}
	return value
}
