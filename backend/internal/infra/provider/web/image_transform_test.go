package web

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestWebImage2KTransformActivation(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		resolution string
		want       bool
	}{
		{name: "ordinary 1k", model: "grok-imagine-image-2.0", resolution: "1k"},
		{name: "explicit 2k", model: "grok-imagine-image-2.0", resolution: "2k", want: true},
		{name: "derived model", model: "grok-imagine-image-2.0-2k", want: true},
		{name: "prefixed derived model", model: "Web/grok-imagine-image-2.0-2k", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := webImage2KTransform(test.model, test.resolution, "1:1").enabled; got != test.want {
				t.Fatalf("enabled=%v, want %v", got, test.want)
			}
		})
	}
}

func TestWebImage2KTargetDimensions(t *testing.T) {
	tests := []struct {
		ratio      string
		source     image.Rectangle
		wantWidth  int
		wantHeight int
	}{
		{ratio: "1:1", source: image.Rect(0, 0, 960, 960), wantWidth: 2048, wantHeight: 2048},
		{ratio: "2:3", source: image.Rect(0, 0, 640, 960), wantWidth: 1672, wantHeight: 2508},
		{ratio: "3:2", source: image.Rect(0, 0, 960, 640), wantWidth: 2508, wantHeight: 1672},
		{ratio: "16:9", source: image.Rect(0, 0, 960, 540), wantWidth: 2731, wantHeight: 1536},
		{ratio: "auto", source: image.Rect(0, 0, 600, 900), wantWidth: 1672, wantHeight: 2508},
	}
	for _, test := range tests {
		t.Run(test.ratio, func(t *testing.T) {
			transform := imageOutputTransform{enabled: true, aspectRatio: test.ratio}
			width, height, err := transform.targetDimensions(test.source)
			if err != nil {
				t.Fatal(err)
			}
			if width != test.wantWidth || height != test.wantHeight {
				t.Fatalf("dimensions=%dx%d, want %dx%d", width, height, test.wantWidth, test.wantHeight)
			}
		})
	}
}

func TestTransformWebImageOutputDisabledPreservesBytes(t *testing.T) {
	raw := []byte("not an image, but disabled transforms must not inspect it")
	got, width, height, err := transformWebImageOutput(raw, imageOutputTransform{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) || width != 0 || height != 0 {
		t.Fatalf("disabled transform changed output: dimensions=%dx%d", width, height)
	}
}

func TestTransformWebImageOutputProducesReal2KJPEG(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 24, 24))
	fillTestImage(source)
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, source, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	raw, width, height, err := transformWebImageOutput(encoded.Bytes(), imageOutputTransform{enabled: true, aspectRatio: "1:1"})
	if err != nil {
		t.Fatal(err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" || width != 2048 || height != 2048 || decoded.Bounds().Dx() != 2048 || decoded.Bounds().Dy() != 2048 {
		t.Fatalf("format=%s reported=%dx%d decoded=%dx%d", format, width, height, decoded.Bounds().Dx(), decoded.Bounds().Dy())
	}
}

func TestTransformWebImageOutputPreservesPNGFormat(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 16, 24))
	fillTestImage(source)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	raw, width, height, err := transformWebImageOutput(encoded.Bytes(), imageOutputTransform{enabled: true, aspectRatio: "2:3"})
	if err != nil {
		t.Fatal(err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if format != "png" || width != 1672 || height != 2508 || decoded.Bounds().Dx() != 1672 || decoded.Bounds().Dy() != 2508 {
		t.Fatalf("format=%s reported=%dx%d decoded=%dx%d", format, width, height, decoded.Bounds().Dx(), decoded.Bounds().Dy())
	}
}

func TestTransformWebImageOutputRejectsInvalidImage(t *testing.T) {
	if _, _, _, err := transformWebImageOutput([]byte("invalid"), imageOutputTransform{enabled: true, aspectRatio: "1:1"}); err == nil {
		t.Fatal("expected invalid image error")
	}
}

func fillTestImage(target *image.NRGBA) {
	for y := target.Bounds().Min.Y; y < target.Bounds().Max.Y; y++ {
		for x := target.Bounds().Min.X; x < target.Bounds().Max.X; x++ {
			target.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 7), G: uint8(y * 5), B: 120, A: 255})
		}
	}
}
