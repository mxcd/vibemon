package main

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"testing"
)

func decodeIcon(t *testing.T) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(crtIcon()))
	if err != nil {
		t.Fatalf("tray icon is not decodable PNG: %v", err)
	}
	return img
}

func TestCRTIconShape(t *testing.T) {
	img := decodeIcon(t)
	if b := img.Bounds(); b.Dx() != iconSize || b.Dy() != iconSize {
		t.Fatalf("icon must be square %dpx, got %dx%d", iconSize, b.Dx(), b.Dy())
	}

	alphaAt := func(x, y int) uint32 {
		_, _, _, a := img.At(x, y).RGBA()
		return a >> 8
	}

	// A template icon carries its shape purely in the alpha channel; a fully opaque or fully
	// transparent image would render as a block or as nothing at all.
	var opaque, clear int
	for y := range iconSize {
		for x := range iconSize {
			if a := alphaAt(x, y); a > 200 {
				opaque++
			} else if a < 40 {
				clear++
			}
		}
	}
	if opaque == 0 || clear == 0 {
		t.Fatalf("icon has no contrast: %d opaque, %d clear pixels", opaque, clear)
	}
	if opaque > iconSize*iconSize/2 {
		t.Errorf("icon is too solid (%d of %d px) — it should read as an outline", opaque, iconSize*iconSize)
	}

	// Spot-check the landmarks that make it a CRT rather than a blob.
	if alphaAt(0, 0) > 40 {
		t.Error("top-left corner should be empty padding")
	}
	if alphaAt(iconSize/2, 7) < 200 {
		t.Error("expected the top bezel across the middle column")
	}
	if alphaAt(iconSize/2, 17) > 40 {
		t.Error("expected hollow screen between the two bars")
	}
	if alphaAt(iconSize/2, 36) < 200 {
		t.Error("expected the stand base near the bottom")
	}
}

// Writing the icon out is how it actually gets judged — geometry assertions cannot tell you it
// looks right. `just icon` sets this and opens the result.
func TestCRTIconPreview(t *testing.T) {
	out := os.Getenv("VIBEMON_ICON_OUT")
	if out == "" {
		t.Skip("set VIBEMON_ICON_OUT to dump a preview PNG")
	}
	if err := os.WriteFile(out, crtIcon(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", out)
}
