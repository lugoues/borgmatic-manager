// Package toolchain provisions a pinned, self-contained borgmatic install so
// the manager does not depend on the host's Python packaging. A pipx or distro
// upgrade rebuilding its environments must not be able to take the backup
// engine's driver down with it.
//
// The chain is: the manager downloads a pinned uv (a static musl binary with
// no dependencies of its own), and uv installs a pinned borgmatic with a
// uv-managed Python. Everything lives under one directory; nothing is read
// from or written to the host's Python, ~/.local, or user-level uv state.
//
// borg itself is deliberately NOT provisioned. Its on-disk repository format
// is version-sensitive and its CLI is used directly against the same
// repositories, so the host must own exactly one borg; preflight fails when
// it is missing rather than shipping a second one.
package toolchain

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lugoues/borgmatic-manager/internal/lockfile"
)

// The pinned toolchain contents. Bumping any of these makes the next launch
// provision a new version directory and atomically switch to it.
const (
	uvVersion = "0.12.6"
	// pythonVersion pins the minor line; uv resolves the patch release at
	// provision time. Patch releases do not reprovision.
	pythonVersion = "3.13"
	// BorgmaticVersion is exported for doctor and log output.
	// renovate: datasource=pypi depName=borgmatic
	BorgmaticVersion = "2.1.7"
)

// uvSHA256 holds the published checksum of uv's release tarball per GOARCH.
// Bumped together with uvVersion by hand (each bump must recompute both sums
// from the release's .sha256 assets), which is why renovate does not manage
// the uv pin.
// The pin is compiled in, so a tampered download is refused without trusting
// anything fetched at runtime.
var uvSHA256 = map[string]string{
	"arm64": "3719891de9ab41c878a84331e55826d2a46421976a346a65326513a6795b089a",
	"amd64": "14e4172aace66a475062cebec7ca04f497d5619e95325dfcc9e4447b9c516846",
}

// uvAssetArch maps GOARCH to uv's release asset architecture names.
var uvAssetArch = map[string]string{
	"arm64": "aarch64",
	"amd64": "x86_64",
}

const defaultDownloadBase = "https://github.com/astral-sh/uv/releases/download"

// provisionTimeout bounds one provisioning attempt: a stalled download must
// not hold the daemon's startup forever, and the existing toolchain (if any)
// keeps working meanwhile.
const provisionTimeout = 10 * time.Minute

// maxDownloadBytes bounds how much of a response is read. The checksum would
// reject an oversized body anyway; this stops the disk filling first.
const maxDownloadBytes = 512 << 20

// Toolchain manages the pinned borgmatic install under one root directory:
//
//	root/
//	  provision.lock
//	  current -> versions/<name>      (atomically flipped symlink)
//	  versions/<name>/
//	    uv                            (the downloaded uv binary)
//	    bin/borgmatic                 (uv's launcher; what callers exec)
//	    tools/ python/ manifest.json
//
// Provisioning builds a complete version directory in place and only then
// flips "current", so a crash or failed download can never damage a working
// install. The launchers embed absolute paths into their own version
// directory, which is why directories are never renamed once built.
type Toolchain struct {
	root   string
	logger *slog.Logger

	// Seams for tests: where releases are fetched from, the expected uv
	// checksums, how uv is executed, and how a binary's version is probed.
	downloadBase string
	uvSums       map[string]string
	client       *http.Client
	runUV        func(ctx context.Context, uvPath string, env []string, args ...string) ([]byte, error)
	binVersion   func(ctx context.Context, bin string) (string, error)
}

// New manages the toolchain rooted at root (conventionally
// <state-dir>/toolchain). The logger receives provisioning progress and
// degradation warnings.
func New(root string, logger *slog.Logger) *Toolchain {
	return &Toolchain{
		root:         root,
		logger:       logger,
		downloadBase: defaultDownloadBase,
		uvSums:       uvSHA256,
		client:       &http.Client{},
		runUV: func(ctx context.Context, uvPath string, env []string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, uvPath, args...) // #nosec G204 -- executing the checksum-verified uv this package just installed
			cmd.Env = env
			return cmd.CombinedOutput()
		},
		binVersion: func(ctx context.Context, bin string) (string, error) {
			cmd := exec.CommandContext(ctx, bin, "--version") // #nosec G204 -- probing the binary this package just installed
			out, err := cmd.CombinedOutput()
			return strings.TrimSpace(string(out)), err
		},
	}
}

// CurrentBorgmatic reports the borgmatic of an already-provisioned toolchain
// under root, without provisioning anything. For callers (restore, config
// rendering) that must not start downloads but should prefer the toolchain
// when one exists.
func CurrentBorgmatic(root string) (string, bool) {
	return New(root, slog.New(slog.DiscardHandler)).BorgmaticPath()
}

// versionDirName identifies one exact toolchain composition. Human-readable
// on purpose: an operator listing the directory should see what is installed.
func versionDirName() string {
	return fmt.Sprintf("uv%s-py%s-borgmatic%s", uvVersion, pythonVersion, BorgmaticVersion)
}

// BorgmaticPath returns the current toolchain's borgmatic, if a provisioned
// toolchain exists at all (fresh or stale).
func (t *Toolchain) BorgmaticPath() (string, bool) {
	p := filepath.Join(t.root, "current", "bin", "borgmatic")
	info, err := os.Stat(p) // follows the current symlink
	if err != nil || info.IsDir() {
		return "", false
	}
	return p, true
}

// Fresh reports whether the current toolchain matches the compiled-in pins.
func (t *Toolchain) Fresh() bool {
	target, err := os.Readlink(filepath.Join(t.root, "current"))
	return err == nil && filepath.Base(target) == versionDirName()
}

// Manifest records what a version directory holds, for doctor output.
type Manifest struct {
	UVVersion        string    `json:"uv_version"`
	PythonVersion    string    `json:"python_version"`
	BorgmaticVersion string    `json:"borgmatic_version"`
	ProvisionedAt    time.Time `json:"provisioned_at"`
}

// Info describes the current toolchain for doctor: the manifest of whatever
// "current" points at, and whether it matches the pins.
func (t *Toolchain) Info() (Manifest, bool, error) {
	var m Manifest
	raw, err := os.ReadFile(filepath.Join(t.root, "current", "manifest.json"))
	if err != nil {
		return m, false, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, false, err
	}
	return m, t.Fresh(), nil
}

// healthy reports whether the toolchain borgmatic at p actually runs and
// reports a version. Freshness alone is a symlink name: a deleted managed
// Python or a corrupted package tree keeps the launcher in place while every
// exec of it fails, and trusting the name would return that broken toolchain
// on every launch without ever reprovisioning.
func (t *Toolchain) healthy(ctx context.Context, p string) bool {
	out, err := t.binVersion(ctx, p)
	if err != nil {
		t.logger.Warn("toolchain borgmatic failed its health check", "borgmatic", p, "error", err, "output", out)
		return false
	}
	return true
}

// Ensure returns a working toolchain borgmatic, provisioning or refreshing
// first when the current one is missing, does not match the pins, or no
// longer runs. A failed refresh degrades to the existing toolchain (if it
// still works) rather than failing the launch: a stale borgmatic that works
// beats no borgmatic at all. Only "nothing usable and provisioning failed" is
// an error.
func (t *Toolchain) Ensure(ctx context.Context) (string, error) {
	if p, ok := t.BorgmaticPath(); ok && t.Fresh() && t.healthy(ctx, p) {
		t.cleanOldVersions(versionDirName())
		return p, nil
	}
	if err := os.MkdirAll(t.root, 0o700); err != nil {
		return "", fmt.Errorf("creating toolchain directory: %w", err)
	}

	// One provisioner at a time, across processes: a daemon start and a
	// one-shot run racing here would both build the same version directory.
	lock, err := lockfile.Exclusive(filepath.Join(t.root, "provision.lock"))
	if err != nil {
		return "", fmt.Errorf("locking toolchain for provisioning: %w", err)
	}
	defer lock.Release()

	// Whoever held the lock may have provisioned exactly what was needed.
	if p, ok := t.BorgmaticPath(); ok && t.Fresh() && t.healthy(ctx, p) {
		return p, nil
	}

	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	if err := t.provision(ctx); err != nil {
		if p, ok := t.BorgmaticPath(); ok && t.healthy(ctx, p) {
			t.logger.Warn("toolchain provisioning failed; continuing with the existing toolchain",
				"error", err, "borgmatic", p)
			return p, nil
		}
		return "", fmt.Errorf("provisioning borgmatic toolchain: %w", err)
	}

	p, ok := t.BorgmaticPath()
	if !ok {
		// Unreachable: provision smoke-tests the binary before flipping.
		return "", errors.New("toolchain provisioned but borgmatic is missing from it")
	}
	t.logger.Info("borgmatic toolchain ready", "borgmatic", p, "version", BorgmaticVersion)
	return p, nil
}

// provision builds the pinned version directory and flips "current" to it.
func (t *Toolchain) provision(ctx context.Context) error {
	name := versionDirName()
	vdir := filepath.Join(t.root, "versions", name)

	// A directory left by a crashed provisioner is unfinished by definition
	// ("current" never pointed at it); rebuild from scratch.
	if err := os.RemoveAll(vdir); err != nil {
		return fmt.Errorf("clearing unfinished toolchain %s: %w", name, err)
	}
	if err := os.MkdirAll(filepath.Join(vdir, "bin"), 0o700); err != nil {
		return fmt.Errorf("creating toolchain version directory: %w", err)
	}

	uvPath := filepath.Join(vdir, "uv")
	if err := t.downloadUV(ctx, uvPath); err != nil {
		return err
	}

	t.logger.Info("installing borgmatic into the toolchain",
		"borgmatic", BorgmaticVersion, "python", pythonVersion)
	out, err := t.runUV(ctx, uvPath, uvEnv(vdir),
		"tool", "install", "--python", pythonVersion, "borgmatic=="+BorgmaticVersion)
	if err != nil {
		return fmt.Errorf("uv tool install borgmatic==%s: %w (output: %s)",
			BorgmaticVersion, err, tail(string(out), 2000))
	}

	// Smoke-test before this install can become "current": the flip is the
	// commit point, and only a binary that actually runs may pass it.
	bin := filepath.Join(vdir, "bin", "borgmatic")
	got, err := t.binVersion(ctx, bin)
	if err != nil {
		return fmt.Errorf("smoke test: running %s --version: %w (output: %s)", bin, err, got)
	}
	if !strings.Contains(got, BorgmaticVersion) {
		return fmt.Errorf("smoke test: %s --version reported %q, expected %s", bin, got, BorgmaticVersion)
	}

	// The wheel cache only helps re-installs, which build a fresh directory
	// anyway; drop it rather than carry megabytes per version.
	_ = os.RemoveAll(filepath.Join(vdir, "cache"))

	manifest, err := json.Marshal(Manifest{
		UVVersion:        uvVersion,
		PythonVersion:    pythonVersion,
		BorgmaticVersion: BorgmaticVersion,
		ProvisionedAt:    time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("encoding toolchain manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(vdir, "manifest.json"), manifest, 0o644); err != nil { // #nosec G306 -- version metadata, not a secret
		return fmt.Errorf("writing toolchain manifest: %w", err)
	}

	// Flip atomically: a symlink is created aside and renamed over, so
	// "current" always points at a complete, smoke-tested install. Relative
	// target, so the state directory can be relocated as a whole.
	tmp := filepath.Join(t.root, ".current.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Join("versions", name), tmp); err != nil {
		return fmt.Errorf("staging current symlink: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(t.root, "current")); err != nil {
		return fmt.Errorf("switching current toolchain: %w", err)
	}

	t.cleanOldVersions(name)
	return nil
}

// downloadUV fetches the pinned uv release tarball, refuses it unless its
// checksum matches the compiled-in pin, and extracts the uv binary to dest.
func (t *Toolchain) downloadUV(ctx context.Context, dest string) error {
	arch, ok := uvAssetArch[runtime.GOARCH]
	if !ok {
		return fmt.Errorf("no pinned uv build for architecture %s; set manager.borgmatic_path to a host install instead", runtime.GOARCH)
	}
	want := t.uvSums[runtime.GOARCH]

	url := fmt.Sprintf("%s/%s/uv-%s-unknown-linux-musl.tar.gz", t.downloadBase, uvVersion, arch)
	t.logger.Info("downloading uv for the borgmatic toolchain", "version", uvVersion, "url", url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building uv download request: %w", err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading uv: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading uv: %s returned %s", url, resp.Status)
	}

	// Buffer to disk while hashing; nothing is extracted until the checksum
	// has passed over the complete file.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".uv-download-*")
	if err != nil {
		return fmt.Errorf("creating uv download file: %w", err)
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxDownloadBytes)); err != nil {
		return fmt.Errorf("downloading uv: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("uv download failed verification: sha256 %s, expected %s (refusing to install it)", got, want)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding uv download: %w", err)
	}

	return extractUV(tmp, dest)
}

// extractUV pulls the "uv" member out of the release tarball. Only that one
// member is taken, by its basename within the archive's own directory, so a
// crafted archive cannot place anything else.
func extractUV(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("reading uv archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("uv archive holds no uv binary")
		}
		if err != nil {
			return fmt.Errorf("reading uv archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "uv" {
			continue
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) // #nosec G302 G304 -- an executable written to this package's own version directory
		if err != nil {
			return fmt.Errorf("writing uv binary: %w", err)
		}
		if _, err := io.Copy(out, io.LimitReader(tr, maxDownloadBytes)); err != nil {
			_ = out.Close()
			return fmt.Errorf("writing uv binary: %w", err)
		}
		return out.Close()
	}
}

// uvEnv is the environment uv runs with: everything it reads or writes stays
// inside the version directory, and the host's Python can never be selected.
// Proxy and CA variables pass through, because provisioning may well be the
// reason a proxied host is configured at all.
func uvEnv(vdir string) []string {
	env := []string{
		"HOME=" + vdir,
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"UV_NO_CONFIG=1",
		"UV_PYTHON_PREFERENCE=only-managed",
		"UV_TOOL_DIR=" + filepath.Join(vdir, "tools"),
		"UV_TOOL_BIN_DIR=" + filepath.Join(vdir, "bin"),
		"UV_PYTHON_INSTALL_DIR=" + filepath.Join(vdir, "python"),
		"UV_CACHE_DIR=" + filepath.Join(vdir, "cache"),
	}
	for _, k := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR",
	} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// supersededGrace is how long a superseded toolchain version survives before
// deletion. A borgmatic started from it (a scheduled run, a restore, or a
// passthrough that holds none of the manager's locks) keeps importing modules
// from its environment for as long as it runs; deleting on the flip would
// pull the interpreter's files out from under it mid-backup. A day outlives
// any sane run, and the cost of waiting is one ~150MB directory.
const supersededGrace = 24 * time.Hour

// cleanOldVersions retires every version directory except keep, in two
// stages: a directory is first marked superseded, and only removed by a later
// pass once the mark has aged past supersededGrace. Best effort throughout: a
// leftover costs disk, not correctness.
func (t *Toolchain) cleanOldVersions(keep string) {
	entries, err := os.ReadDir(filepath.Join(t.root, "versions"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == keep {
			continue
		}
		dir := filepath.Join(t.root, "versions", e.Name())
		marker := filepath.Join(dir, ".superseded")
		info, err := os.Stat(marker)
		if err != nil {
			if err := os.WriteFile(marker, nil, 0o600); err == nil {
				t.logger.Info("toolchain version superseded; removed after a grace period in case a running borgmatic still uses it",
					"version", e.Name(), "grace", supersededGrace)
			}
			continue
		}
		if time.Since(info.ModTime()) < supersededGrace {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			t.logger.Warn("could not remove old toolchain version", "version", e.Name(), "error", err)
		} else {
			t.logger.Info("removed old toolchain version", "version", e.Name())
		}
	}
}

// tail returns the last n bytes of s: uv failures put the useful part
// (resolver or network errors) at the end.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
