// Command screenshots regenerates the images in docs/images using made-up
// data (AcmeCorp, Fabrikam, Contoso). Cards are rendered off-screen and
// dialogs are captured from their own window contents, so nothing else on
// the desktop ends up in the pictures.
//
//	go run ./tools/screenshots -out docs/images -config-exe dist/amd64/OneDriveHelperConfig.exe
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/agent"
	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
	"github.com/denismcapple/OneDriveHelper/internal/remediate"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	gdi32            = windows.NewLazySystemDLL("gdi32.dll")
	dwmapi           = windows.NewLazySystemDLL("dwmapi.dll")
	pFindWindowW     = user32.NewProc("FindWindowW")
	pGetWindowRect   = user32.NewProc("GetWindowRect")
	pPrintWindow     = user32.NewProc("PrintWindow")
	pPostMessageW    = user32.NewProc("PostMessageW")
	pGetDC           = user32.NewProc("GetDC")
	pReleaseDC       = user32.NewProc("ReleaseDC")
	pCreateCompatDC  = gdi32.NewProc("CreateCompatibleDC")
	pCreateDIB       = gdi32.NewProc("CreateDIBSection")
	pSelectObject    = gdi32.NewProc("SelectObject")
	pDeleteObject    = gdi32.NewProc("DeleteObject")
	pDeleteDC        = gdi32.NewProc("DeleteDC")
	pDwmGetWindowAtt = dwmapi.NewProc("DwmGetWindowAttribute")
)

type rect struct{ L, T, R, B int32 }

var (
	pGetClientRect  = user32.NewProc("GetClientRect")
	pClientToScreen = user32.NewProc("ClientToScreen")
)

func main() {
	out := flag.String("out", "docs/images", "output folder")
	cfgExe := flag.String("config-exe", "dist/amd64/OneDriveHelperConfig.exe", "built settings tool (for its screenshot)")
	logo := flag.String("logo", "", "PNG logo to show on the cards (optional)")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	winapi.EnableVisualStyles()
	popup.ForceDPI = 144          // 1.5x for crisp images
	remediate.PreviewLinks = true // the made-up folders don't exist, but show their paths as links
	ui, err := popup.Start()
	if err != nil {
		log.Fatal(err)
	}
	cfg := agent.DemoConfig()
	if *logo != "" {
		abs, err := filepath.Abs(*logo)
		if err != nil {
			log.Fatal(err)
		}
		cfg.LogoFile = abs
	}

	for name, spec := range agent.DocSpecs(cfg) {
		img, err := ui.Snapshot(spec)
		if err != nil {
			log.Fatalf("%s: %v", name, err)
		}
		save(filepath.Join(*out, name+".png"), decorate(img, 12))
	}

	// Tray icons, side by side.
	icons := image.NewRGBA(image.Rect(0, 0, 3*64+2*24, 64))
	for i, sev := range []popup.Severity{popup.OK, popup.Warning, popup.Critical} {
		draw.Draw(icons, image.Rect(i*88, 0, i*88+64, 64), popup.IconImage(sev, 64), image.Point{}, draw.Over)
	}
	save(filepath.Join(*out, "tray-icons.png"), icons)

	// "What does this mean?" card.
	img, err := ui.Snapshot(popup.Spec{Severity: popup.Info, Brand: cfg.Title(), Accent: cfg.AccentColor, Logo: cfg.LogoPath(),
		Title: "What does this mean?", Body: agent.Explain(cfg, "orphans", agent.DemoOrphans()), Buttons: []popup.Button{{Label: "OK", Primary: true}}})
	if err != nil {
		log.Fatalf("explanation: %v", err)
	}
	save(filepath.Join(*out, "what-does-this-mean.png"), decorate(img, 12))

	// Settings tool with a made-up settings file loaded.
	tmp := filepath.Join(os.TempDir(), "acmecorp-settings.json")
	if err := config.WriteFile(cfg, tmp); err != nil {
		log.Fatal(err)
	}
	cmd := exec.Command(*cfgExe, "--open", tmp)
	if err := cmd.Start(); err != nil {
		log.Printf("settings tool: %v", err)
	} else {
		if h := waitWindow("OneDriveHelperConfig", ""); h != 0 {
			time.Sleep(3 * time.Second) // let every control finish painting
			save(filepath.Join(*out, "settings-tool.png"), decorate(capture(h), 8))
		}
		_ = cmd.Process.Kill()
	}
	os.Remove(tmp)
	if cfg.LogoFile != "" {
		os.Remove(filepath.Join(os.TempDir(), config.LogoName)) // copied next to the settings file
	}
	fmt.Println("screenshots written to", *out)
}

func waitWindow(class, title string) uintptr {
	c, _ := windows.UTF16PtrFromString(class)
	var t *uint16
	if title != "" {
		t, _ = windows.UTF16PtrFromString(title)
	}
	for i := 0; i < 50; i++ {
		if h, _, _ := pFindWindowW.Call(uintptr(unsafe.Pointer(c)), uintptr(unsafe.Pointer(t))); h != 0 {
			time.Sleep(700 * time.Millisecond) // let it finish drawing
			return h
		}
		time.Sleep(200 * time.Millisecond)
	}
	return 0
}

// capture renders a window's own contents (no desktop, no overlapping
// windows, no watermark) and crops the invisible resize border. The frame
// comes from DWM (modern title bar); the inside is painted by the window
// itself, because DWM's copy can be missing controls that haven't been
// composed yet.
func capture(h uintptr) *image.RGBA {
	var wr, vis rect
	pGetWindowRect.Call(h, uintptr(unsafe.Pointer(&wr)))
	vis = wr
	pDwmGetWindowAtt.Call(h, 9, uintptr(unsafe.Pointer(&vis)), unsafe.Sizeof(vis)) // DWMWA_EXTENDED_FRAME_BOUNDS
	full := printWindow(h, int(wr.R-wr.L), int(wr.B-wr.T), 2)                      // PW_RENDERFULLCONTENT
	var cr rect
	pGetClientRect.Call(h, uintptr(unsafe.Pointer(&cr)))
	var org struct{ X, Y int32 }
	pClientToScreen.Call(h, uintptr(unsafe.Pointer(&org)))
	client := printWindow(h, int(cr.R), int(cr.B), 1) // PW_CLIENTONLY
	off := image.Pt(int(org.X-wr.L), int(org.Y-wr.T))
	draw.Draw(full, client.Bounds().Add(off), client, image.Point{}, draw.Src)
	crop := image.Rect(int(vis.L-wr.L), int(vis.T-wr.T), int(vis.R-wr.L), int(vis.B-wr.T))
	return full.SubImage(crop).(*image.RGBA)
}

func printWindow(h uintptr, w, ht int, flags uintptr) *image.RGBA {
	screen, _, _ := pGetDC.Call(0)
	defer pReleaseDC.Call(0, screen)
	dc, _, _ := pCreateCompatDC.Call(screen)
	defer pDeleteDC.Call(dc)
	type bih struct {
		Size                   uint32
		Width, Height          int32
		Planes, BitCount       uint16
		Compression, SizeImage uint32
		XPels, YPels           int32
		ClrUsed, ClrImportant  uint32
	}
	hdr := bih{Width: int32(w), Height: -int32(ht), Planes: 1, BitCount: 32}
	hdr.Size = uint32(unsafe.Sizeof(hdr))
	var bits unsafe.Pointer
	bmp, _, _ := pCreateDIB.Call(screen, uintptr(unsafe.Pointer(&hdr)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	defer pDeleteObject.Call(bmp)
	old, _, _ := pSelectObject.Call(dc, bmp)
	pPrintWindow.Call(h, dc, flags)
	pSelectObject.Call(dc, old)
	px := unsafe.Slice((*uint32)(bits), w*ht)
	img := image.NewRGBA(image.Rect(0, 0, w, ht))
	for i, v := range px {
		img.Pix[i*4], img.Pix[i*4+1], img.Pix[i*4+2], img.Pix[i*4+3] = byte(v>>16), byte(v>>8), byte(v), 0xFF
	}
	return img
}

// decorate rounds the corners and adds a soft drop shadow on a transparent
// background, so the image looks right on both light and dark GitHub themes.
func decorate(src *image.RGBA, radius int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	m := 28
	out := image.NewRGBA(image.Rect(0, 0, w+2*m, h+2*m))
	inside := func(x, y float64) float64 { // coverage of the rounded rect at (x,y) in card coords
		r := float64(radius)
		cx := math.Max(r, math.Min(float64(w)-r, x))
		cy := math.Max(r, math.Min(float64(h)-r, y))
		return math.Max(0, math.Min(1, r-math.Hypot(x-cx, y-cy)+0.5))
	}
	for y := 0; y < h+2*m; y++ {
		for x := 0; x < w+2*m; x++ {
			// shadow: distance outside the card, offset 6px down
			fx, fy := float64(x-m)+0.5, float64(y-m-6)+0.5
			dx := math.Max(0, math.Max(-fx, fx-float64(w)))
			dy := math.Max(0, math.Max(-fy, fy-float64(h)))
			a := 70 * math.Exp(-math.Hypot(dx, dy)/9)
			out.SetRGBA(x, y, color.RGBA{A: uint8(a)})
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			cov := inside(float64(x)+0.5, float64(y)+0.5)
			if cov == 0 {
				continue
			}
			s := src.RGBAAt(b.Min.X+x, b.Min.Y+y)
			d := out.RGBAAt(x+m, y+m)
			mix := func(sc, dc uint8) uint8 { return uint8(float64(sc)*cov + float64(dc)*(1-cov)) }
			out.SetRGBA(x+m, y+m, color.RGBA{mix(s.R, d.R), mix(s.G, d.G), mix(s.B, d.B), mix(255, d.A)})
		}
	}
	return out
}

func save(path string, img image.Image) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	b := img.Bounds()
	fmt.Printf("%s (%dx%d)\n", path, b.Dx(), b.Dy())
}
