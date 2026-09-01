package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// ---------------------------------------------------------------------------
// Registry validation mirroring upstream CLIProxyAPI pluginstore rules.
//
// The rules below are copied from router-for-me/CLIProxyAPI commit
// 81e1b5374f99c212f196f34956eeed964a46b8fa:
//   internal/pluginstore/registry.go (ValidateRegistry, ValidatePlugin,
//   validPluginID, validPluginVersion, GitHubRepositoryParts,
//   PluginInstallType).
// Vendoring the upstream module only for its unexported validators is not
// worth the dependency; keep this local mirror in sync with re-audits.
// ---------------------------------------------------------------------------

var (
	upstreamPluginIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	upstreamPluginVersionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]*$`)
)

type storeRegistry struct {
	SchemaVersion int           `json:"schema_version"`
	Plugins       []storePlugin `json:"plugins"`
}

type storePlugin struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Author      string           `json:"author"`
	Version     string           `json:"version"`
	Repository  string           `json:"repository"`
	Homepage    string           `json:"homepage"`
	License     string           `json:"license"`
	Tags        []string         `json:"tags"`
	Install     storeInstallPlan `json:"install"`
}

type storeInstallPlan struct {
	Type string `json:"type"`
}

func upstreamValidPluginID(id string) bool {
	return upstreamPluginIDPattern.MatchString(id)
}

func upstreamValidPluginVersion(version string) bool {
	return version != "" && !strings.HasPrefix(version, "v") && upstreamPluginVersionPattern.MatchString(version)
}

func upstreamGitHubRepositoryParts(repository string) (string, string, error) {
	repository = strings.TrimSpace(repository)
	parsed, errParse := url.Parse(repository)
	if errParse != nil {
		return "", "", fmt.Errorf("invalid repository URL: %w", errParse)
	}
	if parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", fmt.Errorf("repository must be https://github.com/{owner}/{repo}")
	}
	segments := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return "", "", fmt.Errorf("repository must be https://github.com/{owner}/{repo}")
	}
	owner, errOwner := url.PathUnescape(segments[0])
	if errOwner != nil {
		return "", "", fmt.Errorf("invalid repository owner: %w", errOwner)
	}
	repo, errRepo := url.PathUnescape(segments[1])
	if errRepo != nil {
		return "", "", fmt.Errorf("invalid repository name: %w", errRepo)
	}
	if strings.HasSuffix(repo, ".git") {
		return "", "", fmt.Errorf("repository must be https://github.com/{owner}/{repo}")
	}
	return owner, repo, nil
}

func upstreamValidatePlugin(plugin storePlugin) error {
	installType := strings.ToLower(strings.TrimSpace(plugin.Install.Type))
	if installType == "" {
		installType = "github-release"
	}
	if installType != "github-release" {
		// Schema version 1 rejects direct installs and anything unknown.
		return fmt.Errorf("unsupported install type %q", plugin.Install.Type)
	}
	required := map[string]string{
		"id":          plugin.ID,
		"name":        plugin.Name,
		"description": plugin.Description,
		"author":      plugin.Author,
		"repository":  plugin.Repository,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("missing required field %s", field)
		}
	}
	if !upstreamValidPluginID(strings.TrimSpace(plugin.ID)) {
		return fmt.Errorf("invalid plugin id %q", plugin.ID)
	}
	if version := strings.TrimSpace(plugin.Version); version != "" && !upstreamValidPluginVersion(version) {
		return fmt.Errorf("invalid plugin version %q", plugin.Version)
	}
	if _, _, errRepository := upstreamGitHubRepositoryParts(plugin.Repository); errRepository != nil {
		return errRepository
	}
	return nil
}

func upstreamValidateRegistry(raw []byte) error {
	var registry storeRegistry
	if err := json.Unmarshal(raw, &registry); err != nil {
		return err
	}
	if registry.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema_version %d", registry.SchemaVersion)
	}
	seen := make(map[string]struct{}, len(registry.Plugins))
	for index, plugin := range registry.Plugins {
		if errValidate := upstreamValidatePlugin(plugin); errValidate != nil {
			return fmt.Errorf("plugins[%d]: %w", index, errValidate)
		}
		id := strings.TrimSpace(plugin.ID)
		if _, exists := seen[id]; exists {
			return fmt.Errorf("plugins[%d]: duplicate plugin id %q", index, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func TestRegistryPassesUpstreamValidationRules(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(t), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := upstreamValidateRegistry(raw); err != nil {
		t.Fatalf("registry.json fails upstream validation rules: %v", err)
	}

	var registry storeRegistry
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

func TestRegistryValidationRejectsWhatUpstreamRejects(t *testing.T) {
	base := func(mutate func(*storeRegistry)) []byte {
		registry := storeRegistry{
			SchemaVersion: 1,
			Plugins: []storePlugin{{
				ID:          "account-health-pushover",
				Name:        "Account Health Pushover",
				Description: "d",
				Author:      "NoorChasib",
				Repository:  "https://github.com/NoorChasib/cpa-plugin-account-health-pushover",
			}},
		}
		mutate(&registry)
		raw, err := json.Marshal(registry)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	cases := []struct {
		name    string
		mutate  func(*storeRegistry)
		wantErr string
	}{
		{"unsupported schema version", func(r *storeRegistry) { r.SchemaVersion = 3 }, "unsupported schema_version"},
		{"id starting with separator", func(r *storeRegistry) { r.Plugins[0].ID = "-account-health" }, "invalid plugin id"},
		{"id with invalid rune", func(r *storeRegistry) { r.Plugins[0].ID = "account health" }, "invalid plugin id"},
		{"id longer than 128", func(r *storeRegistry) { r.Plugins[0].ID = strings.Repeat("a", 129) }, "invalid plugin id"},
		{"missing description", func(r *storeRegistry) { r.Plugins[0].Description = " " }, "missing required field description"},
		{"missing repository", func(r *storeRegistry) { r.Plugins[0].Repository = "" }, "missing required field repository"},
		{"v-prefixed version", func(r *storeRegistry) { r.Plugins[0].Version = "v0.1.0" }, "invalid plugin version"},
		{"repository .git suffix", func(r *storeRegistry) {
			r.Plugins[0].Repository = "https://github.com/NoorChasib/cpa-plugin-account-health-pushover.git"
		}, "repository must be"},
		{"repository non-github host", func(r *storeRegistry) {
			r.Plugins[0].Repository = "https://gitlab.com/NoorChasib/cpa-plugin-account-health-pushover"
		}, "repository must be"},
		{"repository http scheme", func(r *storeRegistry) {
			r.Plugins[0].Repository = "http://github.com/NoorChasib/cpa-plugin-account-health-pushover"
		}, "repository must be"},
		{"repository extra path segment", func(r *storeRegistry) {
			r.Plugins[0].Repository = "https://github.com/NoorChasib/cpa-plugin-account-health-pushover/tree/main"
		}, "repository must be"},
		{"repository with query", func(r *storeRegistry) {
			r.Plugins[0].Repository = "https://github.com/NoorChasib/cpa-plugin-account-health-pushover?tab=readme"
		}, "repository must be"},
		{"direct install under schema 1", func(r *storeRegistry) { r.Plugins[0].Install.Type = "direct" }, "unsupported install type"},
		{"duplicate plugin ids", func(r *storeRegistry) { r.Plugins = append(r.Plugins, r.Plugins[0]) }, "duplicate plugin id"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := upstreamValidateRegistry(base(testCase.mutate))
			if err == nil {
				t.Fatalf("expected rejection containing %q, got nil", testCase.wantErr)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error %q does not contain %q", err, testCase.wantErr)
			}
		})
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

func TestWorkflowHardeningContract(t *testing.T) {
	root := repositoryRoot(t)
	ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"concurrency:",
		"timeout-minutes:",
		`CPA_SMOKE_REQUIRE_DOCKER: "1"`,
		"verify-release",
	} {
		if !strings.Contains(string(ci), required) {
			t.Fatalf("ci.yml is missing hardening contract text %q", required)
		}
	}
	release, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"workflow_dispatch:",
		"inputs.version",
		"C:/msys64/ucrt64/bin/gcc.exe",
		"mingw-w64-ucrt-x86_64-gcc",
		"verify-bundle:",
		"./scripts/verify-release.sh dist",
		`CPA_SMOKE_REQUIRE_DOCKER: "1"`,
		"timeout-minutes:",
	} {
		if !strings.Contains(string(release), required) {
			t.Fatalf("release.yml is missing hardening contract text %q", required)
		}
	}
	// The rehearsal must never publish: the publish job stays tag-push gated
	// and must depend on the full-bundle verification job.
	if !strings.Contains(string(release), "needs: [build, verify-bundle]") {
		t.Fatal("release.yml publish job does not depend on verify-bundle")
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

// ---------------------------------------------------------------------------
// verify-release.sh behavior: full, partial, and incomplete bundles.
// ---------------------------------------------------------------------------

var verifierExtensions = map[string]string{"linux": "so", "darwin": "dylib", "windows": "dll"}

var verifierAllPlatforms = [][2]string{
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
	{"windows", "amd64"},
}

func writeVerifierArchive(t *testing.T, dir, version, goos, goarch, entryName string) string {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	entry, err := writer.Create(entryName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("library-" + goos + "-" + goarch)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("account-health-pushover_%s_%s_%s.zip", version, goos, goarch)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeVerifierSidecar(t *testing.T, archivePath string) {
	t.Helper()
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	line := hex.EncodeToString(digest[:]) + "  " + filepath.Base(archivePath) + "\n"
	if err := os.WriteFile(archivePath+".sha256", []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeVerifierChecksums(t *testing.T, dir string, archivePaths []string) {
	t.Helper()
	var lines []string
	for _, archivePath := range archivePaths {
		raw, err := os.ReadFile(archivePath)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(raw)
		lines = append(lines, hex.EncodeToString(digest[:])+"  "+filepath.Base(archivePath))
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildVerifierDist(t *testing.T, platforms [][2]string) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	var archives []string
	for _, platform := range platforms {
		goos, goarch := platform[0], platform[1]
		entry := "account-health-pushover." + verifierExtensions[goos]
		archives = append(archives, writeVerifierArchive(t, dir, "0.1.0", goos, goarch, entry))
	}
	for _, archive := range archives {
		writeVerifierSidecar(t, archive)
	}
	writeVerifierChecksums(t, dir, archives)
	return dir, archives
}

func runVerifyRelease(t *testing.T, partial bool, dist string) (string, error) {
	t.Helper()
	script := filepath.Join(repositoryRoot(t), "scripts", "verify-release.sh")
	args := []string{script}
	if partial {
		args = []string{script, "--partial"}
	}
	args = append(args, dist)
	command := exec.Command("bash", args...)
	output, err := command.CombinedOutput()
	return string(output), err
}

func requireVerifierPrerequisites(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("verify-release.sh requires bash")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not available")
	}
}

func TestVerifyReleaseFullBundlePasses(t *testing.T) {
	requireVerifierPrerequisites(t)
	dist, _ := buildVerifierDist(t, verifierAllPlatforms)
	for _, partial := range []bool{false, true} {
		output, err := runVerifyRelease(t, partial, dist)
		if err != nil {
			t.Fatalf("partial=%v: verify failed: %v\n%s", partial, err, output)
		}
		if !strings.Contains(output, "verified 5 CPA Plugin Store archive(s)") {
			t.Fatalf("partial=%v: unexpected output: %s", partial, output)
		}
	}
}

func TestVerifyReleasePartialBundlePassesOnlyInPartialMode(t *testing.T) {
	requireVerifierPrerequisites(t)
	dist, _ := buildVerifierDist(t, [][2]string{{"linux", "amd64"}})

	output, err := runVerifyRelease(t, true, dist)
	if err != nil {
		t.Fatalf("partial mode rejected a valid single-platform bundle: %v\n%s", err, output)
	}
	if !strings.Contains(output, "verified 1 CPA Plugin Store archive(s)") {
		t.Fatalf("unexpected partial output: %s", output)
	}

	output, err = runVerifyRelease(t, false, dist)
	if err == nil {
		t.Fatalf("full mode accepted an incomplete four-platforms-missing bundle:\n%s", output)
	}
	if !strings.Contains(output, "missing required platform archive(s)") {
		t.Fatalf("full-mode failure did not name missing platforms: %s", output)
	}
	for _, missing := range []string{"linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"} {
		if !strings.Contains(output, missing) {
			t.Fatalf("full-mode failure does not list %s: %s", missing, output)
		}
	}
}

func TestVerifyReleaseIncompleteBundlesFailInBothModes(t *testing.T) {
	requireVerifierPrerequisites(t)
	cases := []struct {
		name    string
		corrupt func(t *testing.T, dist string, archives []string)
		wantErr string
	}{
		{
			"missing sidecar",
			func(t *testing.T, dist string, archives []string) {
				if err := os.Remove(archives[0] + ".sha256"); err != nil {
					t.Fatal(err)
				}
			},
			"missing checksum sidecar",
		},
		{
			"tampered sidecar",
			func(t *testing.T, dist string, archives []string) {
				line := strings.Repeat("0", 64) + "  " + filepath.Base(archives[0]) + "\n"
				if err := os.WriteFile(archives[0]+".sha256", []byte(line), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			"checksum sidecar mismatch",
		},
		{
			"missing checksums.txt",
			func(t *testing.T, dist string, archives []string) {
				if err := os.Remove(filepath.Join(dist, "checksums.txt")); err != nil {
					t.Fatal(err)
				}
			},
			"missing checksums.txt",
		},
		{
			"checksums.txt digest mismatch",
			func(t *testing.T, dist string, archives []string) {
				var lines []string
				for _, archive := range archives {
					lines = append(lines, strings.Repeat("0", 64)+"  "+filepath.Base(archive))
				}
				content := strings.Join(lines, "\n") + "\n"
				if err := os.WriteFile(filepath.Join(dist, "checksums.txt"), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			"digest mismatch",
		},
		{
			"checksums.txt orphan entry",
			func(t *testing.T, dist string, archives []string) {
				handle, err := os.OpenFile(filepath.Join(dist, "checksums.txt"), os.O_APPEND|os.O_WRONLY, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				defer handle.Close()
				orphan := strings.Repeat("a", 64) + "  account-health-pushover_9.9.9_linux_amd64.zip\n"
				if _, err := handle.WriteString(orphan); err != nil {
					t.Fatal(err)
				}
			},
			"not present in dist",
		},
		{
			"wrong root entry name",
			func(t *testing.T, dist string, archives []string) {
				path := writeVerifierArchive(t, dist, "0.1.0", "linux", "amd64", "wrong-name.so")
				writeVerifierSidecar(t, path)
				writeVerifierChecksums(t, dist, archives)
			},
			"expected only root entry",
		},
		{
			"mixed versions",
			func(t *testing.T, dist string, archives []string) {
				// Replace the linux/arm64 archive with a different version so
				// all five platforms remain present but versions disagree.
				if err := os.Remove(archives[1]); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(archives[1] + ".sha256"); err != nil {
					t.Fatal(err)
				}
				extra := writeVerifierArchive(t, dist, "0.2.0", "linux", "arm64", "account-health-pushover.so")
				writeVerifierSidecar(t, extra)
				remaining := append([]string{archives[0]}, archives[2:]...)
				writeVerifierChecksums(t, dist, append(remaining, extra))
			},
			"mixed release versions",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			for _, partial := range []bool{false, true} {
				dist, archives := buildVerifierDist(t, verifierAllPlatforms)
				testCase.corrupt(t, dist, archives)
				output, err := runVerifyRelease(t, partial, dist)
				if err == nil {
					t.Fatalf("partial=%v: verifier accepted an incomplete bundle:\n%s", partial, output)
				}
				if !strings.Contains(output, testCase.wantErr) {
					t.Fatalf("partial=%v: failure output %q does not contain %q", partial, output, testCase.wantErr)
				}
			}
		})
	}
}
