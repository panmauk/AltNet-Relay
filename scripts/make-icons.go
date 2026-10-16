//go:build ignore

// make-icons.go renders the AltNet Relay app icon. Go standard library
// only, no network, deterministic (same code in, byte-identical files out).
//
// Run from the repo root (or use scripts/make-icons.sh):
//
//	GOPROXY=off go run scripts/make-icons.go [-preview sheet.png]
//
// Writes:
//
//	app/desktop/build/appicon.png        1024x1024 master (Wails reads this)
//	app/desktop/build/windows/icon.ico   16/24/32/48/64/128/256, embedded in
//	                                     AltNetRelay.exe and used by the installer
//	scripts/altnet_relay_icon.svg        vector rendition of the 1024 design
//
// The mark: a speech bubble drawn as one light (#e8f0ff) outline on the
// dark navy (#060a18) PANMOX tile, with a single glowing cyan (#00c8ff)
// dot inside it -- the same dot as the in-app wordmark. Studio's icon is a
// cyan mesh globe around a glowing centre node; Relay keeps the tile, the
// corner radius and the glowing node, and swaps the mesh for a bubble.
// Cyan stays a signal, not decoration: exactly one cyan thing on the icon.
//
// The PNGs are rendered here from signed distance fields (analytic
// anti-aliasing), not from the SVG. 16, 24 and 32 px have their own
// hand-placed geometry so strokes land on the pixel grid and stay legible
// in the title bar and taskbar; 48 px and up are the 1024 design scaled.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Palette (matches app/desktop/frontend/src/style.css).
var (
	navy  = rgb{0x06, 0x0a, 0x18} // --bg
	cyan  = rgb{0x00, 0xc8, 0xff} // --accent
	light = rgb{0xe8, 0xf0, 0xff} // --text
)

type rgb struct{ r, g, b uint8 }

type pt struct{ x, y float64 }

// geom is the icon's geometry in pixels for one output size.
type geom struct {
	size float64 // canvas edge

	tileR   float64 // tile corner radius (Studio: 130/680 of the edge)
	borderW float64 // hairline just inside the tile edge ...
	borderA float64 // ... in cyan at this opacity (the app's --border)

	x0, y0, x1, y1 float64 // bubble body, stroke centre-line
	r              float64 // bubble corner radius, centre-line
	stroke         float64
	tailA, tailB   float64 // x where the tail leaves / rejoins the bottom edge
	tip            pt      // tail tip

	dot       pt
	dotR      float64
	glowSigma float64 // gaussian falloff outside the dot
	glowA     float64 // glow opacity at the dot's edge
}

// design is the master geometry on a 1024 canvas.
func design() geom {
	return geom{
		size:    1024,
		tileR:   196,
		borderW: 4, borderA: 0.12,
		x0: 236, y0: 266, x1: 788, y1: 658,
		r:      132,
		stroke: 44,
		tailA:  392, tailB: 512,
		tip:       pt{338, 784},
		dot:       pt{512, 462},
		dotR:      58,
		glowSigma: 40, glowA: 0.32,
	}
}

func (g geom) scaled(size float64) geom {
	k := size / g.size
	s := g
	s.size = size
	s.tileR *= k
	s.borderW = math.Max(1, g.borderW*k)
	s.x0, s.y0, s.x1, s.y1 = g.x0*k, g.y0*k, g.x1*k, g.y1*k
	s.r *= k
	s.stroke *= k
	s.tailA *= k
	s.tailB *= k
	s.tip = pt{g.tip.x * k, g.tip.y * k}
	s.dot = pt{g.dot.x * k, g.dot.y * k}
	s.dotR *= k
	s.glowSigma *= k
	return s
}

// geomFor returns the geometry for one output size. The small sizes are
// hinted by hand: pixel (i,j) covers [i,i+1]x[j,j+1], so a 1 px stroke is
// crisp when its centre-line sits on .5, and a 2 px stroke when on .0.
func geomFor(size int) geom {
	switch size {
	// The tile hairline is left off below 48 px: at 1 px it reads as a
	// teal frame rather than an edge. The tail is hinted to a vertical left
	// edge and a 45-degree return so it stays a crisp shape, not a smudge.
	case 16:
		return geom{
			size: 16, tileR: 3,
			// 1 px outline: rows 2 and 11, columns 2 and 13.
			x0: 2.5, y0: 2.5, x1: 13.5, y1: 11.5, r: 2, stroke: 1,
			tailA: 4.5, tailB: 6.5, tip: pt{4.5, 13.5},
			// 2x2 dot centred in the 8-row interior.
			dot: pt{8, 7}, dotR: 1.25, glowSigma: 0.7, glowA: 0.22,
		}
	case 24:
		return geom{
			size: 24, tileR: 4.5,
			// 1.5 px outline with a crisp outer edge (3 and 21).
			x0: 3.75, y0: 3.75, x1: 20.25, y1: 16.25, r: 3, stroke: 1.5,
			tailA: 6.75, tailB: 10.25, tip: pt{6.75, 19.75},
			dot: pt{12, 10}, dotR: 1.6, glowSigma: 1.0, glowA: 0.26,
		}
	case 32:
		return geom{
			size: 32, tileR: 6,
			// 2 px outline, crisp on both edges.
			x0: 5, y0: 5, x1: 27, y1: 21, r: 5, stroke: 2,
			tailA: 10, tailB: 15, tip: pt{10, 26},
			dot: pt{16, 13}, dotR: 2.1, glowSigma: 1.3, glowA: 0.28,
		}
	}
	return design().scaled(float64(size))
}

// bubble returns the bubble outline as a closed polygon (arcs flattened),
// clockwise on screen, with the tail spliced into the bottom edge.
func bubble(g geom) []pt {
	var p []pt
	n := int(math.Max(8, math.Ceil(g.r*1.5)))
	arc := func(cx, cy, a0, a1 float64) {
		for i := 0; i <= n; i++ {
			a := a0 + (a1-a0)*float64(i)/float64(n)
			p = append(p, pt{cx + g.r*math.Cos(a), cy + g.r*math.Sin(a)})
		}
	}
	arc(g.x0+g.r, g.y0+g.r, math.Pi, 1.5*math.Pi)   // top-left
	arc(g.x1-g.r, g.y0+g.r, 1.5*math.Pi, 2*math.Pi) // top-right
	arc(g.x1-g.r, g.y1-g.r, 0, 0.5*math.Pi)         // bottom-right
	p = append(p, pt{g.tailB, g.y1}, g.tip, pt{g.tailA, g.y1})
	arc(g.x0+g.r, g.y1-g.r, 0.5*math.Pi, math.Pi) // bottom-left
	return p
}

// sdPoly is the signed distance from q to a closed polygon (negative inside).
func sdPoly(q pt, poly []pt) float64 {
	best := math.Inf(1)
	inside := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		a, b := poly[j], poly[i]
		dx, dy := b.x-a.x, b.y-a.y
		t := 0.0
		if l := dx*dx + dy*dy; l > 0 {
			t = math.Max(0, math.Min(1, ((q.x-a.x)*dx+(q.y-a.y)*dy)/l))
		}
		ex, ey := q.x-(a.x+t*dx), q.y-(a.y+t*dy)
		best = math.Min(best, ex*ex+ey*ey)
		if (a.y > q.y) != (b.y > q.y) && q.x < a.x+(q.y-a.y)*dx/dy {
			inside = !inside
		}
	}
	d := math.Sqrt(best)
	if inside {
		return -d
	}
	return d
}

// sdRoundBox is the signed distance to the tile [0,s]x[0,s] with corner radius r.
func sdRoundBox(q pt, s, r float64) float64 {
	h := s / 2
	qx := math.Abs(q.x-h) - (h - r)
	qy := math.Abs(q.y-h) - (h - r)
	out := math.Hypot(math.Max(qx, 0), math.Max(qy, 0))
	return out + math.Min(math.Max(qx, qy), 0) - r
}

func cov(sd float64) float64 { return math.Max(0, math.Min(1, 0.5-sd)) }

// Compositing is done in linear light: light-on-dark anti-aliased edges
// keep their weight instead of going grey and thin at 16 px.
func toLin(c uint8) float64 {
	v := float64(c) / 255
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

func toSRGB(v float64) uint8 {
	v = math.Max(0, math.Min(1, v))
	if v <= 0.0031308 {
		v *= 12.92
	} else {
		v = 1.055*math.Pow(v, 1/2.4) - 0.055
	}
	return uint8(math.Round(v * 255))
}

type px struct{ r, g, b, a float64 } // premultiplied, linear

func (d *px) over(c rgb, a float64) {
	if a <= 0 {
		return
	}
	d.r = toLin(c.r)*a + d.r*(1-a)
	d.g = toLin(c.g)*a + d.g*(1-a)
	d.b = toLin(c.b)*a + d.b*(1-a)
	d.a = a + d.a*(1-a)
}

func render(g geom) *image.NRGBA {
	n := int(g.size)
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	poly := bubble(g)
	// Only pixels near the outline need the (slow) polygon distance.
	reach := g.stroke/2 + 1.5
	minX, maxX := g.x0-reach, g.x1+reach
	minY, maxY := g.y0-reach, math.Max(g.y1, g.tip.y)+reach
	for j := 0; j < n; j++ {
		for i := 0; i < n; i++ {
			q := pt{float64(i) + 0.5, float64(j) + 0.5}
			var d px

			sdTile := sdRoundBox(q, g.size, g.tileR)
			tile := cov(sdTile)
			d.over(navy, tile)
			d.over(cyan, g.borderA*(tile-cov(sdTile+g.borderW)))

			dd := math.Hypot(q.x-g.dot.x, q.y-g.dot.y) - g.dotR
			if dd > 0 {
				e := dd / g.glowSigma
				d.over(cyan, tile*g.glowA*math.Exp(-e*e))
			}

			if q.x >= minX && q.x <= maxX && q.y >= minY && q.y <= maxY {
				sd := sdPoly(q, poly)
				d.over(light, tile*cov(math.Abs(sd)-g.stroke/2))
			}

			d.over(cyan, tile*cov(dd))

			if d.a > 0 {
				img.SetNRGBA(i, j, color.NRGBA{
					toSRGB(d.r / d.a), toSRGB(d.g / d.a), toSRGB(d.b / d.a),
					uint8(math.Round(math.Min(1, d.a) * 255)),
				})
			}
		}
	}
	return img
}

func encodePNG(img image.Image) []byte {
	var b bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&b, img); err != nil {
		log.Fatal(err)
	}
	return b.Bytes()
}

// dib encodes img as the 32-bit BMP (BITMAPINFOHEADER + XOR + AND mask)
// an ICO entry expects: bottom-up rows, straight alpha, doubled height.
func dib(img *image.NRGBA) []byte {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	maskRow := (w + 31) / 32 * 4
	var b bytes.Buffer
	le := binary.LittleEndian
	for _, v := range []any{
		uint32(40), int32(w), int32(2 * h), uint16(1), uint16(32),
		uint32(0), uint32(w*h*4 + maskRow*h), int32(0), int32(0), uint32(0), uint32(0),
	} {
		binary.Write(&b, le, v)
	}
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			c := img.NRGBAAt(x, y)
			b.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	for y := h - 1; y >= 0; y-- {
		row := make([]byte, maskRow)
		for x := 0; x < w; x++ {
			if img.NRGBAAt(x, y).A == 0 {
				row[x/8] |= 0x80 >> (x % 8)
			}
		}
		b.Write(row)
	}
	return b.Bytes()
}

// ico builds a multi-resolution .ico: BMP entries below 256 px (what
// every Windows icon loader understands) and a PNG entry at 256.
func ico(imgs []*image.NRGBA) []byte {
	var hdr, data bytes.Buffer
	le := binary.LittleEndian
	binary.Write(&hdr, le, []uint16{0, 1, uint16(len(imgs))})
	offset := 6 + 16*len(imgs)
	for _, img := range imgs {
		s := img.Bounds().Dx()
		var blob []byte
		if s >= 256 {
			blob = encodePNG(img)
		} else {
			blob = dib(img)
		}
		dim := uint8(s)
		if s >= 256 {
			dim = 0
		}
		hdr.Write([]byte{dim, dim, 0, 0})
		binary.Write(&hdr, le, []uint16{1, 32})
		binary.Write(&hdr, le, []uint32{uint32(len(blob)), uint32(offset)})
		data.Write(blob)
		offset += len(blob)
	}
	return append(hdr.Bytes(), data.Bytes()...)
}

func svg(g geom) string {
	f := func(v float64) string {
		return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.2f", v), "0"), ".0")
	}
	r := f(g.r)
	path := fmt.Sprintf("M%s %s A%s %s 0 0 1 %s %s H%s A%s %s 0 0 1 %s %s V%s A%s %s 0 0 1 %s %s H%s L%s %s L%s %s H%s A%s %s 0 0 1 %s %s Z",
		f(g.x0), f(g.y0+g.r), r, r, f(g.x0+g.r), f(g.y0),
		f(g.x1-g.r), r, r, f(g.x1), f(g.y0+g.r),
		f(g.y1-g.r), r, r, f(g.x1-g.r), f(g.y1),
		f(g.tailB), f(g.tip.x), f(g.tip.y), f(g.tailA), f(g.y1),
		f(g.x0+g.r), r, r, f(g.x0), f(g.y1-g.r))
	s, hb := f(g.size), f(g.borderW/2)
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%[1]s" height="%[1]s" viewBox="0 0 %[1]s %[1]s">
  <title>AltNet Relay icon</title>
  <desc>Generated by scripts/make-icons.go (the PNG/ICO files are rendered by that script, not from this file). A light speech-bubble outline with one glowing cyan dot on the PANMOX navy tile.</desc>
  <defs>
    <filter id="glow" x="-150%%" y="-150%%" width="400%%" height="400%%">
      <feGaussianBlur stdDeviation="%[2]s"/>
    </filter>
  </defs>
  <rect width="%[1]s" height="%[1]s" rx="%[3]s" fill="#060a18"/>
  <rect x="%[4]s" y="%[4]s" width="%[5]s" height="%[5]s" rx="%[6]s" fill="none" stroke="#00c8ff" stroke-opacity="%[7]s" stroke-width="%[8]s"/>
  <circle cx="%[9]s" cy="%[10]s" r="%[11]s" fill="#00c8ff" opacity="%[12]s" filter="url(#glow)"/>
  <path d="%[13]s" fill="none" stroke="#e8f0ff" stroke-width="%[14]s" stroke-linejoin="round"/>
  <circle cx="%[9]s" cy="%[10]s" r="%[11]s" fill="#00c8ff"/>
</svg>
`, s, f(g.glowSigma*0.7), f(g.tileR), hb, f(g.size-g.borderW), f(g.tileR-g.borderW/2),
		f(g.borderA), f(g.borderW), f(g.dot.x), f(g.dot.y), f(g.dotR*1.25), f(g.glowA),
		path, f(g.stroke))
}

// preview writes a contact sheet: every size at 1:1 and magnified
// (nearest neighbour), on a dark and on a light backdrop.
func preview(path string, imgs []*image.NRGBA) {
	const pad = 16
	w, h := pad, 0
	for _, img := range imgs {
		s := img.Bounds().Dx()
		z := zoomFor(s)
		w += s*z + pad
		h = max(h, s*z)
	}
	rowH := h + 2*pad + 300
	sheet := image.NewNRGBA(image.Rect(0, 0, w, 2*rowH))
	for row, bg := range []color.NRGBA{{0x20, 0x20, 0x20, 255}, {0xf3, 0xf3, 0xf3, 255}} {
		for y := row * rowH; y < (row+1)*rowH; y++ {
			for x := 0; x < w; x++ {
				sheet.SetNRGBA(x, y, bg)
			}
		}
		x := pad
		for _, img := range imgs {
			s := img.Bounds().Dx()
			z := zoomFor(s)
			blit(sheet, img, x, row*rowH+pad, z)
			blit(sheet, img, x, row*rowH+2*pad+h, 1)
			x += s*z + pad
		}
	}
	if err := os.WriteFile(path, encodePNG(sheet), 0o644); err != nil {
		log.Fatal(err)
	}
}

func zoomFor(s int) int {
	if s <= 64 {
		return 128 / s * 2
	}
	return 1
}

func blit(dst *image.NRGBA, src *image.NRGBA, ox, oy, z int) {
	b := src.Bounds()
	for y := 0; y < b.Dy()*z; y++ {
		for x := 0; x < b.Dx()*z; x++ {
			s := src.NRGBAAt(x/z, y/z)
			d := dst.NRGBAAt(ox+x, oy+y)
			a := float64(s.A) / 255
			mix := func(p, q uint8) uint8 { return uint8(math.Round(float64(p)*a + float64(q)*(1-a))) }
			dst.SetNRGBA(ox+x, oy+y, color.NRGBA{mix(s.R, d.R), mix(s.G, d.G), mix(s.B, d.B), 255})
		}
	}
}

func main() {
	root := flag.String("root", ".", "AltNet Relay repo root")
	prev := flag.String("preview", "", "also write a contact sheet PNG here")
	flag.Parse()

	desk := filepath.Join(*root, "app", "desktop")
	if _, err := os.Stat(filepath.Join(desk, "wails.json")); err != nil {
		log.Fatalf("%s is not the AltNet Relay repo root (no app/desktop/wails.json); run from the root or pass -root", *root)
	}
	if err := os.MkdirAll(filepath.Join(desk, "build", "windows"), 0o755); err != nil {
		log.Fatal(err)
	}

	sizes := []int{16, 24, 32, 48, 64, 128, 256}
	var imgs []*image.NRGBA
	for _, s := range sizes {
		imgs = append(imgs, render(geomFor(s)))
	}
	master := render(design())

	write := func(p string, b []byte) {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  %-44s %7d bytes\n", filepath.ToSlash(p), len(b))
	}
	write(filepath.Join(desk, "build", "appicon.png"), encodePNG(master))
	write(filepath.Join(desk, "build", "windows", "icon.ico"), ico(imgs))
	write(filepath.Join(*root, "scripts", "altnet_relay_icon.svg"), []byte(svg(design())))
	if *prev != "" {
		preview(*prev, imgs)
		fmt.Printf("  preview: %s\n", *prev)
	}
}
