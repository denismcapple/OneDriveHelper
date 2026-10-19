package popup

import (
	"errors"
	"image"
	"image/color"
	"unsafe"
)

// ForceDPI, if set before Start, renders at this DPI instead of the
// system's (used to make crisp documentation screenshots).
var ForceDPI int32

var pGetDCForSnap = user32.NewProc("GetDC")

// Snapshot renders a card off-screen exactly as it would appear and returns
// it as an image. Nothing is shown on screen.
func (m *Manager) Snapshot(s Spec) (*image.RGBA, error) {
	if m == nil {
		return nil, errors.New("no UI")
	}
	var img *image.RGBA
	var err error
	m.doSync(func() { img, err = m.snapshot(s) })
	return img, err
}

func (m *Manager) snapshot(s Spec) (*image.RGBA, error) {
	c := &card{m: m, spec: s, hover: -1}
	c.layout()
	w, h := int(c.w), int(c.h)
	screen := call(pGetDCForSnap, 0)
	defer call(pReleaseDC, 0, screen)
	dc := call(pCreateCompatibleDC, screen)
	defer call(pDeleteDC, dc)
	bih := bitmapInfoHeader{Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32}
	bih.Size = uint32(unsafe.Sizeof(bih))
	var bits unsafe.Pointer
	bmp := call(pCreateDIBSection, screen, ptr(&bih), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == nil {
		return nil, errors.New("CreateDIBSection failed")
	}
	defer call(pDeleteObject, bmp)
	old := call(pSelectObject, dc, bmp)
	c.paint(dc)
	call(pSelectObject, dc, old)

	px := unsafe.Slice((*uint32)(bits), w*h)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i, v := range px {
		img.Pix[i*4+0] = byte(v >> 16)
		img.Pix[i*4+1] = byte(v >> 8)
		img.Pix[i*4+2] = byte(v)
		img.Pix[i*4+3] = 0xFF
	}
	return img, nil
}

// IconImage returns the tray icon for a severity at the given size.
func IconImage(sev Severity, size int) *image.RGBA {
	px := iconPixels(sev, size)
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for i, v := range px {
		a := byte(v >> 24)
		img.SetRGBA(i%size, i/size, color.RGBA{
			R: byte(uint32(byte(v>>16)) * uint32(a) / 255), G: byte(uint32(byte(v>>8)) * uint32(a) / 255),
			B: byte(uint32(byte(v)) * uint32(a) / 255), A: a,
		})
	}
	return img
}
