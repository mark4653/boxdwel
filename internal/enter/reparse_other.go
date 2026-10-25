//go:build !windows

package enter

import (
	"os"
)

// isReparse treats symlinks as reparse equivalents; missing paths are not reparse.
func isReparse(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.Mode()&os.ModeSymlink != 0, nil
}
