// Package config loads settings from the registry and from JSON files.
//
// Values are read from HKLM\SOFTWARE\Policies\OneDriveHelper first (GPO/Intune),
// then HKLM\SOFTWARE\OneDriveHelper, then fall back to built-in defaults.
// Integers and booleans are REG_DWORD, strings are REG_SZ / REG_EXPAND_SZ.
// JSON files use the same names as the registry values.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

const (
	KeyPath    = `SOFTWARE\OneDriveHelper`
	PolicyPath = `SOFTWARE\Policies\OneDriveHelper`
	product    = "OneDrive Helper"
)

// Config holds all settings. Tags: reg = registry/JSON name, def = default,
// label/help = shown in the config tool, min/max = allowed range for numbers,
// kind=multiline = multi-line text.
type Config struct {
	// Branding & support
	CompanyName        string `reg:"CompanyName" def:"" label:"Company name" help:"Shown in the title of every popup and dialog, e.g. 'Contoso'."`
	SupportName        string `reg:"SupportName" def:"your IT support team" label:"Support team name" help:"Used in 'Please contact ...', e.g. 'the Contoso Service Desk'."`
	SupportEmail       string `reg:"SupportEmail" def:"" label:"Support e-mail" help:"Shown in alerts; 'Contact support' opens an e-mail to it if no support web page is set."`
	SupportPhone       string `reg:"SupportPhone" def:"" label:"Support phone" help:"Shown in alerts."`
	SupportURL         string `reg:"SupportURL" def:"" label:"Support web page" help:"Opened by 'Contact support' (https://, http:// or mailto:)."`
	SupportEmailMethod string `reg:"SupportEmailMethod" def:"auto" label:"How to start support e-mails" help:"auto = open a draft with the full report (small monospace text) in the user's mail program, or a simple mailto: link with the report on the clipboard if there is no desktop mail program. draft / link force one of these."`
	AccentColor        string `reg:"AccentColor" def:"#0F6CBD" label:"Accent colour" help:"Colour of the main buttons, as #RRGGBB."`

	// Alert text (blank = built-in wording)
	NotRunningMessage string `reg:"NotRunningMessage" def:"" label:"'OneDrive not running' text" kind:"multiline" help:"Replaces the body of the 'OneDrive is not running' popup."`
	OrphanMessage     string `reg:"OrphanMessage" def:"" label:"'Library not syncing' text" kind:"multiline" help:"Replaces the explanation in the 'library stopped syncing' popup (the folder list and support contact are always shown)."`

	// OneDrive running check
	OneDriveCheckEnabled        bool   `reg:"OneDriveCheckEnabled" def:"1" label:"Alert when OneDrive is not running" help:"Alert when OneDrive.exe is not running for a user with OneDrive set up."`
	OneDriveMissingGraceSeconds int    `reg:"OneDriveMissingGraceSeconds" def:"120" min:"0" max:"3600" label:"Wait before alerting (seconds)" help:"How long OneDrive must be missing before alerting (covers updates/restarts)."`
	IgnoreOptions               string `reg:"IgnoreOptions" def:"5m,30m,reboot" label:"'Ignore' choices" help:"Choices in every alert's Ignore menu (each alert is ignored separately). Comma-separated: durations like 5m, 30m, 2h, and/or 'reboot' (until next restart). Empty = no Ignore button."`

	// Library sync check
	OrphanCheckEnabled bool   `reg:"OrphanCheckEnabled" def:"1" label:"Alert when a library stops syncing" help:"Alert when a library folder exists on disk but is no longer synced."`
	RemediationEnabled bool   `reg:"RemediationEnabled" def:"1" label:"Offer 'Fix (admin)'" help:"Offer the admin-approved fix for libraries that stopped syncing."`
	BackupRoot         string `reg:"BackupRoot" def:"{Profile}\\ODHelper\\Saved" label:"Save files to" help:"Where the fix saves files; {Profile} = the user's profile folder. Each run creates <this>\\<date-time>\\<Library>."`
	FreeSpaceMarginMB  int    `reg:"FreeSpaceMarginMB" def:"1024" min:"0" max:"1048576" label:"Extra free space required (MB)" help:"Free space required on top of the data being saved."`

	// General
	TrayIconEnabled      bool `reg:"TrayIconEnabled" def:"1" label:"Show a tray icon" help:"Status icon in the notification area: click for what needs fixing, right-click for actions and diagnostic details for support."`
	PopupSound           bool `reg:"PopupSound" def:"1" label:"Play a sound when a popup appears" help:"Play the Windows warning sound when an alert popup appears."`
	CheckIntervalSeconds int  `reg:"CheckIntervalSeconds" def:"60" min:"10" max:"3600" label:"Check every (seconds)" help:"How often the user agent runs its checks."`
	AgentWatchdogSeconds int  `reg:"AgentWatchdogSeconds" def:"30" min:"5" max:"3600" label:"Service watchdog (seconds)" help:"How often the service makes sure each user's agent is running."`
}

func (c Config) CheckInterval() time.Duration { return secs(c.CheckIntervalSeconds, 10) }
func (c Config) OneDriveGrace() time.Duration { return secs(c.OneDriveMissingGraceSeconds, 0) }
func (c Config) Watchdog() time.Duration      { return secs(c.AgentWatchdogSeconds, 5) }

// Title is the window/popup title: "<Company> - OneDrive Helper".
func (c Config) Title() string {
	if n := strings.TrimSpace(c.CompanyName); n != "" {
		return n + " - " + product
	}
	return product
}

// BackupDir resolves BackupRoot for a user profile.
func (c Config) BackupDir(profile string) string {
	r := c.BackupRoot
	if i := strings.Index(strings.ToLower(r), "{profile}"); i >= 0 {
		r = r[:i] + profile + r[i+len("{profile}"):]
	}
	return filepath.Clean(r)
}

// SupportContact renders "Please contact X (phone / email)."
func (c Config) SupportContact() string {
	var via []string
	if c.SupportPhone != "" {
		via = append(via, c.SupportPhone)
	}
	if c.SupportEmail != "" {
		via = append(via, c.SupportEmail)
	}
	s := "Please contact " + c.SupportName
	if len(via) > 0 {
		s += " (" + strings.Join(via, " / ") + ")"
	}
	return s + "."
}

// SupportEmailAddress is the support e-mail (from SupportEmail, or a mailto: SupportURL).
func (c Config) SupportEmailAddress() string {
	if c.SupportEmail != "" {
		return c.SupportEmail
	}
	if strings.HasPrefix(strings.ToLower(c.SupportURL), "mailto:") {
		addr, _, _ := strings.Cut(c.SupportURL[len("mailto:"):], "?")
		return addr
	}
	return ""
}

// SupportWebsite is the http(s) SupportURL, or "".
func (c Config) SupportWebsite() string {
	if l := strings.ToLower(c.SupportURL); strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
		return c.SupportURL
	}
	return ""
}

// SupportLink returns what "Contact support" should open, or "".
func (c Config) SupportLink() string {
	if validLink(c.SupportURL) {
		return c.SupportURL
	}
	if c.SupportEmail != "" {
		return "mailto:" + c.SupportEmail
	}
	return ""
}

func validLink(u string) bool {
	l := strings.ToLower(u)
	return strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "mailto:")
}

// IgnoreChoices returns the configured snooze tokens ("5m", "2h", "reboot").
func (c Config) IgnoreChoices() []string {
	var out []string
	for _, t := range strings.Split(c.IgnoreOptions, ",") {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			if _, err := ParseIgnore(t); err == nil {
				out = append(out, t)
			}
		}
	}
	return out
}

// ParseIgnore validates a snooze token; 0 means "until next restart".
func ParseIgnore(t string) (time.Duration, error) {
	if t == "reboot" {
		return 0, nil
	}
	d, err := time.ParseDuration(t)
	if err != nil || d < time.Minute || d > 24*time.Hour {
		return 0, fmt.Errorf("%q is not 'reboot' or a duration between 1m and 24h", t)
	}
	return d, nil
}

// IgnoreLabel is the menu text for a snooze token.
func IgnoreLabel(t string) string {
	d, err := ParseIgnore(t)
	switch {
	case err != nil:
		return t
	case t == "reboot":
		return "Ignore until next restart"
	case d%time.Hour == 0:
		return plural("Ignore for %d hour", int(d/time.Hour))
	default:
		return plural("Ignore for %d minute", int(d/time.Minute))
	}
}

func plural(f string, n int) string {
	s := fmt.Sprintf(f, n)
	if n != 1 {
		s += "s"
	}
	return s
}

var hexColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// Validate checks every setting and returns all problems found.
func (c Config) Validate() error {
	var errs []error
	v := reflect.ValueOf(c)
	for i := 0; i < v.NumField(); i++ {
		sf, f := v.Type().Field(i), v.Field(i)
		if f.Kind() != reflect.Int {
			continue
		}
		lo, _ := strconv.Atoi(sf.Tag.Get("min"))
		hi, err := strconv.Atoi(sf.Tag.Get("max"))
		if n := int(f.Int()); err == nil && (n < lo || n > hi) {
			errs = append(errs, fmt.Errorf("%s must be between %d and %d (got %d)", sf.Name, lo, hi, n))
		}
	}
	if len(c.CompanyName) > 60 {
		errs = append(errs, errors.New("CompanyName must be 60 characters or fewer"))
	}
	if strings.TrimSpace(c.SupportName) == "" {
		errs = append(errs, errors.New("SupportName must not be empty"))
	}
	if e := c.SupportEmail; e != "" && (!strings.Contains(e, "@") || strings.ContainsAny(e, " \t<>\"")) {
		errs = append(errs, fmt.Errorf("SupportEmail %q is not a valid e-mail address", e))
	}
	if c.SupportURL != "" && !validLink(c.SupportURL) {
		errs = append(errs, fmt.Errorf("SupportURL must start with https://, http:// or mailto: (got %q)", c.SupportURL))
	}
	switch strings.ToLower(c.SupportEmailMethod) {
	case "auto", "draft", "link":
	default:
		errs = append(errs, fmt.Errorf("SupportEmailMethod must be auto, draft or link (got %q)", c.SupportEmailMethod))
	}
	if c.AccentColor != "" && !hexColor.MatchString(c.AccentColor) {
		errs = append(errs, fmt.Errorf("AccentColor must look like #0F6CBD (got %q)", c.AccentColor))
	}
	n := 0
	for _, t := range strings.Split(c.IgnoreOptions, ",") {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			n++
			if _, err := ParseIgnore(t); err != nil {
				errs = append(errs, fmt.Errorf("IgnoreOptions: %w", err))
			}
		}
	}
	if n > 5 {
		errs = append(errs, errors.New("IgnoreOptions allows at most 5 choices"))
	}
	if b := c.BackupDir(`C:\Users\user`); strings.TrimSpace(c.BackupRoot) == "" || !filepath.IsAbs(b) || len(b) <= 3 {
		errs = append(errs, fmt.Errorf("BackupRoot must be an absolute folder (or start with {Profile}), got %q", c.BackupRoot))
	}
	return errors.Join(errs...)
}

func secs(v, min int) time.Duration {
	if v < min {
		v = min
	}
	return time.Duration(v) * time.Second
}

func mins(v int) time.Duration {
	if v < 1 {
		v = 1
	}
	return time.Duration(v) * time.Minute
}

// Field describes one setting (for the config tool and "config show").
type Field struct {
	Name, Label, Help, Kind string
	Type                    reflect.Kind
}

// Fields lists all settings in declaration order.
func Fields() []Field {
	t := reflect.TypeOf(Config{})
	out := make([]Field, t.NumField())
	for i := range out {
		sf := t.Field(i)
		out[i] = Field{Name: sf.Name, Label: sf.Tag.Get("label"), Help: sf.Tag.Get("help"), Kind: sf.Tag.Get("kind"), Type: sf.Type.Kind()}
	}
	return out
}

// Get / Set access a field by name as a string ("1"/"0" for booleans).
func (c *Config) Get(name string) string {
	f := reflect.ValueOf(c).Elem().FieldByName(name)
	switch f.Kind() {
	case reflect.Bool:
		if f.Bool() {
			return "1"
		}
		return "0"
	case reflect.Int:
		return strconv.Itoa(int(f.Int()))
	}
	return f.String()
}

func (c *Config) Set(name, val string) error {
	f := reflect.ValueOf(c).Elem().FieldByName(name)
	if !f.IsValid() {
		return fmt.Errorf("unknown setting %s", name)
	}
	if err := setFromString(f, strings.TrimSpace(val)); err != nil {
		return fmt.Errorf("%s: %q is not a valid number", name, val)
	}
	if f.Kind() == reflect.String && reflect.TypeOf(*c).Field(fieldIndex(name)).Tag.Get("kind") == "multiline" {
		f.SetString(val) // keep line breaks/indentation as typed
	}
	return nil
}

func fieldIndex(name string) int {
	sf, _ := reflect.TypeOf(Config{}).FieldByName(name)
	return sf.Index[0]
}

// Setting describes one effective value and where it came from.
type Setting struct {
	Name   string
	Value  any
	Source string // policy, registry, default
	Help   string
}

// Defaults returns a Config populated only from the `def` tags.
func Defaults() Config {
	var c Config
	v := reflect.ValueOf(&c).Elem()
	for i := 0; i < v.NumField(); i++ {
		_ = setFromString(v.Field(i), v.Type().Field(i).Tag.Get("def"))
	}
	return c
}

// Load returns the effective configuration.
func Load() Config {
	c, _ := LoadDetailed()
	return c
}

// LoadDetailed returns the effective configuration and per-setting provenance.
func LoadDetailed() (Config, []Setting) {
	c := Defaults()
	policy, _ := registry.OpenKey(registry.LOCAL_MACHINE, PolicyPath, registry.QUERY_VALUE)
	main, _ := registry.OpenKey(registry.LOCAL_MACHINE, KeyPath, registry.QUERY_VALUE)
	defer policy.Close()
	defer main.Close()

	v := reflect.ValueOf(&c).Elem()
	var out []Setting
	for i := 0; i < v.NumField(); i++ {
		f, sf := v.Field(i), v.Type().Field(i)
		name := sf.Tag.Get("reg")
		src := "default"
		for _, k := range []struct {
			key registry.Key
			src string
		}{{policy, "policy"}, {main, "registry"}} {
			if k.key != 0 && readValue(k.key, name, f) {
				src = k.src
				break
			}
		}
		out = append(out, Setting{Name: name, Value: f.Interface(), Source: src, Help: sf.Tag.Get("help")})
	}
	return c, out
}

// LoadStored returns HKLM\SOFTWARE\OneDriveHelper over defaults, ignoring
// policy overrides (what the config tool edits and exports).
func LoadStored() Config {
	c := Defaults()
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, KeyPath, registry.QUERY_VALUE); err == nil {
		v := reflect.ValueOf(&c).Elem()
		for i := 0; i < v.NumField(); i++ {
			readValue(k, v.Type().Field(i).Tag.Get("reg"), v.Field(i))
		}
		k.Close()
	}
	return c
}

// PolicyOverrides lists settings enforced via HKLM\SOFTWARE\Policies\OneDriveHelper.
func PolicyOverrides() []string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, PolicyPath, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	names, _ := k.ReadValueNames(-1)
	return names
}

func readValue(k registry.Key, name string, f reflect.Value) bool {
	switch f.Kind() {
	case reflect.String:
		s, _, err := k.GetStringValue(name)
		if err != nil {
			return false
		}
		if x, err := registry.ExpandString(s); err == nil {
			s = x
		}
		f.SetString(s)
	case reflect.Int:
		n, _, err := k.GetIntegerValue(name)
		if err != nil {
			return false
		}
		f.SetInt(int64(n))
	case reflect.Bool:
		n, _, err := k.GetIntegerValue(name)
		if err != nil {
			return false
		}
		f.SetBool(n != 0)
	default:
		return false
	}
	return true
}

func setFromString(f reflect.Value, s string) error {
	switch f.Kind() {
	case reflect.String:
		f.SetString(s)
	case reflect.Int:
		n, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		f.SetInt(int64(n))
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		f.SetBool(b)
	}
	return nil
}

func writeValue(k registry.Key, name string, f reflect.Value) error {
	switch f.Kind() {
	case reflect.String:
		return k.SetStringValue(name, f.String())
	case reflect.Int:
		return k.SetDWordValue(name, uint32(f.Int()))
	case reflect.Bool:
		var n uint32
		if f.Bool() {
			n = 1
		}
		return k.SetDWordValue(name, n)
	}
	return fmt.Errorf("unsupported type for %s", name)
}

// WriteDefaults creates HKLM\SOFTWARE\OneDriveHelper and fills in any values
// that are missing, leaving existing values untouched (requires admin).
func WriteDefaults() error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, KeyPath, registry.ALL_ACCESS)
	if err != nil {
		return err
	}
	defer k.Close()
	d := Defaults()
	v := reflect.ValueOf(&d).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Tag.Get("reg")
		if _, _, err := k.GetValue(name, nil); err == nil {
			continue
		}
		if err := writeValue(k, name, v.Field(i)); err != nil {
			return err
		}
	}
	return nil
}

// RemoveObsolete deletes values under HKLM\SOFTWARE\OneDriveHelper that are
// no longer settings (left by older versions). Returns the names removed.
func RemoveObsolete() []string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, KeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	known := map[string]bool{}
	for _, f := range Fields() {
		known[strings.ToLower(f.Name)] = true
	}
	names, _ := k.ReadValueNames(-1)
	var removed []string
	for _, n := range names {
		if !known[strings.ToLower(n)] && k.DeleteValue(n) == nil {
			removed = append(removed, n)
		}
	}
	return removed
}

// Save validates c and writes every setting to HKLM\SOFTWARE\OneDriveHelper (requires admin).
func Save(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, KeyPath, registry.ALL_ACCESS)
	if err != nil {
		return err
	}
	defer k.Close()
	v := reflect.ValueOf(&c).Elem()
	var errs []error
	for i := 0; i < v.NumField(); i++ {
		errs = append(errs, writeValue(k, v.Type().Field(i).Tag.Get("reg"), v.Field(i)))
	}
	return errors.Join(errs...)
}

// ReadFile parses and validates a JSON settings file. Settings missing from
// the file take their default value. It returns the config and the names
// present in the file.
func ReadFile(path string) (Config, []string, error) {
	c, present, err := ParseFile(path, Defaults())
	if err != nil {
		return c, present, err
	}
	return c, present, c.Validate()
}

// ParseFile parses a JSON settings file over base without validating it.
func ParseFile(path string, base Config) (Config, []string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return base, nil, err
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) // tolerate a UTF-8 BOM (Notepad)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return base, nil, fmt.Errorf("not valid JSON: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	c := base
	if err := dec.Decode(&c); err != nil {
		return base, nil, err
	}
	var present []string
	for name := range raw {
		present = append(present, name)
	}
	return c, present, nil
}

// WriteFile writes c as indented JSON.
func WriteFile(c Config, path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\r', '\n'), 0o644)
}

// Export writes the settings stored on this PC (defaults for anything unset).
func Export(path string) error { return WriteFile(LoadStored(), path) }

// Import validates a JSON file and saves it to the registry. Settings missing
// from the file keep their current value on this PC.
func Import(path string) error {
	c, _, err := ParseFile(path, LoadStored())
	if err != nil {
		return err
	}
	return Save(c) // Save validates
}
