package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate repository root")
	}
	return filepath.Dir(file)
}

func TestRegistrySchemaAndIdentity(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(t), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		SchemaVersion int `json:"schema_version"`
		Plugins       []struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Author      string   `json:"author"`
			Repository  string   `json:"repository"`
			Homepage    string   `json:"homepage"`
			License     string   `json:"license"`
			Tags        []string `json:"tags"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		t.Fatal(err)
	}
	if registry.SchemaVersion != 1 || len(registry.Plugins) != 1 {
		t.Fatalf("registry header=%+v", registry)
	}
	plugin := registry.Plugins[0]
	if plugin.ID != "account-health-pushover" || plugin.Author != "NoorChasib" {
		t.Fatalf("plugin identity=%+v", plugin)
	}
	wantRepo := "https://github.com/NoorChasib/cpa-plugin-account-health-pushover"
	if plugin.Repository != wantRepo || plugin.Homepage != wantRepo || plugin.License != "MIT" {
		t.Fatalf("plugin URLs/license=%+v", plugin)
	}
	if plugin.Name == "" || plugin.Description == "" || len(plugin.Tags) < 5 {
		t.Fatalf("incomplete registry entry=%+v", plugin)
	}
}

func TestRequiredReleaseAssetNames(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	platforms := []struct {
		name, goos, goarch, extension string
	}{
		{"linux-amd64", "linux", "amd64", "so"},
		{"linux-arm64", "linux", "arm64", "so"},
		{"darwin-amd64", "darwin", "amd64", "dylib"},
		{"darwin-arm64", "darwin", "arm64", "dylib"},
		{"windows-amd64", "windows", "amd64", "dll"},
	}
	for _, platform := range platforms {
		for _, required := range []string{
			"name: " + platform.name,
			"goos: " + platform.goos,
			"goarch: " + platform.goarch,
			"extension: " + platform.extension,
		} {
			if !strings.Contains(text, required) {
				t.Fatalf("release workflow is missing %q for %s", required, platform.name)
			}
		}
	}
	for _, required := range []string{
		"account-health-pushover_${VERSION}_${{ matrix.goos }}_${{ matrix.goarch }}.zip",
		"-X github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/plugin.Version=${VERSION}",
		"dist/checksums.txt",
		"github.event_name == 'push'",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("release workflow is missing contract text %q", required)
		}
	}
}

func TestWorkflowsPinActionsAndReleaseOnlyStableTags(t *testing.T) {
	root := repositoryRoot(t)
	for _, name := range []string{"ci.yml", "release.yml"} {
		raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, mutable := range []string{"actions/checkout@v", "actions/setup-go@v", "actions/upload-artifact@v", "actions/download-artifact@v"} {
			if strings.Contains(text, mutable) {
				t.Fatalf("%s contains mutable action reference %q", name, mutable)
			}
		}
	}
	release, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(release), `^[0-9]+\.[0-9]+\.[0-9]+$`) {
		t.Fatal("release workflow does not enforce stable X.Y.Z versions")
	}
}

func TestPackageScriptCreatesSingleRootLibraryAndChecksum(t *testing.T) {
	root := repositoryRoot(t)
	temp := t.TempDir()
	library := filepath.Join(temp, "account-health-pushover.so")
	archive := filepath.Join(temp, "account-health-pushover_0.1.0_linux_amd64.zip")
	if err := os.WriteFile(library, []byte("test-shared-library"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("python3", filepath.Join(root, "scripts", "package-release.py"),
		"--library", library,
		"--archive", archive,
		"--entry", "account-health-pushover.so",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("package script failed: %v\n%s", err, output)
	}
	bundle, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.Close()
	if len(bundle.File) != 1 || bundle.File[0].Name != "account-health-pushover.so" {
		t.Fatalf("ZIP entries=%v", bundle.File)
	}
	archiveRaw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archiveRaw)
	want := hex.EncodeToString(digest[:]) + "  " + filepath.Base(archive)
	sidecar, err := os.ReadFile(archive + ".sha256")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(sidecar)) != want {
		t.Fatalf("checksum=%q want=%q", strings.TrimSpace(string(sidecar)), want)
	}

	secondArchive := filepath.Join(temp, "second.zip")
	future := time.Now().Add(24 * time.Hour)
	if err := os.Chtimes(library, future, future); err != nil {
		t.Fatal(err)
	}
	second := exec.Command("python3", filepath.Join(root, "scripts", "package-release.py"),
		"--library", library,
		"--archive", secondArchive,
		"--entry", "account-health-pushover.so",
	)
	if output, err := second.CombinedOutput(); err != nil {
		t.Fatalf("second package script failed: %v\n%s", err, output)
	}
	secondRaw, err := os.ReadFile(secondArchive)
	if err != nil {
		t.Fatal(err)
	}
	if string(archiveRaw) != string(secondRaw) {
		t.Fatal("release archive changed when only the source library mtime changed")
	}
}
