package media

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"

	"github.com/XuLaiQ/all2api/internal/infrastructure/persistence"
	"github.com/XuLaiQ/all2api/internal/ports"
)

func TestStoreWritesSafeAssetAndDeletesIt(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/media.db"
	db, err := persistence.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer db.Close()
	store := NewStore(db.SQLDB(), db.Path())
	asset, err := store.AddMediaBytes(ctx, ports.MediaAsset{Actor: "admin", Channel: "doubao", Model: "image-test", Kind: "image", MIMEType: "image/png", Filename: "../../unsafe name.png"}, []byte("image-fixture"))
	if err != nil {
		t.Fatalf("AddMediaBytes() error = %v", err)
	}
	if asset.StoragePath == nil || *asset.StoragePath == "" {
		t.Fatal("asset has no storage path")
	}
	contentPath, ok := store.MediaContentPath(asset)
	if !ok {
		t.Fatal("asset content path is unavailable")
	}
	if _, err := os.Stat(contentPath); err != nil {
		t.Fatalf("stored content missing: %v", err)
	}
	rows, total, err := store.ListMedia(ctx, ports.MediaQuery{Page: 1, PageSize: 20, Actor: "admin"})
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatalf("ListMedia() = %#v/%d/%v", rows, total, err)
	}
	if err := store.DeleteMedia(ctx, asset.ID, "admin", ports.AuditEvent{Actor: "admin"}); err != nil {
		t.Fatalf("DeleteMedia() error = %v", err)
	}
	if _, ok := store.MediaContentPath(asset); ok {
		t.Fatal("deleted asset still has content path")
	}
}

func TestRemoveWatermarkReturnsValidLosslessPng(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 512, 512))
	for y := 0; y < 512; y++ {
		for x := 0; x < 512; x++ {
			source.Set(x, y, color.RGBA{uint8(x % 255), uint8(y % 255), 90, 255})
		}
	}
	var input bytes.Buffer
	if err := png.Encode(&input, source); err != nil {
		t.Fatal(err)
	}
	output, width, height, err := RemoveWatermark(input.Bytes())
	if err != nil || width != 512 || height != 512 {
		t.Fatalf("RemoveWatermark() = %d/%d/%v", width, height, err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(output))
	if err != nil || format != "png" || decoded.Bounds().Dx() != 512 || decoded.Bounds().Dy() != 512 {
		t.Fatalf("repaired image = format=%q bounds=%v err=%v", format, decoded.Bounds(), err)
	}
}
