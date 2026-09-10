//go:build linux || darwin

package receiver

import (
	"os"

	"github.com/google/renameio/v2"
)

func symlink(root *SafeRoot, oldname, newname string) error {
	return renameio.SymlinkRoot(root.Root, oldname, newname)
}
