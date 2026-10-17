package media

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
)

// WatermarkMethod identifies the repair algorithm in asset metadata and API
// responses.
const WatermarkMethod = "telea_inpaint"

// watermarkInpaintRadius matches the radius used with cv2.INPAINT_TELEA.
const watermarkInpaintRadius = 7

// RemoveWatermark repairs the fixed lower-right watermark region of provider
// images with Telea inpainting and returns a lossless PNG of the same size.
// Images smaller than 512x512, or too short to have clean pixels above the
// region, are returned unchanged.
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
	x1, y1, y2 := watermarkRegion(width, height)
	if regionHeight := y2 - y1; regionHeight <= 0 || y1-regionHeight < 0 {
		return append([]byte(nil), data...), width, height, nil
	}
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canvas, canvas.Bounds(), source, bounds.Min, draw.Src)
	mask := make([]bool, width*height)
	for y := y1; y < y2; y++ {
		for x := x1; x < width; x++ {
			mask[y*width+x] = true
		}
	}
	inpaintTelea(canvas, mask, watermarkInpaintRadius)
	var output bytes.Buffer
	if err := png.Encode(&output, canvas); err != nil {
		return nil, 0, 0, fmt.Errorf("encode repaired image: %w", err)
	}
	return output.Bytes(), width, height, nil
}

// watermarkRegion returns the masked rectangle [x1,width) x [y1,y2). The
// bottom margin below y2 stays untouched so the image edge remains stable.
func watermarkRegion(width, height int) (x1, y1, y2 int) {
	x1 = maxInt(0, int(float64(width)*0.655))
	y2 = maxInt(1, height-maxInt(12, int(float64(height)*0.01)))
	y1 = maxInt(0, height-maxInt(64, int(float64(height)*0.165)))
	return x1, y1, y2
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
