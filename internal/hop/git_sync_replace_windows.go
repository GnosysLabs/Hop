//go:build windows

package hop

func replaceFileAtomic(source, target string) error {
	return moveFileReplace(source, target)
}
