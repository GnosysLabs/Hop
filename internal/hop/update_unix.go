//go:build !windows

package hop

import (
	"errors"
	"fmt"
	"os"
)

func applyBinaryUpdate(staged, target, _ string) (bool, error) {
	info, err := os.Lstat(target)
	if err != nil {
		return false, fmt.Errorf("hop update: inspect installed binary: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("hop update: refusing to replace symlink %s; update it through its package manager or rerun the standalone installer", target)
	}
	if err := os.Rename(staged, target); err != nil {
		return false, fmt.Errorf("hop update: atomically replace %s: %w", target, err)
	}
	return false, nil
}

func completePendingUpdate(_, _ string, _ int) error {
	return errors.New("hop update: deferred replacement is only used on Windows")
}
