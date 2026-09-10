//go:build windows

package receiver

import (
	"os"
	"path/filepath"
	"strings"
)

// fallbackPath reports the \\?\-prefixed absolute path for name when any of
// its components is a Windows reserved device name (AUX, CON, NUL, COM1…,
// with any extension). os.Root rejects such paths ("path escapes from
// parent"), but they are legal on NTFS, so they are resolved directly.
// Names containing ".." are left to os.Root so its traversal check keeps
// rejecting them.
func (r *SafeRoot) fallbackPath(name string) (string, bool) {
	if name == "" || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", false
	}
	found := false
	for _, part := range strings.Split(filepath.FromSlash(name), string(filepath.Separator)) {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", false
		}
		if reservedDeviceName(part) {
			found = true
		}
	}
	if !found {
		return "", false
	}
	return r.directPath(name), true
}

// directPath builds a literal \\?\ path below the root, which disables Win32
// reserved-name interpretation (and the 260-char limit).
func (r *SafeRoot) directPath(name string) string {
	return `\\?\` + filepath.Join(r.absRoot, filepath.FromSlash(name))
}

// fallbackRoot opens name as an *os.Root via its \\?\ direct path.
func (r *SafeRoot) fallbackRoot(name string) (*os.Root, string, bool) {
	abs, ok := r.fallbackPath(name)
	if !ok {
		return nil, "", false
	}
	sub, err := os.OpenRoot(abs)
	if err != nil {
		return nil, "", false
	}
	return sub, filepath.Join(r.absRoot, filepath.FromSlash(name)), true
}

// unrepresentable reports whether name cannot exist as an ordinary file or
// directory on Windows: any component containing a character NTFS forbids.
// Colons are the field case (Perl man pages like "Convert::Binary::C.3pm"):
// os.Root rejects them outright, and even \\?\ paths cannot create them
// because NTFS treats ":" as a stream separator. C rsync cannot transfer
// these names on Windows either — the receiver skips them and counts an
// IO error.
func unrepresentable(name string) bool {
	for _, part := range strings.Split(filepath.FromSlash(name), string(filepath.Separator)) {
		switch part {
		case "", ".", "..":
			continue
		}
		if strings.ContainsAny(part, `<>:"|?*`) || strings.IndexFunc(part, func(r rune) bool { return r < 0x20 }) >= 0 {
			return true
		}
	}
	return false
}

// reservedDeviceName reports whether part is a DOS device name up to its
// first extension dot (the mirror of Go's internal filepathlite check that
// makes os.Root reject such paths).
func reservedDeviceName(part string) bool {
	if i := strings.IndexByte(part, '.'); i >= 0 {
		part = part[:i]
	}
	switch strings.ToUpper(part) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}
