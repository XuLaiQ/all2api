package media

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

const maxDownloadBytes = 100 * 1024 * 1024

type ByteTransformer func([]byte) ([]byte, error)

type GenerationStoreOptions struct {
	Actor          string
	Channel        string
	Model          string
	Capability     string
	RunID          *string
	ConversationID *string
	Metadata       map[string]any
}

type Service struct {
	repository ports.MediaRepository
	transform  ByteTransformer
}

func NewService(repository ports.MediaRepository) *Service {
	return NewServiceWithTransformer(repository, nil)
}

func NewServiceWithTransformer(repository ports.MediaRepository, transform ByteTransformer) *Service {
	return &Service{repository: repository, transform: transform}
}

func (s *Service) List(ctx context.Context, query ports.MediaQuery) ([]ports.MediaAsset, int, error) {
	if s.repository == nil {
		return nil, 0, errors.New("media storage is unavailable")
	}
	return s.repository.ListMedia(ctx, query)
}

func (s *Service) Get(ctx context.Context, id, actor string) (ports.MediaAsset, error) {
	if s.repository == nil {
		return ports.MediaAsset{}, errors.New("media storage is unavailable")
	}
	return s.repository.GetMedia(ctx, id, actor)
}

func (s *Service) Delete(ctx context.Context, id, actor string, audit ports.AuditEvent) error {
	if s.repository == nil {
		return errors.New("media storage is unavailable")
	}
	return s.repository.DeleteMedia(ctx, id, actor, audit)
}

func (s *Service) ContentPath(asset ports.MediaAsset) (string, bool) {
	if s.repository == nil {
		return "", false
	}
	return s.repository.MediaContentPath(asset)
}

func (s *Service) AddBytes(ctx context.Context, asset ports.MediaAsset, data []byte) (ports.MediaAsset, error) {
	if s.repository == nil {
		return ports.MediaAsset{}, errors.New("media storage is unavailable")
	}
	return s.repository.AddMediaBytes(ctx, asset, data)
}

func (s *Service) AddRemote(ctx context.Context, asset ports.MediaAsset, sourceURL string) (ports.MediaAsset, error) {
	if s.repository == nil {
		return ports.MediaAsset{}, errors.New("media storage is unavailable")
	}
	return s.repository.AddMediaRemote(ctx, asset, sourceURL)
}

// StoreGeneration normalizes provider media responses into durable local assets.
// Signed provider URLs remain usable when download is unavailable, but their
// metadata is still recorded so the API can expose a stable asset identity.
func (s *Service) StoreGeneration(ctx context.Context, response any, options GenerationStoreOptions) ([]ports.MediaAsset, error) {
	if s.repository == nil {
		return nil, errors.New("media storage is unavailable")
	}
	if options.Capability != "image" && options.Capability != "video" {
		return nil, nil
	}
	object, ok := response.(map[string]any)
	if !ok {
		return nil, nil
	}
	values := generationValues(object["data"])
	assets := make([]ports.MediaAsset, 0, len(values))
	for index, value := range values {
		metadata := cloneMetadata(options.Metadata)
		metadata["generation_index"] = index
		if value["watermark_free"] == true {
			metadata["watermark_free"] = true
			if method, ok := value["watermark_method"].(string); ok && method != "" {
				metadata["watermark_method"] = method
			}
		}
		kind := "image"
		if options.Capability == "video" {
			kind = "video"
		}
		filename := generatedFilename(value, kind, index+1)
		if encoded, ok := value["b64_json"].(string); ok && strings.TrimSpace(encoded) != "" {
			data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
			if err == nil && len(data) > 0 {
				mimeType := generatedMIME(kind, data)
				asset, storeErr := s.storeGeneratedBytes(ctx, options, kind, mimeType, filename, "", data, metadata)
				if storeErr != nil {
					return assets, storeErr
				}
				assets = append(assets, asset)
				continue
			}
		}
		sourceURL := firstString(value["video_url"], value["url"], value["source_url"])
		if sourceURL == "" {
			continue
		}
		asset, err := s.storeGeneratedURL(ctx, options, kind, filename, sourceURL, metadata)
		if err != nil {
			return assets, err
		}
		assets = append(assets, asset)
	}
	return assets, nil
}

func (s *Service) storeGeneratedURL(ctx context.Context, options GenerationStoreOptions, kind, filename, sourceURL string, metadata map[string]any) (ports.MediaAsset, error) {
	parsed, err := url.Parse(strings.TrimSpace(sourceURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ports.MediaAsset{}, fmt.Errorf("media source URL is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return ports.MediaAsset{}, err
	}
	client := &http.Client{Timeout: 90 * time.Second}
	response, err := client.Do(request)
	if err == nil {
		defer response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, maxDownloadBytes+1))
			if readErr != nil {
				return ports.MediaAsset{}, readErr
			}
			if len(data) > maxDownloadBytes {
				return ports.MediaAsset{}, errors.New("media asset exceeds size limit")
			}
			mimeType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
			if mimeType == "" || mimeType == "application/octet-stream" {
				mimeType = generatedMIME(kind, data)
			}
			return s.storeGeneratedBytes(ctx, options, kind, mimeType, filename, parsed.String(), data, metadata)
		}
	}

	// Preserve a signed CDN result even when it cannot be downloaded locally.
	asset := ports.MediaAsset{
		Actor:          options.Actor,
		RunID:          options.RunID,
		ConversationID: options.ConversationID,
		Channel:        options.Channel,
		Model:          options.Model,
		Kind:           kind,
		MIMEType:       mimeFromFilename(filename, kind),
		Filename:       filename,
		Metadata:       cloneMetadata(metadata),
	}
	asset.Metadata["storage_status"] = "remote"
	return s.AddRemote(ctx, asset, parsed.String())
}

func (s *Service) storeGeneratedBytes(ctx context.Context, options GenerationStoreOptions, kind, mimeType, filename, sourceURL string, data []byte, metadata map[string]any) (ports.MediaAsset, error) {
	metadata = cloneMetadata(metadata)
	if options.Channel == "doubao" && kind == "image" && s.transform != nil {
		if repaired, err := s.transform(data); err == nil && len(repaired) > 0 {
			data = repaired
			mimeType = "image/png"
			metadata["watermark_free"] = true
			metadata["watermark_method"] = "go_region_repair"
		}
	}
	asset := ports.MediaAsset{
		Actor:          options.Actor,
		RunID:          options.RunID,
		ConversationID: options.ConversationID,
		Channel:        options.Channel,
		Model:          options.Model,
		Kind:           kind,
		MIMEType:       mimeType,
		Filename:       filename,
		Metadata:       metadata,
	}
	if sourceURL != "" {
		asset.SourceURL = &sourceURL
	}
	return s.AddBytes(ctx, asset, data)
}

func generationValues(value any) []map[string]any {
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

func cloneMetadata(value map[string]any) map[string]any {
	result := make(map[string]any, len(value)+2)
	for key, item := range value {
		result[key] = item
	}
	return result
}

func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func generatedFilename(value map[string]any, kind string, index int) string {
	if name := firstString(value["filename"], value["name"]); name != "" {
		return name
	}
	if source := firstString(value["video_url"], value["url"]); source != "" {
		if parsed, err := url.Parse(source); err == nil && path.Ext(parsed.Path) != "" {
			return path.Base(parsed.Path)
		}
	}
	if kind == "video" {
		return fmt.Sprintf("video_%d.mp4", index)
	}
	return fmt.Sprintf("image_%d.png", index)
}

func generatedMIME(kind string, data []byte) string {
	if len(data) > 0 {
		if detected := http.DetectContentType(data); detected != "application/octet-stream" {
			return detected
		}
	}
	if kind == "video" {
		return "video/mp4"
	}
	return "image/png"
}

func mimeFromFilename(filename, kind string) string {
	if value := mime.TypeByExtension(path.Ext(filename)); value != "" {
		return value
	}
	if kind == "video" {
		return "video/mp4"
	}
	return "image/png"
}
