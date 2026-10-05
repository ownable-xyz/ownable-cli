// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// Package imgout writes and reads the raw RGBA frames the renderers produce.
package imgout

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
)

// SHA256 is the hex sha256 of a raw RGBA buffer.
func SHA256(rgba []byte) string {
	h := sha256.Sum256(rgba)
	return hex.EncodeToString(h[:])
}

// EncodePNG encodes top-down, non-premultiplied RGBA as a PNG.
func EncodePNG(rgba []byte, w, h int) ([]byte, error) {
	if len(rgba) != w*h*4 {
		return nil, fmt.Errorf("imgout: %d bytes for %dx%d", len(rgba), w, h)
	}
	img := &image.NRGBA{Pix: rgba, Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WritePNG writes a PNG file.
func WritePNG(path string, rgba []byte, w, h int) error {
	b, err := EncodePNG(rgba, w, h)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// ReadPNG decodes a PNG file to top-down, non-premultiplied RGBA.
func ReadPNG(path string) ([]byte, int, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, 0, err
	}
	pix, w, h, err := DecodePNG(b)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%s: %w", path, err)
	}
	return pix, w, h, nil
}

// DecodePNG decodes PNG bytes to top-down, non-premultiplied RGBA.
func DecodePNG(data []byte) ([]byte, int, int, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, err
	}
	b := img.Bounds()
	if n, ok := img.(*image.NRGBA); ok && n.Stride == b.Dx()*4 && b.Min == (image.Point{}) {
		return n.Pix, b.Dx(), b.Dy(), nil
	}
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	if _, opaque := img.(*image.RGBA); opaque || isOpaqueModel(img.ColorModel()) {
		draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	} else {
		for y := 0; y < b.Dy(); y++ {
			for x := 0; x < b.Dx(); x++ {
				out.Set(x, y, color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)))
			}
		}
	}
	return out.Pix, b.Dx(), b.Dy(), nil
}

func isOpaqueModel(m color.Model) bool {
	return m == color.GrayModel || m == color.Gray16Model || m == color.YCbCrModel
}

// Diff compares two RGBA frames of the same size and returns how many pixels
// differ and the largest per-channel difference.
func Diff(a, b []byte) (pixels int, maxDelta int) {
	for i := 0; i+3 < len(a) && i+3 < len(b); i += 4 {
		d := 0
		for c := 0; c < 4; c++ {
			v := int(a[i+c]) - int(b[i+c])
			if v < 0 {
				v = -v
			}
			d = max(d, v)
		}
		if d > 0 {
			pixels++
			maxDelta = max(maxDelta, d)
		}
	}
	return pixels, maxDelta
}
