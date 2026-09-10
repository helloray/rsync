//go:build !windows

package receiver

import (
	"os"
	"path/filepath"
)

// On non-Windows platforms there are no reserved device names, so every
// operation goes through os.Root unchanged.

func (r *SafeRoot) fallbackPath(name string) (string, bool) { return "", false }

func (r *SafeRoot) directPath(name string) string {
	return filepath.Join(r.absRoot, filepath.FromSlash(name))
}

func (r *SafeRoot) fallbackRoot(name string) (*os.Root, string, bool) {
	return nil, "", false
}
