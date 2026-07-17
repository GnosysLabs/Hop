package hop

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	defaultUpdateBaseURL    = "https://githop.xyz"
	defaultUpdateRepository = "GnosysLabs/Hop"
	maxUpdateArchiveBytes   = 128 << 20
	maxUpdateChecksumBytes  = 1 << 20
)

type UpdateOptions struct {
	Version    string
	CheckOnly  bool
	Force      bool
	BaseURL    string
	Repository string
}

type UpdateResult struct {
	CurrentVersion   string `json:"current_version"`
	AvailableVersion string `json:"available_version"`
	Updated          bool   `json:"updated"`
	RestartRequired  bool   `json:"restart_required"`
	Binary           string `json:"binary,omitempty"`
	SkillsRefreshed  bool   `json:"skills_refreshed"`
}

type Updater struct {
	HTTP       *http.Client
	GOOS       string
	GOARCH     string
	Executable string
}

func NewUpdater() *Updater {
	return &Updater{
		HTTP: &http.Client{Timeout: 2 * time.Minute},
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
	}
}

func (u *Updater) Update(ctx context.Context, currentVersion string, options UpdateOptions) (UpdateResult, error) {
	result := UpdateResult{CurrentVersion: strings.TrimPrefix(currentVersion, "v")}
	if u.HTTP == nil {
		u.HTTP = &http.Client{Timeout: 2 * time.Minute}
	}
	if u.GOOS == "" {
		u.GOOS = runtime.GOOS
	}
	if u.GOARCH == "" {
		u.GOARCH = runtime.GOARCH
	}
	baseURL := strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultUpdateBaseURL
	}
	if parsed, err := url.Parse(baseURL); err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.Host == "" {
		return result, errors.New("hop update: release base URL must be an absolute HTTP(S) URL")
	}
	repository := strings.Trim(strings.TrimSpace(options.Repository), "/")
	if repository == "" {
		repository = defaultUpdateRepository
	}
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || !safeReleaseName(parts[0]) || !safeReleaseName(parts[1]) {
		return result, errors.New("hop update: repository must be OWNER/NAME")
	}

	tag, err := u.resolveReleaseTag(ctx, baseURL, repository, options.Version)
	if err != nil {
		return result, err
	}
	result.AvailableVersion = strings.TrimPrefix(tag, "v")
	comparison := compareReleaseVersions(currentVersion, tag)
	if comparison == 0 && !options.Force {
		return result, nil
	}
	if comparison > 0 && !options.Force {
		return result, fmt.Errorf("hop update: installed version %s is newer than requested %s; pass --force to downgrade", result.CurrentVersion, result.AvailableVersion)
	}
	if options.CheckOnly {
		return result, nil
	}

	asset, err := updateAssetName(u.GOOS, u.GOARCH)
	if err != nil {
		return result, err
	}
	executable := u.Executable
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return result, fmt.Errorf("hop update: locate current executable: %w", err)
		}
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return result, fmt.Errorf("hop update: resolve current executable: %w", err)
	}
	stagingDir, err := os.MkdirTemp(filepath.Dir(executable), ".hop-update-*")
	if err != nil {
		return result, fmt.Errorf("hop update: create staging directory beside %s: %w", executable, err)
	}
	keepStaging := false
	defer func() {
		if !keepStaging {
			_ = os.RemoveAll(stagingDir)
		}
	}()

	releaseURL := baseURL + "/" + repository + "/releases/download/" + url.PathEscape(tag)
	archiveBytes, err := u.download(ctx, releaseURL+"/"+asset, maxUpdateArchiveBytes)
	if err != nil {
		return result, fmt.Errorf("hop update: download %s: %w", asset, err)
	}
	checksums, err := u.download(ctx, releaseURL+"/checksums.txt", maxUpdateChecksumBytes)
	if err != nil {
		return result, fmt.Errorf("hop update: download checksums.txt: %w", err)
	}
	if err := verifyUpdateChecksum(asset, archiveBytes, checksums); err != nil {
		return result, err
	}
	stagedName := "hop"
	if u.GOOS == "windows" {
		stagedName = "hop.exe"
	}
	staged := filepath.Join(stagingDir, stagedName)
	if err := extractUpdateBinary(asset, archiveBytes, staged); err != nil {
		return result, err
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		return result, fmt.Errorf("hop update: make staged binary executable: %w", err)
	}
	if err := verifyUpdateBinary(ctx, staged, result.AvailableVersion); err != nil {
		return result, err
	}

	pending, err := applyBinaryUpdate(staged, executable, stagingDir)
	if err != nil {
		return result, err
	}
	result.Updated = true
	result.Binary = executable
	result.RestartRequired = pending
	keepStaging = pending
	if pending {
		return result, nil
	}
	if output, err := exec.CommandContext(ctx, executable, "skill", "install", "--force").CombinedOutput(); err != nil {
		return result, fmt.Errorf("hop update: binary updated, but skill refresh failed: %s", strings.TrimSpace(string(output)))
	}
	result.SkillsRefreshed = true
	return result, nil
}

func (u *Updater) resolveReleaseTag(ctx context.Context, baseURL, repository, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" && requested != "latest" {
		tag := requested
		if !strings.HasPrefix(tag, "v") {
			tag = "v" + tag
		}
		if !safeReleaseTag(tag) {
			return "", fmt.Errorf("hop update: unsafe release tag %q", tag)
		}
		return tag, nil
	}
	body, err := u.download(ctx, baseURL+"/api/v1/repos/"+repository+"/releases?draft=false&page=1&limit=20", maxUpdateChecksumBytes)
	if err != nil {
		return "", fmt.Errorf("hop update: determine latest release: %w", err)
	}
	var releases []struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(body, &releases); err != nil {
		return "", fmt.Errorf("hop update: decode latest release: %w", err)
	}
	for _, release := range releases {
		if !release.Draft && !release.Prerelease && safeReleaseTag(release.TagName) {
			return release.TagName, nil
		}
	}
	return "", errors.New("hop update: no safe stable release was returned")
}

func (u *Updater) download(ctx context.Context, address string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "hop/"+effectiveVersion())
	response, err := u.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	reader := io.LimitReader(response.Body, limit+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return body, nil
}

func updateAssetName(goos, goarch string) (string, error) {
	if goarch != "amd64" && goarch != "arm64" {
		return "", fmt.Errorf("hop update: unsupported architecture %s", goarch)
	}
	switch goos {
	case "darwin", "linux":
		return fmt.Sprintf("hop_%s_%s.tar.gz", goos, goarch), nil
	case "windows":
		return fmt.Sprintf("hop_windows_%s.zip", goarch), nil
	default:
		return "", fmt.Errorf("hop update: unsupported operating system %s", goos)
	}
}

func verifyUpdateChecksum(asset string, archive, checksumFile []byte) error {
	expected := ""
	for _, line := range strings.Split(string(checksumFile), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == asset && len(fields[0]) == 64 {
			candidate := strings.ToLower(fields[0])
			if _, err := hex.DecodeString(candidate); err != nil {
				continue
			}
			expected = candidate
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("hop update: checksums.txt does not contain %s", asset)
	}
	digest := sha256.Sum256(archive)
	actual := hex.EncodeToString(digest[:])
	if actual != expected {
		return fmt.Errorf("hop update: checksum mismatch for %s", asset)
	}
	return nil
}

func extractUpdateBinary(asset string, archive []byte, destination string) error {
	if strings.HasSuffix(asset, ".zip") {
		reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return fmt.Errorf("hop update: open release zip: %w", err)
		}
		for _, file := range reader.File {
			if filepath.ToSlash(file.Name) != "hop.exe" || !file.Mode().IsRegular() {
				continue
			}
			input, err := file.Open()
			if err != nil {
				return err
			}
			err = writeUpdateBinary(destination, input)
			_ = input.Close()
			return err
		}
		return errors.New("hop update: release zip does not contain hop.exe")
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("hop update: open release archive: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("hop update: read release archive: %w", err)
		}
		if filepath.ToSlash(header.Name) != "hop" || header.Typeflag != tar.TypeReg {
			continue
		}
		return writeUpdateBinary(destination, io.LimitReader(tarReader, maxUpdateArchiveBytes))
	}
	return errors.New("hop update: release archive does not contain hop")
}

func writeUpdateBinary(destination string, input io.Reader) error {
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("hop update: create staged binary: %w", err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return fmt.Errorf("hop update: extract staged binary: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("hop update: close staged binary: %w", closeErr)
	}
	return nil
}

func verifyUpdateBinary(ctx context.Context, binary, expectedVersion string) error {
	output, err := exec.CommandContext(ctx, binary, "version", "--json").Output()
	if err != nil {
		return fmt.Errorf("hop update: run staged binary: %w", err)
	}
	var response struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return fmt.Errorf("hop update: decode staged binary version: %w", err)
	}
	if strings.TrimPrefix(response.Version, "v") != strings.TrimPrefix(expectedVersion, "v") {
		return fmt.Errorf("hop update: staged binary reports version %s, expected %s", response.Version, expectedVersion)
	}
	return nil
}

func compareReleaseVersions(current, available string) int {
	current = normalizeReleaseVersion(current)
	available = normalizeReleaseVersion(available)
	if semver.IsValid(current) && semver.IsValid(available) {
		return semver.Compare(current, available)
	}
	if strings.TrimPrefix(current, "v") == strings.TrimPrefix(available, "v") {
		return 0
	}
	return -1
}

func normalizeReleaseVersion(version string) string {
	version = strings.TrimSpace(strings.TrimPrefix(version, "v"))
	if version == "" || version == "dev" {
		return ""
	}
	return "v" + version
}

func safeReleaseTag(tag string) bool {
	if tag == "" {
		return false
	}
	for _, character := range tag {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("._-", character) {
			continue
		}
		return false
	}
	return true
}

func safeReleaseName(name string) bool {
	return name != "" && safeReleaseTag(name) && name != "." && name != ".."
}
