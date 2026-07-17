//go:build windows

package hop

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

func applyBinaryUpdate(staged, target, stagingDir string) (bool, error) {
	command := exec.Command(staged, "update-apply", "--target", target, "--staging", stagingDir, "--parent", strconv.Itoa(os.Getpid()))
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return false, fmt.Errorf("hop update: start Windows replacement helper: %w", err)
	}
	if err := command.Process.Release(); err != nil {
		return false, fmt.Errorf("hop update: release Windows replacement helper: %w", err)
	}
	return true, nil
}

func completePendingUpdate(target, stagingDir string, parentPID int) error {
	source, err := os.Executable()
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 120; attempt++ {
		if process, findErr := os.FindProcess(parentPID); findErr == nil {
			_, _ = process.Wait()
		}
		candidate := target + ".hop-update"
		if copyErr := copyUpdateFile(source, candidate); copyErr == nil {
			if moveErr := moveFileReplace(candidate, target); moveErr == nil {
				command := exec.Command(target, "skill", "install", "--force")
				command.Stdout = os.Stdout
				command.Stderr = os.Stderr
				if err := command.Run(); err != nil {
					return fmt.Errorf("updated binary, but skill refresh failed: %w", err)
				}
				_ = scheduleDelete(source)
				_ = scheduleDelete(stagingDir)
				return nil
			} else {
				lastErr = moveErr
			}
		} else {
			lastErr = copyErr
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("replace running Hop executable: %w", lastErr)
}

func copyUpdateFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func moveFileReplace(source, target string) error {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	moveFileEx := kernel32.NewProc("MoveFileExW")
	sourcePtr, _ := syscall.UTF16PtrFromString(source)
	targetPtr, _ := syscall.UTF16PtrFromString(target)
	result, _, callErr := moveFileEx.Call(uintptr(unsafe.Pointer(sourcePtr)), uintptr(unsafe.Pointer(targetPtr)), 0x1|0x8)
	if result == 0 {
		return callErr
	}
	return nil
}

func scheduleDelete(path string) error {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	moveFileEx := kernel32.NewProc("MoveFileExW")
	pathPtr, _ := syscall.UTF16PtrFromString(path)
	result, _, callErr := moveFileEx.Call(uintptr(unsafe.Pointer(pathPtr)), 0, 0x4)
	if result == 0 {
		return callErr
	}
	return nil
}
