package media

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"testing"
)

// smoothScene is a gradient "background" that inpainting should reconstruct.
func smoothScene(x, y int) color.RGBA {
	return color.RGBA{uint8(40 + x/8), uint8(60 + y/8), uint8(120 + (x+y)/16), 255}
}

func TestRemoveWatermarkReconstructsSmoothBackground(t *testing.T) {
	const size = 768
	source := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			source.SetRGBA(x, y, smoothScene(x, y))
		}
	}
	x1, y1, y2 := watermarkRegion(size, size)
	// Stamp a bright "watermark" inside the masked region.
	for y := y1 + 10; y < y2-10; y++ {
		for x := x1 + 20; x < size-20; x++ {
			if (x/6+y/6)%2 == 0 {
				source.SetRGBA(x, y, color.RGBA{255, 255, 255, 255})
			}
		}
	}
	var input bytes.Buffer
	if err := png.Encode(&input, source); err != nil {
		t.Fatal(err)
	}

	output, width, height, err := RemoveWatermark(input.Bytes())
	if err != nil || width != size || height != size {
		t.Fatalf("RemoveWatermark() = %d/%d/%v", width, height, err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(output))
	if err != nil || format != "png" {
		t.Fatalf("output format=%q err=%v", format, err)
	}

	var totalError float64
	var pixels int
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			got := color.RGBAModel.Convert(decoded.At(x, y)).(color.RGBA)
			want := smoothScene(x, y)
			inRegion := x >= x1 && y >= y1 && y < y2
			if !inRegion {
				if got != source.RGBAAt(x, y) {
					t.Fatalf("pixel outside the mask changed at %d,%d: %v -> %v", x, y, source.RGBAAt(x, y), got)
				}
				continue
			}
			totalError += math.Abs(float64(got.R)-float64(want.R)) + math.Abs(float64(got.G)-float64(want.G)) + math.Abs(float64(got.B)-float64(want.B))
			pixels++
		}
	}
	// The watermark is ~50% white (error ~150/channel); a correct inpaint of a
	// linear gradient stays within a few levels of the true background.
	if mean := totalError / float64(pixels*3); mean > 6 {
		t.Fatalf("mean absolute error in repaired region = %.2f, want <= 6", mean)
	}
}

func TestRemoveWatermarkLeavesSmallImagesUnchanged(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 256, 256))
	var input bytes.Buffer
	if err := png.Encode(&input, source); err != nil {
		t.Fatal(err)
	}
	output, width, height, err := RemoveWatermark(input.Bytes())
	if err != nil || width != 256 || height != 256 || !bytes.Equal(output, input.Bytes()) {
		t.Fatalf("small image was modified: %d/%d/%v", width, height, err)
	}
}
