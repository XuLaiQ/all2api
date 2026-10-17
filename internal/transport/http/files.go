package httptransport

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	mediainfra "github.com/XuLaiQ/all2api/internal/infrastructure/media"
	"github.com/XuLaiQ/all2api/internal/ports"
)

func (s *Server) filesRoot(w http.ResponseWriter, r *http.Request) {
	key, ok := s.authenticateFileKey(w, r)
	if !ok {
		return
	}
	if s.media == nil {
		writeOpenAIError(w, r, http.StatusServiceUnavailable, "service_unavailable", "file storage is unavailable", nil)
		return
	}
	actor := fileActor(key)
	switch r.Method {
	case http.MethodGet:
		page, size, err := parsePagination(r)
		if err != nil {
			writeOpenAIError(w, r, http.StatusBadRequest, "invalid_request", err.Error(), nil)
			return
		}
		rows, _, err := s.media.List(r.Context(), ports.MediaQuery{Page: page, PageSize: size, Actor: actor, Kind: "other"})
		if err != nil {
			writeOpenAIError(w, r, http.StatusServiceUnavailable, "service_unavailable", "file storage is unavailable", nil)
			return
		}
		data := make([]map[string]any, 0, len(rows))
		for _, asset := range rows {
			data = append(data, filePayload(asset))
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"object": "list", "data": data})
	case http.MethodPost:
		if err := s.uploadFile(w, r, key, actor); err != nil {
			status, code, message := http.StatusUnprocessableEntity, "invalid_request", err.Error()
			if errors.Is(err, errFileTooLarge) {
				status, code, message = http.StatusRequestEntityTooLarge, "file_too_large", "file exceeds the 100 MB limit"
			}
			writeOpenAIError(w, r, status, code, message, nil)
		}
	default:
		writeOpenAIError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
	}
}

func (s *Server) fileItem(w http.ResponseWriter, r *http.Request) {
	key, ok := s.authenticateFileKey(w, r)
	if !ok {
		return
	}
	if s.media == nil {
		writeOpenAIError(w, r, http.StatusServiceUnavailable, "service_unavailable", "file storage is unavailable", nil)
		return
	}
	id := r.PathValue("file_id")
	if !validFileID(id) {
		writeOpenAIError(w, r, http.StatusNotFound, "file_not_found", "file not found", nil)
		return
	}
	asset, err := s.media.Get(r.Context(), id, fileActor(key))
	if errors.Is(err, ports.ErrKeyNotFound) {
		writeOpenAIError(w, r, http.StatusNotFound, "file_not_found", "file not found", nil)
		return
	}
	if err != nil {
		writeOpenAIError(w, r, http.StatusServiceUnavailable, "service_unavailable", "file storage is unavailable", nil)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, r, http.StatusOK, filePayload(asset))
	case http.MethodDelete:
		if err := s.media.Delete(r.Context(), id, fileActor(key), ports.AuditEvent{Actor: fileActor(key), Action: "file.deleted", IP: s.clientIP(r)}); err != nil {
			writeOpenAIError(w, r, http.StatusServiceUnavailable, "service_unavailable", "file could not be deleted", nil)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"id": id, "object": "file", "deleted": true})
	default:
		writeOpenAIError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
	}
}

func (s *Server) fileContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	key, ok := s.authenticateFileKey(w, r)
	if !ok {
		return
	}
	if s.media == nil || !validFileID(r.PathValue("file_id")) {
		writeOpenAIError(w, r, http.StatusNotFound, "file_not_found", "file not found", nil)
		return
	}
	asset, err := s.media.Get(r.Context(), r.PathValue("file_id"), fileActor(key))
	if err != nil {
		writeOpenAIError(w, r, http.StatusNotFound, "file_not_found", "file not found", nil)
		return
	}
	contentPath, ok := s.media.ContentPath(asset)
	if !ok {
		writeOpenAIError(w, r, http.StatusNotFound, "file_not_found", "file content is unavailable", nil)
		return
	}
	if asset.MIMEType != "" {
		w.Header().Set("Content-Type", asset.MIMEType)
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", asset.Filename))
	http.ServeFile(w, r, contentPath)
}

var errFileTooLarge = errors.New("file is too large")

func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request, key ports.APIKey, actor string) error {
	r.Body = http.MaxBytesReader(w, r.Body, mediainfra.MaxAssetBytes+2<<20)
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "request body too large") {
			return errFileTooLarge
		}
		return errors.New("multipart file body is invalid")
	}
	fileHeaders := r.MultipartForm.File["file"]
	if len(fileHeaders) == 0 {
		return errors.New("file field is required")
	}
	file, err := fileHeaders[0].Open()
	if err != nil {
		return errors.New("file could not be opened")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, mediainfra.MaxAssetBytes+1))
	if err != nil {
		return errors.New("file could not be read")
	}
	if len(data) == 0 {
		return errors.New("file must not be empty")
	}
	if len(data) > mediainfra.MaxAssetBytes {
		return errFileTooLarge
	}
	mimeType := fileHeaders[0].Header.Get("Content-Type")
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = http.DetectContentType(data)
	}
	filename := fileHeaders[0].Filename
	kind := "other"
	purpose := ""
	if values := r.MultipartForm.Value["purpose"]; len(values) > 0 {
		purpose = strings.TrimSpace(values[0])
	}
	if purpose == "" {
		purpose = "assistants"
	}
	asset, err := s.media.AddBytes(r.Context(), ports.MediaAsset{Actor: actor, Channel: "local", Kind: kind, MIMEType: mimeType, Filename: filename, Metadata: map[string]any{"purpose": purpose, "key_id": key.ID}}, data)
	if err != nil {
		return errors.New("file could not be stored")
	}
	if s.auditWriter != nil {
		if err := s.auditWriter.RecordAudit(r.Context(), actor, "file.uploaded", asset.ID, "purpose="+purpose, s.clientIP(r)); err != nil {
			_ = s.media.Delete(r.Context(), asset.ID, actor, ports.AuditEvent{Actor: actor, Action: "file.upload_rollback", IP: s.clientIP(r)})
			return errors.New("file audit could not be recorded")
		}
	}
	writeJSON(w, r, http.StatusOK, filePayload(asset))
	return nil
}

func (s *Server) authenticateFileKey(w http.ResponseWriter, r *http.Request) (ports.APIKey, bool) {
	key, err := s.gateway.Authenticate(r.Context(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
	if err != nil {
		s.writeOpenAIServiceError(w, r, err)
		return ports.APIKey{}, false
	}
	return key, true
}

func fileActor(key ports.APIKey) string { return fmt.Sprintf("key:%d", key.ID) }

func validFileID(value string) bool {
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "/\\") {
		return false
	}
	return strings.HasPrefix(value, "asset_")
}

func filePayload(asset ports.MediaAsset) map[string]any {
	purpose := "assistants"
	if value, ok := asset.Metadata["purpose"].(string); ok && value != "" {
		purpose = value
	}
	return map[string]any{"id": asset.ID, "object": "file", "bytes": asset.SizeBytes, "created_at": asset.CreatedAt, "filename": asset.Filename, "purpose": purpose, "status": "processed", "status_details": nil}
}
