package popup

import (
	"fmt"
	"unsafe"
)

func unsafeSizeof[T any](v T) uintptr { return unsafe.Sizeof(v) }

var (
	colorWhite     = rgb(0xFF, 0xFF, 0xFF)
	colorBorder    = rgb(0xC8, 0xC8, 0xC8)
	colorTitle     = rgb(0x1F, 0x1F, 0x1F)
	colorBody      = rgb(0x42, 0x42, 0x42)
	colorPath      = rgb(0x6B, 0x6B, 0x6B)
	colorPrimary   = rgb(0x0F, 0x6C, 0xBD)
	colorPrimaryHv = rgb(0x11, 0x5E, 0xA3)
	colorSecondary = rgb(0xF3, 0xF3, 0xF3)
	colorSecondHv  = rgb(0xE3, 0xE3, 0xE3)
)

func headerColor(s Spec) uintptr {
	switch s.Severity {
	case Critical:
		return rgb(0xC4, 0x2B, 0x1C)
	case Info:
		p, _ := accent(s.Accent)
		return p
	case OK:
		return rgb(0x10, 0x7C, 0x10)
	}
	return rgb(0xD9, 0x7A, 0x00)
}

// accent returns the primary colour and its hover shade for "#RRGGBB".
func accent(hex string) (uintptr, uintptr) {
	var r, g, b byte
	if n, err := fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b); err != nil || n != 3 {
		return colorPrimary, colorPrimaryHv
	}
	dark := func(v byte) byte { return byte(int(v) * 85 / 100) }
	return rgb(r, g, b), rgb(dark(r), dark(g), dark(b))
}

type card struct {
	m        *Manager
	hwnd     uintptr
	spec     Spec
	w, h     int32
	header   int32
	title    rect
	body     rect
	buttons  []rect
	help     rect // "What does this mean? (i)" link in the header (empty if no OnHelp)
	items    []itemRects
	footer   rect
	logo     rect
	logoPx   []uint32
	hover    int
	tracking bool
	// A card taller than the screen scrolls the part between the header and
	// the buttons (view) by scroll pixels, up to scrollMax.
	view              rect
	scroll, scrollMax int32
}

// helpHit is the hover/click index of the header help link.
const helpHit = -2

const helpLabel = "What does this mean?"

// itemRects holds the laid-out parts of one Item.
type itemRects struct{ title, path, note rect }

// layout measures text and positions everything for the current spec.
func (c *card) layout() {
	m := c.m
	pad := m.s(18)
	c.header = m.s(34)
	bh, gap, indent := m.s(32), m.s(8), m.s(16)

	dc := call(pCreateCompatibleDC, 0)
	defer call(pDeleteDC, dc)
	measure := func(font uintptr, text string, width int32, flags uint32) (int32, int32) {
		old := call(pSelectObject, dc, font)
		r := rect{Right: width}
		drawText(dc, text, &r, flags|dtCalcRect|dtNoPrefix)
		call(pSelectObject, dc, old)
		return r.Right - r.Left, r.Bottom - r.Top
	}

	// Width: at least 440, wide enough for every path on one line and for
	// all buttons in one row, but never wider than the screen allows.
	widths := make([]int32, len(c.spec.Buttons))
	var need int32
	for i, b := range c.spec.Buttons {
		w, _ := measure(m.fonts.button, buttonLabel(b), 2000, dtSingleLine)
		widths[i] = w + m.s(28)
		need += widths[i] + gap
	}
	need -= gap
	for _, it := range c.spec.Items {
		if it.Path != "" {
			w, _ := measure(m.fonts.small, it.Path, 4000, dtSingleLine)
			need = max(need, w+indent)
		}
	}
	wa := workArea()
	c.w = min(max(m.s(440), need+2*pad), max(m.s(440), (wa.Right-wa.Left)*3/4))
	inner := c.w - 2*pad

	c.help = rect{}
	if c.spec.OnHelp != nil {
		lw, _ := measure(m.fonts.brand, helpLabel, 1000, dtSingleLine)
		d := m.s(18)
		c.help = rect{Left: c.w - pad - d - m.s(6) - lw, Top: (c.header - d) / 2, Right: c.w - pad, Bottom: (c.header-d)/2 + d}
	}

	y := c.header + m.s(14)
	c.logo, c.logoPx = rect{}, nil
	if img := m.logo(c.spec.Logo); img != nil {
		px, lw, lh := fitLogo(img, min(inner, m.s(200)), m.s(40))
		c.logo, c.logoPx = rect{Left: pad, Top: y, Right: pad + lw, Bottom: y + lh}, px
		y += lh + m.s(12)
	}
	_, th := measure(m.fonts.title, c.spec.Title, inner, dtWordBreak)
	c.title = rect{Left: pad, Top: y, Right: pad + inner, Bottom: y + th}
	y += th + m.s(6)
	if c.spec.Body != "" {
		_, h := measure(m.fonts.body, c.spec.Body, inner, dtWordBreak|dtEditControl)
		c.body = rect{Left: pad, Top: y, Right: pad + inner, Bottom: y + h}
		y += h
	} else {
		c.body = rect{}
	}

	// Items: bold name, path in small grey on one line, then the note.
	c.items = make([]itemRects, len(c.spec.Items))
	for i, it := range c.spec.Items {
		y += m.s(12)
		var r itemRects
		_, h := measure(m.fonts.button, it.Title, inner, dtWordBreak|dtEditControl)
		r.title = rect{Left: pad, Top: y, Right: pad + inner, Bottom: y + h}
		y += h
		if it.Path != "" {
			_, h = measure(m.fonts.small, it.Path, inner-indent, dtSingleLine)
			r.path = rect{Left: pad + indent, Top: y + m.s(2), Right: pad + inner, Bottom: y + m.s(2) + h}
			y += h + m.s(4)
		}
		if it.Note != "" {
			_, h = measure(m.fonts.body, it.Note, inner-indent, dtWordBreak|dtEditControl)
			r.note = rect{Left: pad + indent, Top: y + m.s(2), Right: pad + inner, Bottom: y + m.s(2) + h}
			y += h + m.s(2)
		}
		c.items[i] = r
	}
	c.footer = rect{}
	if c.spec.Footer != "" {
		y += m.s(12)
		_, h := measure(m.fonts.body, c.spec.Footer, inner, dtWordBreak|dtEditControl)
		c.footer = rect{Left: pad, Top: y, Right: pad + inner, Bottom: y + h}
		y += h
	}

	contentEnd := y

	// Buttons: right-aligned in one row (the card was sized for that); they
	// only wrap if the screen is too narrow. The first button ends up left-most.
	c.buttons = make([]rect, len(c.spec.Buttons))
	if len(c.spec.Buttons) > 0 {
		y += m.s(18)
		for i := range widths {
			widths[i] = min(widths[i], inner)
		}
		var rows [][]int
		var cur []int
		var used int32
		for i := len(widths) - 1; i >= 0; i-- { // fill from the right
			need := widths[i]
			if len(cur) > 0 {
				need += gap
			}
			if len(cur) > 0 && used+need > inner {
				rows = append(rows, cur)
				cur, used, need = nil, 0, widths[i]
			}
			cur = append(cur, i)
			used += need
		}
		rows = append(rows, cur)
		for r := len(rows) - 1; r >= 0; r-- { // first-filled row goes at the bottom
			x := pad + inner
			for _, i := range rows[r] {
				c.buttons[i] = rect{Left: x - widths[i], Top: y, Right: x, Bottom: y + bh}
				x -= widths[i] + gap
			}
			y += bh + gap
		}
		y -= gap
	}
	c.h = y + pad

	// Taller than the screen: keep the header and buttons, scroll the rest.
	c.view, c.scrollMax = rect{}, 0
	if maxH := wa.Bottom - wa.Top - 2*m.s(16); maxH > c.header+m.s(120) && c.h > maxH {
		over := c.h - maxH
		c.h = maxH
		bottom := c.h - pad
		for i := range c.buttons {
			c.buttons[i].Top -= over
			c.buttons[i].Bottom -= over
			bottom = min(bottom, c.buttons[i].Top-m.s(12))
		}
		c.view = rect{Left: 0, Top: c.header, Right: c.w, Bottom: bottom}
		c.scrollMax = max(contentEnd+m.s(10)-bottom, 0)
	}
	c.scroll = min(max(c.scroll, 0), c.scrollMax)
}

// scrollBy scrolls the middle of a tall card (mouse wheel).
func (c *card) scrollBy(delta int32) {
	if c.scrollMax == 0 {
		return
	}
	if s := min(max(c.scroll+delta, 0), c.scrollMax); s != c.scroll {
		c.scroll = s
		call(pInvalidateRect, c.hwnd, 0, 0)
	}
}

func buttonLabel(b Button) string {
	if len(b.Menu) > 0 {
		return b.Label + "  \u25BE"
	}
	return b.Label
}

func (c *card) paint(target uintptr) {
	m := c.m
	dc := call(pCreateCompatibleDC, target)
	bmp := call(pCreateCompatibleBitmap, target, uintptr(c.w), uintptr(c.h))
	oldBmp := call(pSelectObject, dc, bmp)
	call(pSetBkMode, dc, bkTransparent)

	all := rect{Left: 0, Top: 0, Right: c.w, Bottom: c.h}
	fill(dc, all, colorWhite)
	header := headerColor(c.spec)
	fill(dc, rect{Left: 0, Top: 0, Right: c.w, Bottom: c.header}, header)

	// Header: a white "!" badge and the brand name.
	pad := m.s(18)
	d := m.s(18)
	badge := rect{Left: pad, Top: (c.header - d) / 2, Right: pad + d, Bottom: (c.header-d)/2 + d}
	ellipse(dc, badge, colorWhite)
	old := call(pSelectObject, dc, m.fonts.button)
	call(pSetTextColor, dc, header)
	mark := "!"
	switch c.spec.Severity {
	case Info:
		mark = "i"
	case OK:
		mark = "\u2713" // check mark
	}
	drawText(dc, mark, &badge, dtCenter|dtVCenter|dtSingleLine|dtNoPrefix)
	call(pSelectObject, dc, m.fonts.brand)
	call(pSetTextColor, dc, colorWhite)
	br := rect{Left: badge.Right + m.s(10), Top: 0, Right: c.w - pad, Bottom: c.header}
	if c.help.Right > 0 {
		br.Right = c.help.Left - m.s(10)
	}
	drawText(dc, c.spec.Brand, &br, dtVCenter|dtSingleLine|dtNoPrefix|dtEndEllipsis)

	// Header help link: "What does this mean?" followed by an outlined (i).
	if c.help.Right > 0 {
		h := c.help
		icon := rect{Left: h.Right - (h.Bottom - h.Top), Top: h.Top, Right: h.Right, Bottom: h.Bottom}
		fillIcon := header
		if c.hover == helpHit {
			fillIcon = colorWhite
		}
		circle(dc, icon, fillIcon, colorWhite, m.s(2))
		lbl := rect{Left: h.Left, Top: 0, Right: icon.Left - m.s(6), Bottom: c.header}
		drawText(dc, helpLabel, &lbl, dtVCenter|dtSingleLine|dtNoPrefix)
		if c.hover == helpHit { // underline the label on hover
			fill(dc, rect{Left: lbl.Left, Top: h.Bottom - m.s(1), Right: lbl.Right, Bottom: h.Bottom}, colorWhite)
		}
		call(pSelectObject, dc, m.fonts.button)
		if c.hover == helpHit {
			call(pSetTextColor, dc, header)
		}
		drawText(dc, "i", &icon, dtCenter|dtVCenter|dtSingleLine|dtNoPrefix)
	}

	// The middle part (logo, title, text, items, footer); it scrolls on tall cards.
	off := func(r rect) rect { r.Top -= c.scroll; r.Bottom -= c.scroll; return r }
	if c.scrollMax > 0 {
		call(pSaveDC, dc)
		call(pIntersectClipRect, dc, uintptr(c.view.Left), uintptr(c.view.Top), uintptr(c.view.Right), uintptr(c.view.Bottom))
	}
	if len(c.logoPx) > 0 {
		r := off(c.logo)
		drawPixels(dc, c.logoPx, r.Left, r.Top, r.Right-r.Left, r.Bottom-r.Top)
	}
	call(pSelectObject, dc, m.fonts.title)
	call(pSetTextColor, dc, colorTitle)
	t := off(c.title)
	drawText(dc, c.spec.Title, &t, dtWordBreak|dtNoPrefix)
	if c.spec.Body != "" {
		call(pSelectObject, dc, m.fonts.body)
		call(pSetTextColor, dc, colorBody)
		b := off(c.body)
		drawText(dc, c.spec.Body, &b, dtWordBreak|dtEditControl|dtNoPrefix)
	}
	for i, it := range c.spec.Items {
		r := c.items[i]
		r.title, r.path, r.note = off(r.title), off(r.path), off(r.note)
		call(pSelectObject, dc, m.fonts.button)
		call(pSetTextColor, dc, colorTitle)
		drawText(dc, it.Title, &r.title, dtWordBreak|dtEditControl|dtNoPrefix)
		if it.Path != "" {
			call(pSelectObject, dc, m.fonts.small)
			call(pSetTextColor, dc, colorPath)
			drawText(dc, it.Path, &r.path, dtSingleLine|dtNoPrefix|dtPathEllipsis)
		}
		if it.Note != "" {
			call(pSelectObject, dc, m.fonts.body)
			call(pSetTextColor, dc, colorBody)
			drawText(dc, it.Note, &r.note, dtWordBreak|dtEditControl|dtNoPrefix)
		}
	}
	if c.spec.Footer != "" {
		call(pSelectObject, dc, m.fonts.body)
		call(pSetTextColor, dc, colorBody)
		f := off(c.footer)
		drawText(dc, c.spec.Footer, &f, dtWordBreak|dtEditControl|dtNoPrefix)
	}
	if c.scrollMax > 0 {
		call(pRestoreDC, dc, ^uintptr(0)) // -1: the state saved above
		// A slim scroll bar on the right and a line above the buttons.
		v := c.view
		track := v.Bottom - v.Top - m.s(8)
		thumb := max(track*track/(track+c.scrollMax), m.s(24))
		top := v.Top + m.s(4) + (track-thumb)*c.scroll/c.scrollMax
		roundRect(dc, rect{Left: v.Right - m.s(7), Top: top, Right: v.Right - m.s(3), Bottom: top + thumb}, m.s(4), colorBorder, colorBorder)
		fill(dc, rect{Left: 0, Top: v.Bottom, Right: c.w, Bottom: v.Bottom + 1}, colorSecondHv)
	}

	call(pSelectObject, dc, m.fonts.button)
	primary, primaryHv := accent(c.spec.Accent)
	for i, b := range c.spec.Buttons {
		r := c.buttons[i]
		bg, border, fg := colorSecondary, colorBorder, colorTitle
		if b.Primary {
			bg, border, fg = primary, primary, colorWhite
			if c.hover == i {
				bg, border = primaryHv, primaryHv
			}
		} else if c.hover == i {
			bg = colorSecondHv
		}
		roundRect(dc, r, m.s(6), bg, border)
		call(pSetTextColor, dc, fg)
		drawText(dc, buttonLabel(b), &r, dtCenter|dtVCenter|dtSingleLine|dtNoPrefix)
	}
	call(pSelectObject, dc, old)

	// Outer border.
	bb := call(pCreateSolidBrush, colorBorder)
	call(pFrameRect, dc, ptr(&all), bb)
	call(pDeleteObject, bb)

	call(pBitBlt, target, 0, 0, uintptr(c.w), uintptr(c.h), dc, 0, 0, srcCopy)
	call(pSelectObject, dc, oldBmp)
	call(pDeleteObject, bmp)
	call(pDeleteDC, dc)
}

func (c *card) hit(lParam uintptr) int {
	x, y := int32(int16(lParam&0xFFFF)), int32(int16((lParam>>16)&0xFFFF))
	if c.help.Right > 0 && c.help.Contains(x, y) {
		return helpHit
	}
	for i, r := range c.buttons {
		if r.Contains(x, y) {
			return i
		}
	}
	return -1
}

func (c *card) setHover(i int) {
	if i != c.hover {
		c.hover = i
		call(pInvalidateRect, c.hwnd, 0, 0)
	}
}

func (c *card) click(i int) {
	if i == helpHit {
		if f := c.spec.OnHelp; f != nil {
			go safeRun(f)
		}
		return
	}
	b := c.spec.Buttons[i]
	id := c.spec.ID
	if len(b.Menu) > 0 {
		menu := call(pCreatePopupMenu)
		for j, it := range b.Menu {
			call(pAppendMenuW, menu, 0, uintptr(j+1), ptr(u16(it.Label)))
		}
		pt := point{X: c.buttons[i].Left, Y: c.buttons[i].Top}
		call(pClientToScreen, c.hwnd, ptr(&pt))
		call(pSetForegroundWindow, c.hwnd) // required so the menu closes when clicking elsewhere
		cmd := int(call(pTrackPopupMenu, menu, tpmReturnCmd|tpmNoNotify|tpmBottomAlign, uintptr(pt.X), uintptr(pt.Y), 0, c.hwnd, 0))
		call(pPostMessageW, c.hwnd, wmNull, 0, 0)
		call(pDestroyMenu, menu)
		c.m.rewake()
		if cmd >= 1 && cmd <= len(b.Menu) {
			if b.Dismiss {
				c.m.setVisible(id, false)
				c.m.hide(id)
			}
			if a := b.Menu[cmd-1].Action; a != nil {
				go safeRun(a)
			}
		}
		return
	}
	if b.Dismiss {
		c.m.setVisible(id, false)
		c.m.hide(id) // c is invalid after this
	}
	if b.Action != nil {
		go safeRun(b.Action)
	}
}
