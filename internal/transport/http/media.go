package httptransport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	mediaapp "github.com/XuLaiQ/all2api/internal/application/media"
	mediainfra "github.com/XuLaiQ/all2api/internal/infrastructure/media"
	"github.com/XuLaiQ/all2api/internal/ports"
)

func (s *Server) mediaAssets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	user, ok := s.requireAdmin(w, r, false)
	if !ok {
		return
	}
	if s.media == nil {
		writeAdminError(w, r, 503, "service_unavailable", "media storage is unavailable", nil)
		return
	}
	page, size, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, r, 422, "validation_error", err.Error(), nil)
		return
	}
	rows, total, err := s.media.List(r.Context(), ports.MediaQuery{Page: page, PageSize: size, Actor: user.Username, Kind: r.URL.Query().Get("kind"), Channel: r.URL.Query().Get("channel"), Search: r.URL.Query().Get("search")})
	if err != nil {
		writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, asset := range rows {
		payload := map[string]any{"id": asset.ID, "actor": asset.Actor, "run_id": asset.RunID, "conversation_id": asset.ConversationID, "channel": asset.Channel, "model": asset.Model, "kind": asset.Kind, "mime_type": asset.MIMEType, "filename": asset.Filename, "source_url": asset.SourceURL, "size_bytes": asset.SizeBytes, "metadata": asset.Metadata, "created_at": asset.CreatedAt, "updated_at": asset.UpdatedAt, "content_url": "/admin/api/media/assets/" + asset.ID + "/content?inline=1", "storage_status": "remote"}
		if _, ok := s.media.ContentPath(asset); ok {
			payload["storage_status"] = "stored"
		}
		data = append(data, payload)
	}
	writeJSON(w, r, 200, map[string]any{"data": data, "pagination": map[string]int{"page": page, "page_size": size, "total": total, "total_pages": (total + size - 1) / size}})
}

func (s *Server) mediaAsset(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	if s.media == nil {
		writeAdminError(w, r, 503, "service_unavailable", "media storage is unavailable", nil)
		return
	}
	if user.Role != "admin" {
		writeAdminError(w, r, 403, "forbidden", "admin role required", nil)
		return
	}
	if r.Method != http.MethodDelete {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	if err := s.media.Delete(r.Context(), r.PathValue("asset_id"), user.Username, ports.AuditEvent{Actor: user.Username, Action: "delete_media", IP: s.clientIP(r)}); err != nil {
		if errors.Is(err, ports.ErrKeyNotFound) {
			writeAdminError(w, r, 404, "not_found", "media asset not found", nil)
		} else {
			writeAdminError(w, r, 503, "service_unavailable", err.Error(), nil)
		}
		return
	}
	w.WriteHeader(204)
}

func (s *Server) mediaContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAdminError(w, r, 405, "request_error", "method not allowed", nil)
		return
	}
	user, ok := s.requireAdmin(w, r, false)
	if !ok {
		return
	}
	if s.media == nil {
		writeAdminError(w, r, 503, "service_unavailable", "media storage is unavailable", nil)
		return
	}
	asset, err := s.media.Get(r.Context(), r.PathValue("asset_id"), user.Username)
	if err != nil {
		writeAdminError(w, r, 404, "not_found", "media asset not found", nil)
		return
	}
	path, ok := s.media.ContentPath(asset)
	if !ok {
		if asset.SourceURL != nil && *asset.SourceURL != "" {
			http.Redirect(w, r, *asset.SourceURL, http.StatusFound)
			return
		}
		writeAdminError(w, r, 404, "not_found", "media content is unavailable", nil)
		return
	}
	if asset.MIMEType != "" {
		w.Header().Set("Content-Type", asset.MIMEType)
	}
	http.ServeFile(w, r, path)
}

func (s *Server) mediaWatermark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminError(w, r, http.StatusMethodNotAllowed, "request_error", "method not allowed", nil)
		return
	}
	user, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	if user.Role != "admin" {
		writeAdminError(w, r, http.StatusForbidden, "forbidden", "admin role required", nil)
		return
	}
	if s.media == nil {
		writeAdminError(w, r, http.StatusServiceUnavailable, "service_unavailable", "media storage is unavailable", nil)
		return
	}
	source, err := s.media.Get(r.Context(), r.PathValue("asset_id"), user.Username)
	if err != nil || source.Kind != "image" {
		writeAdminError(w, r, http.StatusNotFound, "not_found", "image asset not found", nil)
		return
	}
	var body struct {
		DataURL  string `json:"data_url"`
		Filename string `json:"filename"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 140<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "watermark result is invalid", nil)
		return
	}
	data, mimeType, ok := decodeImageDataURL(body.DataURL)
	if !ok {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "watermark result must be a base64 image", nil)
		return
	}
	filename := strings.TrimSpace(body.Filename)
	if filename == "" {
		filename = source.Filename
		if dot := strings.LastIndex(filename, "."); dot > 0 {
			filename = filename[:dot]
		}
		filename += ".png"
	}
	metadata := map[string]any{"operation": "client_watermark_removal", "derived_from": source.ID}
	asset, err := s.media.AddBytes(r.Context(), ports.MediaAsset{
		Actor:          user.Username,
		RunID:          source.RunID,
		ConversationID: source.ConversationID,
		Channel:        source.Channel,
		Model:          source.Model,
		Kind:           "image",
		MIMEType:       mimeType,
		Filename:       filename,
		Metadata:       metadata,
	}, data)
	if err != nil {
		writeAdminError(w, r, http.StatusUnprocessableEntity, "validation_error", "could not store watermark result", nil)
		return
	}
	if s.auditWriter != nil {
		if err := s.auditWriter.RecordAudit(r.Context(), user.Username, "remove_media_watermark", asset.ID, "derived_from="+source.ID, s.clientIP(r)); err != nil {
			writeAdminError(w, r, http.StatusInternalServerError, "internal_error", "could not record media audit", nil)
			return
		}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"data": mediaAssetPayload(asset, s.media)})
}

func decodeImageDataURL(value string) ([]byte, string, bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "data:image/") {
		return nil, "", false
	}
	comma := strings.IndexByte(value, ',')
	if comma <= len("data:image/") || !strings.Contains(value[:comma], ";base64") {
		return nil, "", false
	}
	header := strings.ToLower(value[:comma])
	mimeType := strings.TrimPrefix(strings.SplitN(header, ";", 2)[0], "data:")
	switch mimeType {
	case "image/png", "image/jpeg", "image/webp":
	default:
		return nil, "", false
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value[comma+1:]))
	if err != nil || len(data) == 0 || len(data) > mediainfra.MaxAssetBytes {
		return nil, "", false
	}
	return data, mimeType, true
}

func mediaAssetPayload(asset ports.MediaAsset, service *mediaapp.Service) map[string]any {
	storageStatus := "remote"
	if _, ok := service.ContentPath(asset); ok {
		storageStatus = "stored"
	}
	return map[string]any{
		"id": asset.ID, "actor": asset.Actor, "run_id": asset.RunID, "conversation_id": asset.ConversationID,
		"channel": asset.Channel, "model": asset.Model, "kind": asset.Kind, "mime_type": asset.MIMEType,
		"filename": asset.Filename, "source_url": asset.SourceURL, "size_bytes": asset.SizeBytes,
		"metadata": asset.Metadata, "created_at": asset.CreatedAt, "updated_at": asset.UpdatedAt,
		"content_url": "/admin/api/media/assets/" + asset.ID + "/content?inline=1", "storage_status": storageStatus,
	}
}

func (s *Server) storeCapabilityMedia(ctx context.Context, actor, channel, model, capability string, body any, publicURL bool, runID, conversationID *string) (any, []ports.MediaAsset, error) {
	if s.media == nil {
		return body, nil, nil
	}
	assets, err := s.media.StoreGeneration(ctx, body, mediaapp.GenerationStoreOptions{
		Actor:          actor,
		Channel:        channel,
		Model:          model,
		Capability:     capability,
		RunID:          runID,
		ConversationID: conversationID,
	})
	if err != nil {
		return body, assets, err
	}
	object, ok := body.(map[string]any)
	if !ok || len(assets) == 0 {
		return body, assets, nil
	}
	values := capabilityDataItems(object["data"])
	for fallbackIndex, asset := range assets {
		index := fallbackIndex
		if value, ok := asset.Metadata["generation_index"].(int); ok {
			index = value
		} else if value, ok := asset.Metadata["generation_index"].(float64); ok {
			index = int(value)
		}
		if index < 0 || index >= len(values) {
			continue
		}
		contentURL := "/admin/api/media/assets/" + asset.ID + "/content?inline=1"
		if publicURL {
			contentURL = "/v1/files/" + asset.ID + "/content"
		}
		values[index]["url"] = contentURL
		if capability == "video" {
			values[index]["video_url"] = contentURL
		}
		if free, ok := asset.Metadata["watermark_free"].(bool); ok && free {
			values[index]["watermark_free"] = true
			values[index]["source_variant"] = "telea_inpaint"
		}
	}
	object["data"] = values
	return object, assets, nil
}

func capabilityDataItems(value any) []map[string]any {
	switch values := value.(type) {
	case []map[string]any:
		return values
	case []any:
		result := make([]map[string]any, 0, len(values))
		for _, item := range values {
			if object, ok := item.(map[string]any); ok {
				result = append(result, object)
			}
		}
		return result
	default:
		return nil
	}
}
