package qoder

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeELF builds the smallest byte string whose first 28 base64 chars decode
// to an ELF header with the requested e_machine: 20 bytes of header is enough
// for the machine check in findELFByMachine.
func fakeELF(machine uint16) []byte {
	head := make([]byte, 20)
	copy(head, "\x7fELF")
	head[4], head[5] = 2, 1 // 64-bit, little-endian
	head[18] = byte(machine)
	head[19] = byte(machine >> 8)
	return append(head, make([]byte, 64)...)
}

// fakeTarball packs a bundle containing both embedded ELF literals the way
// bundle/qodercli.js does: `"<base64 of \x7fELF\x02\x01...>"`.
func fakeTarball(t *testing.T, x64, arm64 []byte) []byte {
	t.Helper()
	encode := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	bundle := "var x = \"" + encode(x64) + "\", y = \"" + encode(arm64) + "\";"
	_ = encode

	var buf strings.Builder
	gzipWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzipWriter)
	writeEntry := func(name, body string) {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Size: int64(len(body)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	writeEntry("package/bundle/other.js", "placeholder")
	writeEntry(umidBundleEntry, bundle)
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(buf.String())
}

func integrityOf(t *testing.T, data []byte) string {
	t.Helper()
	sum := sha512.Sum512(data)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func setUmidEnv(t *testing.T, dataDir string) {
	t.Helper()
	t.Setenv("QODER_DATA_DIR", dataDir)
	t.Setenv("QODER_INSTALL_RUNTIME_INFO", "1")
	// Isolate the helper search: an app-provided helper on the dev machine
	// would short-circuit the install flow.
	t.Setenv("HOME", dataDir)
	t.Setenv("QODER_HOME", "")
	t.Setenv("QODER_CN_HOME", "")
}

func withUmidRegistry(t *testing.T, tarball []byte, integrity string, calls *int) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest"):
			// A relative tarball path keeps the registry-rewrite below honest.
			_, _ = w.Write([]byte(`{"version":"1.1.67","dist":{"integrity":"` + integrity + `","tarball":"/pkg.tgz"}}`))
		case strings.HasSuffix(r.URL.Path, "/pkg.tgz"):
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	original := umidRegistry
	umidRegistry = func(ctx context.Context, url string) ([]byte, error) {
		// Route every registry fetch at the test server.
		return original(ctx, server.URL+strings.TrimPrefix(url, "https://registry.npmjs.org"))
	}
	t.Cleanup(func() { umidRegistry = original })
}

func TestFindELFByMachine(t *testing.T) {
	x64, arm64 := fakeELF(elfMachineX64), fakeELF(elfMachineArm64)
	bundle := "\"" + base64.StdEncoding.EncodeToString(x64) + "\" \"" + base64.StdEncoding.EncodeToString(arm64) + "\""

	got, err := findELFByMachine([]byte(bundle), elfMachineArm64)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(arm64) {
		t.Fatal("arm64 literal not matched")
	}
	got, err = findELFByMachine([]byte(bundle), elfMachineX64)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(x64) {
		t.Fatal("x64 literal not matched")
	}
	if _, err := findELFByMachine([]byte("no binaries here"), elfMachineX64); err == nil {
		t.Fatal("expected error for missing ELF")
	}
}

func TestExtractRuntimeInfoELF(t *testing.T) {
	tarball := fakeTarball(t, fakeELF(elfMachineX64), fakeELF(elfMachineArm64))
	elf, err := extractRuntimeInfoELF(tarball, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if head := string(elf[:4]); head != "\x7fELF" {
		t.Fatalf("not an ELF: %q", head)
	}
	if _, err := extractRuntimeInfoELF(tarball, "386"); err == nil {
		t.Fatal("expected error for unsupported arch")
	}
}

// withUmidHostSupported pretends the host can run the embedded ELFs so the
// install flow is exercised on every OS, not just linux CI.
func withUmidHostSupported(t *testing.T) {
	t.Helper()
	original := umidHostSupported
	umidHostSupported = func() bool { return true }
	t.Cleanup(func() { umidHostSupported = original })
}

func TestEnsureRuntimeInfoInstalls(t *testing.T) {
	withUmidHostSupported(t)
	dataDir := t.TempDir()
	setUmidEnv(t, dataDir)
	x64, arm64 := fakeELF(elfMachineX64), fakeELF(elfMachineArm64)
	tarball := fakeTarball(t, x64, arm64)
	var calls int
	withUmidRegistry(t, tarball, integrityOf(t, tarball), &calls)

	// The probe run will fail (fake ELF is not executable); the write-then-
	// probe-then-cleanup sequence must surface the probe error and leave no
	// broken binary behind.
	err := ensureRuntimeInfo(context.Background(), "cn")
	target := umidInstalledPath()
	if err == nil || !strings.Contains(err.Error(), "probe") {
		t.Fatalf("expected probe failure for a fake ELF, got %v", err)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatal("failed probe should remove the candidate binary")
	}
	// Nothing usable may be discoverable by riskHelperPath afterwards.
	if got := riskHelperPath("cn"); got != "" {
		t.Fatalf("nothing should be installed, found %q", got)
	}
}

func TestEnsureRuntimeInfoIntegrityMismatch(t *testing.T) {
	withUmidHostSupported(t)
	dataDir := t.TempDir()
	setUmidEnv(t, dataDir)
	tarball := fakeTarball(t, fakeELF(elfMachineX64), fakeELF(elfMachineArm64))
	var calls int
	withUmidRegistry(t, tarball, integrityOf(t, []byte("other bytes")), &calls)

	err := ensureRuntimeInfo(context.Background(), "cn")
	if err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
		t.Fatalf("expected integrity mismatch, got %v", err)
	}
}

func TestEnsureRuntimeInfoDisabled(t *testing.T) {
	t.Setenv("QODER_INSTALL_RUNTIME_INFO", "")
	t.Setenv("QODER_DATA_DIR", t.TempDir())
	err := ensureRuntimeInfo(context.Background(), "cn")
	if err == nil || !strings.Contains(err.Error(), "QODER_INSTALL_RUNTIME_INFO") {
		t.Fatalf("expected opt-in error, got %v", err)
	}
}

func TestEnsureRuntimeInfoMissingHelperMessage(t *testing.T) {
	withUmidHostSupported(t)
	setUmidEnv(t, t.TempDir())
	// Registry unreachable → install fails with the fetch error.
	original := umidRegistry
	umidRegistry = func(ctx context.Context, url string) ([]byte, error) {
		return nil, errors.New("dial timeout")
	}
	t.Cleanup(func() { umidRegistry = original })

	err := ensureRuntimeInfo(context.Background(), "cn")
	if err == nil || !strings.Contains(err.Error(), "dial timeout") {
		t.Fatalf("expected fetch error, got %v", err)
	}
}

func TestUmidBinPathJoinsHelperSearch(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("QODER_DATA_DIR", dataDir)
	t.Setenv("HOME", dataDir) // keep the real ~/.qoder homes out of the search
	t.Setenv("QODER_HOME", "")
	t.Setenv("QODER_CN_HOME", "")
	bin := filepath.Join(dataDir, ".bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "runtime-info-" + goPlatform() + "-" + goArch() + "-selfinstall"
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := riskHelperPath("cn"); filepath.Base(got) != name {
		t.Fatalf("riskHelperPath=%q, want %q", got, name)
	}
}
