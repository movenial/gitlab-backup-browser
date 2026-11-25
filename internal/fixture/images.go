package fixture

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// A real image, so that the demo shows the issue attachment.

// blurryLogoPNG is the attachment of "Logo is blurry on retina": a round
// logo with a soft, out-of-focus edge.
func blurryLogoPNG() []byte {
	const w, h = 420, 140
	return drawPNG(w, h, func(x, y int) color.RGBA {
		d := math.Hypot(float64(x-w/2), float64(y-h/2))
		// 1 inside the logo, 0 outside, with a wide blurred ring.
		in := 1 / (1 + math.Exp((d-44)/6))
		bg := [3]float64{245, 243, 238}
		fg := [3]float64{226, 67, 41}
		var c [3]uint8
		for i := range c {
			c[i] = uint8(bg[i] + (fg[i]-bg[i])*in)
		}
		return color.RGBA{c[0], c[1], c[2], 255}
	})
}

func drawPNG(w, h int, at func(x, y int) color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, at(x, y))
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err)
	}
	return b.Bytes()
}
