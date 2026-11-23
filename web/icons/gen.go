//go:build ignore

// Command gen draws SD2NAS's PNG app icons into web/public. Run it from the
// repo root after changing the design:  go run web/icons/gen.go
// The shapes match web/public/icon.svg.
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

var (
	bg    = color.NRGBA{0x14, 0x1c, 0x27, 0xff}
	card  = color.NRGBA{0x4f, 0x9c, 0xf9, 0xff}
	arrow = color.NRGBA{0xe6, 0xed, 0xf6, 0xff}
)

type pt struct{ x, y float64 }

// Card geometry in a unit square.
const (
	w, h   = 0.44, 0.56
	x0, y0 = 0.5 - w/2, 0.5 - h/2
	x1, y1 = x0 + w, y0 + h
	cut    = 0.3 * w
)

// shade is the colour at (x, y) in the unit square; ok is false where the
// icon is transparent (outside the rounded corners).
func shade(x, y float64, rounded bool) (c color.NRGBA, ok bool) {
	if rounded && !inRoundedSquare(x, y, 0.22) {
		return color.NRGBA{}, false
	}
	if !inConvex(x, y, []pt{{x0, y0}, {x1 - cut, y0}, {x1, y0 + cut}, {x1, y1}, {x0, y1}}) {
		return bg, true
	}
	s := (x1 - cut - 0.05*w - (x0 + 0.1*w)) / 5.5 // contact width; gaps are half a contact
	for i := 0; i < 4; i++ {
		sx := x0 + 0.1*w + float64(i)*1.5*s
		if x >= sx && x < sx+s && y >= y0+0.08*h && y < y0+0.28*h {
			return bg, true
		}
	}
	cx := (x0 + x1) / 2
	if x >= cx-0.06*w && x < cx+0.06*w && y >= y0+0.42*h && y < y0+0.66*h {
		return arrow, true
	}
	if inConvex(x, y, []pt{{cx - 0.22*w, y0 + 0.62*h}, {cx + 0.22*w, y0 + 0.62*h}, {cx, y0 + 0.86*h}}) {
		return arrow, true
	}
	return card, true
}

func inConvex(x, y float64, poly []pt) bool {
	var pos, neg bool
	for i := range poly {
		a, b := poly[i], poly[(i+1)%len(poly)]
		cross := (b.x-a.x)*(y-a.y) - (b.y-a.y)*(x-a.x)
		if cross > 0 {
			pos = true
		} else if cross < 0 {
			neg = true
		}
		if pos && neg {
			return false
		}
	}
	return true
}

func inRoundedSquare(x, y, r float64) bool {
	dx := math.Max(math.Max(r-x, x-(1-r)), 0)
	dy := math.Max(math.Max(r-y, y-(1-r)), 0)
	return dx*dx+dy*dy <= r*r
}

// render draws the icon at size px with 4×4 supersampling.
func render(size int, rounded bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, n float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) / float64(size)
					y := (float64(py) + (float64(sy)+0.5)/ss) / float64(size)
					c, ok := shade(x, y, rounded)
					if !ok {
						continue
					}
					r, g, b, n = r+float64(c.R), g+float64(c.G), b+float64(c.B), n+1
				}
			}
			if n == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{uint8(r/n + 0.5), uint8(g/n + 0.5), uint8(b/n + 0.5), uint8(n/(ss*ss)*255 + 0.5)})
		}
	}
	return img
}

func main() {
	dir := filepath.Join("web", "public")
	if _, err := os.Stat(dir); err != nil {
		log.Fatalf("run from the repo root: %v", err)
	}
	for _, icon := range []struct {
		name    string
		size    int
		rounded bool
	}{
		{"icon-192.png", 192, true},
		{"icon-512.png", 512, true},
		{"icon-maskable-512.png", 512, false}, // launchers apply their own mask
		{"apple-touch-icon.png", 180, false},  // iOS rounds it; transparency would turn black
	} {
		f, err := os.Create(filepath.Join(dir, icon.name))
		if err != nil {
			log.Fatal(err)
		}
		if err := png.Encode(f, render(icon.size, icon.rounded)); err != nil {
			f.Close()
			log.Fatal(err)
		}
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
	}
}
