package media_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	mediaapp "github.com/XuLaiQ/all2api/internal/application/media"
	mediainfra "github.com/XuLaiQ/all2api/internal/infrastructure/media"
	"github.com/XuLaiQ/all2api/internal/infrastructure/persistence"
)

func TestStoreGenerationDownloadsAndTransformsProviderURL(t *testing.T) {
	data := pngFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "image/png")
		_, _ = writer.Write(data)
	}))
	defer server.Close()

	store, err := persistence.Open(context.Background(), t.TempDir()+"/generation.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mediaStore := mediainfra.NewStore(store.SQLDB(), store.Path())
	service := mediaapp.NewServiceWithTransformer(mediaStore, func(value []byte) ([]byte, error) {
		return value, nil
	})

	assets, err := service.StoreGeneration(context.Background(), map[string]any{
		"data": []any{map[string]any{"url": server.URL + "/generated.png"}},
	}, mediaapp.GenerationStoreOptions{Actor: "key:7", Channel: "doubao", Model: "model-a", Capability: "image"})
	if err != nil || len(assets) != 1 {
		t.Fatalf("StoreGeneration() = %#v/%v", assets, err)
	}
	asset := assets[0]
	if asset.SourceURL == nil || *asset.SourceURL != server.URL+"/generated.png" {
		t.Fatalf("source URL = %#v", asset.SourceURL)
	}
	if asset.Metadata["watermark_free"] != true || asset.Metadata["watermark_method"] != "go_region_repair" {
		t.Fatalf("watermark metadata = %#v", asset.Metadata)
	}
	if path, ok := mediaStore.MediaContentPath(asset); !ok {
		t.Fatal("generated asset was not stored")
	} else if _, err := os.Stat(path); err != nil {
		t.Fatalf("generated asset path = %v", err)
	}
}

func TestStoreGenerationKeepsRemoteAssetWhenDownloadFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	store, err := persistence.Open(context.Background(), t.TempDir()+"/remote.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mediaStore := mediainfra.NewStore(store.SQLDB(), store.Path())
	service := mediaapp.NewService(mediaStore)
	assets, err := service.StoreGeneration(context.Background(), map[string]any{
		"data": []any{map[string]any{"video_url": server.URL + "/video.mp4"}},
	}, mediaapp.GenerationStoreOptions{Actor: "key:8", Channel: "doubao", Model: "video-a", Capability: "video"})
	if err != nil || len(assets) != 1 {
		t.Fatalf("StoreGeneration() = %#v/%v", assets, err)
	}
	asset := assets[0]
	if asset.SourceURL == nil || asset.Metadata["storage_status"] != "remote" {
		t.Fatalf("remote asset = %#v", asset)
	}
	if _, ok := mediaStore.MediaContentPath(asset); ok {
		t.Fatal("failed download unexpectedly has local content")
	}
}

func TestStoreGenerationIgnoresSearchSources(t *testing.T) {
	store, err := persistence.Open(context.Background(), t.TempDir()+"/search.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := mediaapp.NewService(mediainfra.NewStore(store.SQLDB(), store.Path()))
	assets, err := service.StoreGeneration(context.Background(), map[string]any{
		"data": []any{map[string]any{"url": "https://example.test/source"}},
	}, mediaapp.GenerationStoreOptions{Actor: "key:9", Channel: "chatgpt", Model: "search-a", Capability: "search"})
	if err != nil || len(assets) != 0 {
		t.Fatalf("search assets = %#v/%v", assets, err)
	}
}

func pngFixture(t *testing.T) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			canvas.Set(x, y, color.RGBA{R: uint8(x * 40), G: uint8(y * 40), B: 120, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, canvas); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
