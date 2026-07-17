package hop

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestUpdaterDownloadsVerifiesReplacesAndRefreshesSkills(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows replacement finishes asynchronously after the parent exits")
	}
	root := t.TempDir()
	target := filepath.Join(root, "hop")
	if err := os.WriteFile(target, []byte("old binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	skillMarker := filepath.Join(root, "skill-refreshed")
	binary := []byte("#!/bin/sh\nif [ \"$1\" = version ]; then printf '{\"version\":\"1.0.10\"}\\n'; exit 0; fi\nif [ \"$1\" = skill ]; then touch \"" + skillMarker + "\"; exit 0; fi\nexit 1\n")
	asset, err := updateAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	archive := makeUpdateArchive(t, asset, binary)
	server, requests := newUpdateServer(t, "v1.0.10", asset, archive, false)
	defer server.Close()

	updater := NewUpdater()
	updater.HTTP = server.Client()
	updater.Executable = target
	result, err := updater.Update(context.Background(), "1.0.9", UpdateOptions{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.RestartRequired || !result.SkillsRefreshed || result.AvailableVersion != "1.0.10" {
		t.Fatalf("update result = %#v", result)
	}
	if contents, err := os.ReadFile(target); err != nil || !bytes.Equal(contents, binary) {
		t.Fatalf("installed binary mismatch: %v", err)
	}
	if _, err := os.Stat(skillMarker); err != nil {
		t.Fatalf("updated binary did not refresh skills: %v", err)
	}
	for _, expected := range []string{"/api/v1/repos/GnosysLabs/Hop/releases", "/" + asset, "/checksums.txt"} {
		if !strings.Contains(strings.Join(*requests, "\n"), expected) {
			t.Fatalf("requests = %v, missing %s", *requests, expected)
		}
	}
}

func TestUpdaterCheckOnlyDoesNotDownloadAssets(t *testing.T) {
	asset, err := updateAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	server, requests := newUpdateServer(t, "v1.0.10", asset, []byte("unused"), false)
	defer server.Close()
	updater := NewUpdater()
	updater.HTTP = server.Client()
	result, err := updater.Update(context.Background(), "1.0.9", UpdateOptions{BaseURL: server.URL, CheckOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated || result.AvailableVersion != "1.0.10" || len(*requests) != 1 {
		t.Fatalf("check result = %#v, requests=%v", result, *requests)
	}
}

func TestUpdaterChecksumFailurePreservesInstalledBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("replacement path is platform-specific")
	}
	root := t.TempDir()
	target := filepath.Join(root, "hop")
	if err := os.WriteFile(target, []byte("original\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	asset, err := updateAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	server, _ := newUpdateServer(t, "v1.0.10", asset, []byte("corrupt"), true)
	defer server.Close()
	updater := NewUpdater()
	updater.HTTP = server.Client()
	updater.Executable = target
	_, err = updater.Update(context.Background(), "1.0.9", UpdateOptions{BaseURL: server.URL})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("update error = %v", err)
	}
	contents, readErr := os.ReadFile(target)
	if readErr != nil || string(contents) != "original\n" {
		t.Fatalf("failed update changed target: %q, %v", contents, readErr)
	}
}

func TestUpdaterRefusesImplicitDowngrade(t *testing.T) {
	server, _ := newUpdateServer(t, "v1.0.9", "unused", nil, false)
	defer server.Close()
	updater := NewUpdater()
	updater.HTTP = server.Client()
	_, err := updater.Update(context.Background(), "1.0.10", UpdateOptions{BaseURL: server.URL})
	if err == nil || !strings.Contains(err.Error(), "newer than requested") {
		t.Fatalf("downgrade error = %v", err)
	}
}

func TestUpdaterLatestSkipsPrerelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		fmt.Fprint(response, `[{"tag_name":"v1.1.0-rc.1","draft":false,"prerelease":true},{"tag_name":"v1.0.10","draft":false,"prerelease":false}]`)
	}))
	defer server.Close()
	updater := NewUpdater()
	updater.HTTP = server.Client()
	result, err := updater.Update(context.Background(), "1.0.9", UpdateOptions{BaseURL: server.URL, CheckOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.AvailableVersion != "1.0.10" {
		t.Fatalf("available version = %q", result.AvailableVersion)
	}
}

func TestUpdateArchiveExtractionRejectsMissingBinary(t *testing.T) {
	archive := makeUpdateArchiveNamed(t, "hop_darwin_arm64.tar.gz", "README.md", []byte("no binary"))
	err := extractUpdateBinary("hop_darwin_arm64.tar.gz", archive, filepath.Join(t.TempDir(), "hop"))
	if err == nil || !strings.Contains(err.Error(), "does not contain hop") {
		t.Fatalf("extract error = %v", err)
	}
}

func TestCompareReleaseVersions(t *testing.T) {
	for _, test := range []struct {
		current, available string
		want               int
	}{
		{"1.0.9", "v1.0.10", -1},
		{"v1.0.10", "1.0.10", 0},
		{"1.1.0", "v1.0.10", 1},
		{"dev", "v1.0.10", -1},
	} {
		got := compareReleaseVersions(test.current, test.available)
		if got != test.want {
			t.Fatalf("compare(%q, %q) = %d, want %d", test.current, test.available, got, test.want)
		}
	}
}

func TestUpdaterDefaultsUnsetRuntimeAndHTTPFields(t *testing.T) {
	asset, err := updateAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	server, _ := newUpdateServer(t, "v1.0.10", asset, nil, false)
	defer server.Close()
	updater := &Updater{}
	result, err := updater.Update(context.Background(), "1.0.9", UpdateOptions{BaseURL: server.URL, CheckOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.AvailableVersion != "1.0.10" {
		t.Fatalf("available version = %q", result.AvailableVersion)
	}
}

func newUpdateServer(t *testing.T, tag, asset string, archive []byte, wrongChecksum bool) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	requests := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests = append(requests, request.URL.RequestURI())
		mu.Unlock()
		switch {
		case strings.HasPrefix(request.URL.Path, "/api/v1/repos/GnosysLabs/Hop/releases"):
			fmt.Fprintf(response, `[{"tag_name":%q,"draft":false}]`, tag)
		case strings.HasSuffix(request.URL.Path, "/"+asset):
			_, _ = response.Write(archive)
		case strings.HasSuffix(request.URL.Path, "/checksums.txt"):
			digest := sha256.Sum256(archive)
			if wrongChecksum {
				digest[0] ^= 0xff
			}
			fmt.Fprintf(response, "%x  %s\n", digest, asset)
		default:
			http.NotFound(response, request)
		}
	}))
	return server, &requests
}

func makeUpdateArchive(t *testing.T, asset string, binary []byte) []byte {
	t.Helper()
	name := "hop"
	if strings.HasSuffix(asset, ".zip") {
		name = "hop.exe"
	}
	return makeUpdateArchiveNamed(t, asset, name, binary)
}

func makeUpdateArchiveNamed(t *testing.T, asset, name string, contents []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if strings.HasSuffix(asset, ".zip") {
		writer := zip.NewWriter(&buffer)
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(contents); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
