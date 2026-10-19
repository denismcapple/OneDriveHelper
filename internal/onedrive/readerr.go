package onedrive

import (
	"errors"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// readErrs collects sources that could not be read. A key, value or folder
// that simply does not exist is "empty", not an error; anything else (access
// denied, I/O errors...) is recorded so callers can refuse to act on
// incomplete data. A nil *readErrs drops everything.
type readErrs struct{ list []string }

func notFound(err error) bool {
	return errors.Is(err, registry.ErrNotExist) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func (r *readErrs) add(source string, err error) {
	if r != nil && err != nil && !notFound(err) {
		r.list = append(r.list, source+": "+err.Error())
	}
}

// subKeys lists the subkeys of path, recording real errors.
func (h Hive) subKeys(path string, r *readErrs) []string {
	k, err := h.Open(path, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		r.add(h.Prefix+path, err)
		return nil
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	r.add(h.Prefix+path, err)
	return names
}

// str reads a string value, recording real errors (a missing value is "").
func (h Hive) str(path, name string, r *readErrs) string {
	k, err := h.Open(path, registry.QUERY_VALUE)
	if err != nil {
		r.add(h.Prefix+path, err)
		return ""
	}
	defer k.Close()
	s, _, err := k.GetStringValue(name)
	if err != nil && !errors.Is(err, registry.ErrUnexpectedType) {
		r.add(h.Prefix+path+`\`+name, err)
	}
	return s
}

// values returns every value name of path and, for string values, the value.
func (h Hive) values(path string, r *readErrs) (names, strs []string) {
	k, err := h.Open(path, registry.QUERY_VALUE)
	if err != nil {
		r.add(h.Prefix+path, err)
		return nil, nil
	}
	defer k.Close()
	names, err = k.ReadValueNames(-1)
	r.add(h.Prefix+path, err)
	for _, n := range names {
		if s, _, err := k.GetStringValue(n); err == nil && s != "" {
			strs = append(strs, s)
		}
	}
	return names, strs
}
