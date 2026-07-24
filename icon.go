package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"
)

// The menu bar glyph: a little CRT with two bars on its screen, echoing the panel's session and
// weekly gauges.
//
// ponytail: drawn in code rather than shipped as a PNG asset. The shape is a handful of rounded
// rectangles, so the generator is smaller than the binary it would replace and the proportions stay
// editable — tweak the constants below and run `just icon` to look at the result.
//
// Geometry is written in a 44-unit design space (see the iconSize comment) so the numbers read as
// coordinates on the finished glyph regardless of what it is finally rasterised to.
const (
	iconSize   = 44 // 22pt at 2x; Wails rescales the NSImage to the menu bar thickness anyway
	iconSuper  = 4  // supersampling factor — the only anti-aliasing this needs
	iconDesign = 44.0
)

var crtIcon = sync.OnceValue(func() []byte {
	hi := iconSize * iconSuper
	scale := float64(hi) / iconDesign
	ink := make([]bool, hi*hi)

	// paint fills (or clears) a rounded rectangle given in design-space coordinates.
	paint := func(x0, y0, x1, y1, radius float64, set bool) {
		for py := range hi {
			for px := range hi {
				x := (float64(px) + 0.5) / scale
				y := (float64(py) + 0.5) / scale
				if insideRoundRect(x, y, x0, y0, x1, y1, radius) {
					ink[py*hi+px] = set
				}
			}
		}
	}

	// Near-square body with a deep bezel and heavily rounded glass: the proportions are what make
	// this read as a CRT rather than a widescreen flat panel.
	paint(5, 4, 39, 30, 6.5, true)        // case
	paint(9.5, 8, 34.5, 26, 4.5, false)   // screen, knocked back out of it
	paint(13, 12.5, 31, 15, 1.25, true)   // session bar
	paint(13, 18.5, 24.5, 21, 1.25, true) // weekly bar, deliberately shorter
	paint(18, 30, 26, 33, 0, true)        // neck
	paint(10.5, 33, 33.5, 37, 1.5, true)  // base

	// Downsample the mask into the alpha channel. macOS template images ignore colour entirely and
	// render from alpha alone, so the pixels are black and only coverage matters.
	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	per := iconSuper * iconSuper
	for y := range iconSize {
		for x := range iconSize {
			covered := 0
			for sy := range iconSuper {
				for sx := range iconSuper {
					if ink[(y*iconSuper+sy)*hi+x*iconSuper+sx] {
						covered++
					}
				}
			}
			img.SetNRGBA(x, y, color.NRGBA{A: uint8(covered * 255 / per)})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic("vibemon: encoding the tray icon: " + err.Error()) // pure computation; cannot fail in practice
	}
	return buf.Bytes()
})

// insideRoundRect reports whether (x, y) falls within the rounded rectangle, by measuring the
// distance to the rectangle shrunk by the corner radius.
func insideRoundRect(x, y, x0, y0, x1, y1, radius float64) bool {
	cx := math.Min(math.Max(x, x0+radius), x1-radius)
	cy := math.Min(math.Max(y, y0+radius), y1-radius)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= radius*radius
}
