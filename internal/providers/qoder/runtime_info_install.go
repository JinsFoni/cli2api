package qoder

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Optional runtime-info self-install: the Qoder desktop app bundles the
// runtime-info helper, but NAS/container deployments have no app. The official
// npm package @qoder-ai/qodercli embeds one runtime-info ELF per linux
// platform (base64 literals inside bundle/qodercli.js) — the same observation
// CreditDaddy made. When QODER_INSTALL_RUNTIME_INFO=1, a missing helper is
// fetched from npm, integrity-checked against the npmjs metadata, the ELF for
// the running GOARCH is extracted, and a probe run must succeed before it
// lands in <dataDir>/.bin where riskHelperPath looks.
//
// Opt-in only: the tarball is several MB and the npm fetch happens on the
// server; default-off keeps the check-in path free of network dependencies.

const (
	umidPackage        = "@qoder-ai/qodercli"
	umidPackageTarball = "https://registry.npmjs.org/" + umidPackage + "/-/qodercli-%s.tgz"
	umidPackageMeta    = "https://registry.npmjs.org/" + umidPackage + "/latest"
	umidBundleEntry    = "package/bundle/qodercli.js"
	umidFetchTimeout   = 3 * time.Minute
	umidInstallTimeout = 5 * time.Minute
	// ELF e_machine for the embedded 64-bit little-endian binaries.
	elfMachineX64   = 62
	elfMachineArm64 = 183
)

// umidInstallDirEnv mirrors the config package's QODER_DATA_DIR default; the
// qoder package cannot import internal/config (provider layering), so the
// same fallback rule is duplicated here.
const umidDataDirFallback = ".qoder-api-proxy"

func umidInstallEnabled() bool {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("QODER_INSTALL_RUNTIME_INFO"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func umidDataDir() string {
	if dir := strings.TrimSpace(os.Getenv("QODER_DATA_DIR")); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, umidDataDirFallback)
	}
	return umidDataDirFallback
}

// umidBinPath is the directory the self-installed helper lands in;
// umidHelperHome reports the Qoder HOME to add to riskHelperHomes
// (riskHelperPath appends ".bin" itself).
func umidBinPath() string {
	return filepath.Join(umidDataDir(), ".bin")
}

func umidHelperHome() string {
	return umidDataDir()
}

func umidHelperName() string {
	return fmt.Sprintf("runtime-info-%s-%s-selfinstall", goPlatform(), goArch())
}

func umidInstalledPath() string {
	return filepath.Join(umidBinPath(), umidHelperName())
}

// umidRegistry is the npm fetch client; swappable in tests.
var umidRegistry = func(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, umidFetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// The default transport would reject registry TLS through some proxies;
	// a plain client keeps this path predictable.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("npm fetch %s: HTTP %d", url, response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 256<<20))
}

// ensureRuntimeInfoFn is the seam checkinOnce calls; tests stub it.
var ensureRuntimeInfoFn = ensureRuntimeInfo

// umidHostSupported gates the install to platforms the embedded ELFs run on;
// a variable so tests exercise the flow on every host OS.
var umidHostSupported = func() bool {
	return runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")
}

func umidHostError() error {
	return fmt.Errorf("runtime-info self-install supports linux amd64/arm64 only (host is %s/%s)", runtime.GOOS, runtime.GOARCH)
}

// ensureRuntimeInfo installs the helper when allowed and missing. All failures
// are returned; the check-in caller decides how loud to be.
func ensureRuntimeInfo(ctx context.Context, region string) error {
	if !umidInstallEnabled() {
		return errors.New("runtime-info helper not installed (set QODER_INSTALL_RUNTIME_INFO=1 to self-install)")
	}
	if !umidHostSupported() {
		return umidHostError()
	}
	if path := riskHelperPath(region); path != "" {
		return nil // an app-provided helper already exists
	}
	target := umidInstalledPath()
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
		if err := probeRuntimeInfo(ctx, target, region); err == nil {
			return nil
		}
		// A stale/broken install: fall through and re-fetch.
		_ = os.Remove(target)
	}

	meta, err := umidRegistry(ctx, umidPackageMeta)
	if err != nil {
		return fmt.Errorf("runtime-info self-install metadata: %w", err)
	}
	var manifest struct {
		Version string `json:"version"`
		Dist    struct {
			Integrity string `json:"integrity"`
			Tarball   string `json:"tarball"`
		} `json:"dist"`
	}
	if err := json.Unmarshal(meta, &manifest); err != nil {
		return fmt.Errorf("runtime-info self-install metadata: %w", err)
	}
	version := strings.TrimPrefix(strings.TrimSpace(manifest.Version), "v")
	if version == "" {
		return errors.New("runtime-info self-install: npm metadata has no version")
	}
	integrity := strings.TrimSpace(manifest.Dist.Integrity)
	if integrity == "" {
		return errors.New("runtime-info self-install: npm metadata has no integrity")
	}
	tarballURL := manifest.Dist.Tarball
	if tarballURL == "" {
		tarballURL = fmt.Sprintf(umidPackageTarball, version)
	}

	// The tarball comes from whatever host the metadata names (npmjs); the
	// integrity check is what makes a mirror or redirect trustworthy.
	tarball, err := umidRegistry(ctx, tarballURL)
	if err != nil {
		return fmt.Errorf("runtime-info self-install download: %w", err)
	}
	if err := verifyIntegrity(tarball, integrity); err != nil {
		return err
	}
	elf, err := extractRuntimeInfoELF(tarball, goArch())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(umidBinPath(), 0o755); err != nil {
		return fmt.Errorf("runtime-info self-install: %w", err)
	}
	if err := os.WriteFile(target, elf, 0o755); err != nil {
		return fmt.Errorf("runtime-info self-install: %w", err)
	}
	if err := probeRuntimeInfo(ctx, target, region); err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("runtime-info self-install probe: %w", err)
	}
	return nil
}

func verifyIntegrity(tarball []byte, integrity string) error {
	encoded, ok := strings.CutPrefix(integrity, "sha512-")
	if !ok || encoded == "" {
		return fmt.Errorf("runtime-info self-install: unsupported integrity %q", integrity)
	}
	expected, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("runtime-info self-install: decode integrity: %w", err)
	}
	actual := sha512.Sum512(tarball)
	if !equalBytes(actual[:], expected) {
		return errors.New("runtime-info self-install: tarball integrity mismatch")
	}
	return nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// extractRuntimeInfoELF scans bundle/qodercli.js for base64 ELF literals and
// returns the one matching the requested arch ("amd64" → x64, "arm64").
func extractRuntimeInfoELF(tarball []byte, goarch string) ([]byte, error) {
	var wantMachine int
	switch goarch {
	case "amd64":
		wantMachine = elfMachineX64
	case "arm64":
		wantMachine = elfMachineArm64
	default:
		return nil, fmt.Errorf("runtime-info self-install: unsupported arch %q", goarch)
	}
	gzipReader, err := gzip.NewReader(strings.NewReader(string(tarball)))
	if err != nil {
		return nil, fmt.Errorf("runtime-info self-install: open tarball: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("runtime-info self-install: read tarball: %w", err)
		}
		if header.Name != umidBundleEntry {
			continue
		}
		bundle, err := io.ReadAll(io.LimitReader(tarReader, 512<<20))
		if err != nil {
			return nil, fmt.Errorf("runtime-info self-install: read bundle: %w", err)
		}
		return findELFByMachine(bundle, uint16(wantMachine))
	}
	return nil, fmt.Errorf("runtime-info self-install: %s not found in tarball", umidBundleEntry)
}

const elfBase64Mark = `"f0VMRgIB` // "\x7fELF" + 64-bit LE, base64 prefix

func findELFByMachine(bundle []byte, wantMachine uint16) ([]byte, error) {
	haystack := string(bundle)
	searchFrom := 0
	for {
		i := strings.Index(haystack[searchFrom:], elfBase64Mark)
		if i < 0 {
			break
		}
		start := searchFrom + i + 1 // skip the opening quote
		end := strings.IndexByte(haystack[start:], '"')
		if end < 0 {
			break
		}
		end += start
		searchFrom = end
		// The e_machine field sits at offset 18 of the ELF header; decoding the
		// first 28 base64 chars is enough to read it.
		head, err := base64.StdEncoding.DecodeString(haystack[start : start+28])
		if err != nil || len(head) < 20 {
			continue
		}
		machine := uint16(head[18]) | uint16(head[19])<<8
		if machine != wantMachine {
			continue
		}
		elf, err := base64.StdEncoding.DecodeString(haystack[start:end])
		if err != nil {
			return nil, fmt.Errorf("runtime-info self-install: decode embedded ELF: %w", err)
		}
		return elf, nil
	}
	return nil, fmt.Errorf("runtime-info self-install: no embedded ELF for machine %d", wantMachine)
}

// probeRuntimeInfo runs the candidate helper once and discards the output; a
// zero exit proves the binary executes on this host (noexec mounts fail here).
func probeRuntimeInfo(ctx context.Context, helperPath, region string) error {
	_, err := runRiskIdentity(ctx, helperPath, riskEnv(region), probeUID())
	return err
}

// probeUID supplies the stdin payload for darwin/windows invocation; linux
// ignores it, and a non-empty placeholder keeps the uid check happy.
func probeUID() string { return "self-install-probe" }
