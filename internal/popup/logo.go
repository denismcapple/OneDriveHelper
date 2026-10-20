package popup

import (
	"errors"
	"image"
	"image/draw"
	"image/png"
	"math"
	"os"
	"time"
	"unsafe"
)

// Logo limits: anything bigger is refused (not drawn).
const (
	MaxLogoBytes  = 2 << 20
	MaxLogoWidth  = 2000
	MaxLogoHeight = 1000
)

type logoEntry struct {
	mod  time.Time
	size int64
	img  *image.RGBA // composited over white
}

// LoadLogo decodes a PNG logo within the size limits, composited over white
// (cards are white where the logo is drawn).
func LoadLogo(path string) (*image.RGBA, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > MaxLogoBytes {
		return nil, errors.New("the logo file is larger than 2 MB")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		return nil, errors.New("the logo is not a valid PNG image")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > MaxLogoWidth || cfg.Height > MaxLogoHeight {
		return nil, errors.New("the logo must be at most 2000 x 1000 pixels")
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	src, err := png.Decode(f)
	if err != nil {
		return nil, errors.New("the logo is not a valid PNG image")
	}
	b := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(out, out.Bounds(), src, b.Min, draw.Over)
	return out, nil
}

// logo returns the (cached) decoded logo, or nil if there is none or it
// can't be used. UI thread only.
func (m *Manager) logo(path string) *image.RGBA {
	if path == "" {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if e, ok := m.logos[path]; ok && e.mod.Equal(fi.ModTime()) && e.size == fi.Size() {
		return e.img
	}
	img, err := LoadLogo(path)
	if err != nil {
		img = nil
	}
	if m.logos == nil {
		m.logos = map[string]logoEntry{}
	}
	m.logos[path] = logoEntry{mod: fi.ModTime(), size: fi.Size(), img: img}
	return img
}

// fitLogo scales img to fit maxW x maxH (keeping its shape, never more than
// twice its size) and returns the pixels as 0x00RRGGBB.
func fitLogo(img *image.RGBA, maxW, maxH int32) ([]uint32, int32, int32) {
	sw, sh := float64(img.Bounds().Dx()), float64(img.Bounds().Dy())
	scale := math.Min(math.Min(float64(maxW)/sw, float64(maxH)/sh), 2)
	w, h := int32(math.Max(1, math.Round(sw*scale))), int32(math.Max(1, math.Round(sh*scale)))
	// Average n x n bilinear samples per pixel: smooth when shrinking, soft when enlarging.
	n := int(math.Max(1, math.Ceil(1/scale)))
	px := make([]uint32, int(w)*int(h))
	at := func(x, y int) (float64, float64, float64) {
		x = min(max(x, 0), int(sw)-1)
		y = min(max(y, 0), int(sh)-1)
		i := img.PixOffset(x, y)
		return float64(img.Pix[i]), float64(img.Pix[i+1]), float64(img.Pix[i+2])
	}
	for y := 0; y < int(h); y++ {
		for x := 0; x < int(w); x++ {
			var r, g, b float64
			for sy := 0; sy < n; sy++ {
				for sx := 0; sx < n; sx++ {
					fx := (float64(x)+(float64(sx)+0.5)/float64(n))/scale - 0.5
					fy := (float64(y)+(float64(sy)+0.5)/float64(n))/scale - 0.5
					x0, y0 := int(math.Floor(fx)), int(math.Floor(fy))
					ax, ay := fx-float64(x0), fy-float64(y0)
					r00, g00, b00 := at(x0, y0)
					r10, g10, b10 := at(x0+1, y0)
					r01, g01, b01 := at(x0, y0+1)
					r11, g11, b11 := at(x0+1, y0+1)
					lerp := func(p00, p10, p01, p11 float64) float64 {
						return (p00*(1-ax)+p10*ax)*(1-ay) + (p01*(1-ax)+p11*ax)*ay
					}
					r += lerp(r00, r10, r01, r11)
					g += lerp(g00, g10, g01, g11)
					b += lerp(b00, b10, b01, b11)
				}
			}
			k := float64(n * n)
			px[y*int(w)+x] = uint32(r/k+0.5)<<16 | uint32(g/k+0.5)<<8 | uint32(b/k+0.5)
		}
	}
	return px, w, h
}

// drawPixels copies 0x00RRGGBB pixels to the DC at (x, y).
func drawPixels(dc uintptr, px []uint32, x, y, w, h int32) {
	if len(px) == 0 {
		return
	}
	bih := bitmapInfoHeader{Width: w, Height: -h, Planes: 1, BitCount: 32}
	bih.Size = uint32(unsafe.Sizeof(bih))
	var bits unsafe.Pointer
	bmp := call(pCreateDIBSection, dc, ptr(&bih), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == nil {
		return
	}
	defer call(pDeleteObject, bmp)
	copy(unsafe.Slice((*uint32)(bits), len(px)), px)
	src := call(pCreateCompatibleDC, dc)
	defer call(pDeleteDC, src)
	old := call(pSelectObject, src, bmp)
	call(pBitBlt, dc, uintptr(x), uintptr(y), uintptr(w), uintptr(h), src, 0, 0, srcCopy)
	call(pSelectObject, src, old)
}
