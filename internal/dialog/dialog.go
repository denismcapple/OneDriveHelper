// Package dialog shows OneDrive Helper's messages and questions as its own
// card popups (internal/popup) instead of Windows message boxes, branded
// with the configured company name, accent colour and logo.
//
// Every function blocks until the user answers, so call them from button
// actions or other goroutines, never from the popup UI thread. If no card
// can be shown (no desktop), a plain Windows message box is used instead.
package dialog

import (
	"strings"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// Kind selects the header colour.
type Kind int

const (
	KindInfo    Kind = iota // accent colour
	KindSuccess             // green
	KindWarning             // amber
	KindError               // red
)

// Message is a card with a heading, text and buttons.
type Message struct {
	Brand   string // header text; default: the configured title ("<Company> - OneDrive Helper")
	Heading string
	Text    string
	Kind    Kind
	Buttons []string // default: OK
	Primary int      // index of the accent-coloured button; -1 = none
	Copy    bool     // add a "Copy text" button (stays open) for passing details to support
	Help    func()   // "What does this mean?" link in the header
	Items   []popup.Item
	Footer  string // text after the items
	Wide    bool   // a wider card (see popup.Spec.Wide)
}

// Show displays m and returns the index of the button clicked (-1 if the
// card timed out or could not be shown).
func Show(m Message) int {
	i, _ := ShowWithin(m, 0)
	return i
}

// ShowWithin is Show with a time limit (0 = none).
func ShowWithin(m Message, limit time.Duration) (int, bool) {
	cfg := config.Load()
	m = m.withDefaults(cfg)
	ui, err := popup.Shared()
	if err != nil {
		return fallback(m, limit)
	}
	s := Spec(m, cfg)
	if m.Copy {
		text := m.Heading + "\r\n\r\n" + strings.ReplaceAll(strings.TrimSpace(m.PlainText()), "\n", "\r\n")
		s.ID = "dialog-" + time.Now().Format("150405.000000000")
		id := s.ID
		s.Buttons[0].Action = func() {
			if ui.CopyText(text) == nil {
				ui.SetButtonLabel(id, 0, "Copied \u2713")
			}
		}
	}
	i, timedOut := ui.AskWithin(s, limit)
	if m.Copy && i >= 0 {
		i-- // the Copy button comes first
	}
	return i, timedOut
}

// PlainText is the whole message as text: for "Copy text" and for the
// message-box fallback, which can't show items.
func (m Message) PlainText() string {
	var b strings.Builder
	b.WriteString(m.Text)
	for _, it := range m.Items {
		if it.Heading {
			b.WriteString("\n")
		}
		b.WriteString("\n" + it.Title)
		if it.Path != "" {
			b.WriteString("\n    " + it.Path)
		}
		if it.Note != "" {
			b.WriteString("\n" + it.Note)
		}
	}
	if m.Footer != "" {
		b.WriteString("\n\n" + m.Footer)
	}
	return strings.TrimSpace(b.String())
}

func (m Message) withDefaults(cfg config.Config) Message {
	if m.Brand == "" {
		m.Brand = cfg.Title()
	}
	if len(m.Buttons) == 0 {
		m.Buttons, m.Primary = []string{"OK"}, 0
	}
	return m
}

// Spec is the card m is shown as (also used for previews and screenshots).
// With Copy, the first button is "Copy text".
func Spec(m Message, cfg config.Config) popup.Spec {
	m = m.withDefaults(cfg)
	s := popup.Spec{
		Severity: map[Kind]popup.Severity{KindInfo: popup.Info, KindSuccess: popup.OK, KindWarning: popup.Warning, KindError: popup.Critical}[m.Kind],
		Brand:    m.Brand, Title: m.Heading, Body: strings.TrimSpace(m.Text), Items: m.Items, Footer: m.Footer, Wide: m.Wide,
		Accent: cfg.AccentColor, Logo: cfg.LogoPath(), OnHelp: m.Help,
		Sound: cfg.PopupSound && (m.Kind == KindWarning || m.Kind == KindError),
	}
	if m.Copy {
		s.Buttons = append(s.Buttons, popup.Button{Label: "Copy text", Stay: true})
	}
	for i, b := range m.Buttons {
		s.Buttons = append(s.Buttons, popup.Button{Label: b, Primary: i == m.Primary})
	}
	return s
}

// fallback shows m as a Windows message box (OK, or OK/Cancel for two buttons).
func fallback(m Message, limit time.Duration) (int, bool) {
	text := m.PlainText()
	if m.Heading != "" {
		text = m.Heading + "\n\n" + text
	}
	if len(m.Buttons) < 2 {
		switch m.Kind {
		case KindWarning:
			winapi.Warn(m.Brand, text)
		case KindError:
			winapi.Error(m.Brand, text)
		default:
			winapi.Info(m.Brand, text)
		}
		return 0, false
	}
	// Two or more choices: OK picks the primary (action) button, Cancel the first.
	ok, timedOut := false, false
	if limit > 0 {
		ok, timedOut = winapi.ConfirmWithin(m.Brand, text, limit)
	} else {
		ok = winapi.Confirm(m.Brand, text)
	}
	switch {
	case timedOut:
		return -1, true
	case ok && m.Primary >= 0:
		return m.Primary, false
	}
	return 0, false
}

// Info shows information with an OK button.
func Info(heading, text string) { Show(Message{Heading: heading, Text: text, Kind: KindInfo}) }

// Success shows good news with an OK button.
func Success(heading, text string) { Show(Message{Heading: heading, Text: text, Kind: KindSuccess}) }

// Warn shows a warning with OK and "Copy text".
func Warn(heading, text string) {
	Show(Message{Heading: heading, Text: text, Kind: KindWarning, Copy: true})
}

// Error shows an error with OK and "Copy text".
func Error(heading, text string) {
	Show(Message{Heading: heading, Text: text, Kind: KindError, Copy: true})
}

// Confirm asks a question with "Cancel" and yes (the accent button). It
// returns true only if yes was clicked.
func Confirm(heading, text, yes string) bool {
	ok, _ := ConfirmWithin(heading, text, yes, 0)
	return ok
}

// ConfirmWithin is Confirm with a time limit (0 = none); after it the card
// closes and counts as "not confirmed" (timedOut is set).
func ConfirmWithin(heading, text, yes string, limit time.Duration) (ok, timedOut bool) {
	i, timedOut := ShowWithin(Message{Heading: heading, Text: text, Kind: KindWarning, Buttons: []string{"Cancel", yes}, Primary: 1}, limit)
	return i == 1, timedOut
}

// WithBrand returns helpers that use brand in the header (e.g. the settings tool).
type WithBrand string

func (b WithBrand) Info(heading, text string) {
	Show(Message{Brand: string(b), Heading: heading, Text: text, Kind: KindInfo})
}

func (b WithBrand) Warn(heading, text string) {
	Show(Message{Brand: string(b), Heading: heading, Text: text, Kind: KindWarning, Copy: true})
}

func (b WithBrand) Error(heading, text string) {
	Show(Message{Brand: string(b), Heading: heading, Text: text, Kind: KindError, Copy: true})
}

func (b WithBrand) Confirm(heading, text, yes string) bool {
	return Show(Message{Brand: string(b), Heading: heading, Text: text, Kind: KindWarning, Buttons: []string{"Cancel", yes}, Primary: 1}) == 1
}
