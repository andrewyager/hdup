//go:build !windows

package job

import (
	"fmt"
	"os"
)

// checkPrivate fails if the group or other users have access to the file.
func checkPrivate(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o and holds secrets; other users must have no access (chmod 600)", path, mode)
	}
	return nil
}
