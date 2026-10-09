package media

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
)

// RemoveWatermark applies a deterministic lower-right region repair for
// provider images. It deliberately returns PNG so the result is lossless and
// does not depend on Python/OpenCV at runtime.
func RemoveWatermark(data []byte) ([]byte, int, int, error) {
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode image: %w", err)
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width < 512 || height < 512 {
		return append([]byte(nil), data...), width, height, nil
	}
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canvas, canvas.Bounds(), source, bounds.Min, draw.Src)
	x1 := int(float64(width) * 0.655)
	y1 := height - maxInt(64, int(float64(height)*0.165))
	y2 := height - maxInt(12, int(float64(height)*0.01))
	if x1 < 1 {
		x1 = 1
	}
	if y1 < 1 {
		y1 = 1
	}
	if y2 <= y1 {
		return append([]byte(nil), data...), width, height, nil
	}
	// Copy the nearest clean vertical strip into the masked region. This is a
	// conservative deterministic fallback; provider-specific masks remain an
	// E2E concern and are never claimed from this local operation alone.
	for y := y1; y < y2; y++ {
		for x := x1; x < width; x++ {
			canvas.Set(x, y, canvas.At(x1-1, y))
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, canvas); err != nil {
		return nil, 0, 0, fmt.Errorf("encode repaired image: %w", err)
	}
	return output.Bytes(), width, height, nil
}

func DecodeAndEncodePNG(data []byte, mimeType string) ([]byte, error) {
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode %s image: %w", mimeType, err)
	}
	var output bytes.Buffer
	if err := png.Encode(&output, source); err != nil {
		return nil, fmt.Errorf("encode png image: %w", err)
	}
	return output.Bytes(), nil
}

func EncodeJPEG(data []byte, quality int) ([]byte, error) {
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := jpeg.Encode(&output, source, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
