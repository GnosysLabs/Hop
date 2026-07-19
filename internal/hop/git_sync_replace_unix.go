//go:build !windows

package hop

import "os"

func replaceFileAtomic(source, target string) error {
	return os.Rename(source, target)
}
