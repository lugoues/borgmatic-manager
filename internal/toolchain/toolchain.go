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
	"syscall"
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

// probeTimeout bounds each health probe of a toolchain binary. The daemon's
// context is its lifetime: probing on it unbounded means a launcher wedged by
// a corrupted managed Python hangs the startup forever instead of counting as
// unhealthy and being reprovisioned.
const probeTimeout = time.Minute

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
	downloadBase     string
	uvSums           map[string]string
	provisionTimeout time.Duration
	probeTimeout     time.Duration
	client           *http.Client
	runUV            func(ctx context.Context, uvPath string, env []string, args ...string) ([]byte, error)
	binVersion       func(ctx context.Context, bin string) (string, error)
}

// New manages the toolchain rooted at root (conventionally
// <state-dir>/toolchain). The logger receives provisioning progress and
// degradation warnings.
func New(root string, logger *slog.Logger) *Toolchain {
	return &Toolchain{
		root:             root,
		logger:           logger,
		downloadBase:     defaultDownloadBase,
		uvSums:           uvSHA256,
		provisionTimeout: provisionTimeout,
		probeTimeout:     probeTimeout,
		client:           &http.Client{},
		runUV: func(ctx context.Context, uvPath string, env []string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, uvPath, args...) // #nosec G204 -- executing the checksum-verified uv this package just installed
			cmd.Env = env
			// Its own process group, killed whole on cancellation, and a
			// bounded pipe wait: uv spawns Python and installer descendants,
			// and an expired provisioning deadline must actually return
			// instead of waiting on a descendant that inherited stdout.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error {
				if cmd.Process == nil {
					return nil
				}
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			cmd.WaitDelay = 10 * time.Second
			out, err := cmd.CombinedOutput()
			if errors.Is(err, exec.ErrWaitDelay) && cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return out, err
		},
		binVersion: func(ctx context.Context, bin string) (string, error) {
			cmd := exec.CommandContext(ctx, bin, "--version") // #nosec G204 -- probing the binary this package just installed
			// The managed interpreter must never read the host's Python
			// configuration, and that includes these probes: a poisoned
			// PYTHONHOME failing the health check would refuse the very
			// toolchain that escapes it.
			cmd.Env = SanitizedEnviron()
			// Its own process group, killed whole on timeout: a damaged
			// launcher's descendant must not survive the probe, or repeated
			// health checks would accumulate orphans.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error {
				if cmd.Process == nil {
					return nil
				}
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			// WaitDelay releases the output-pipe wait after the kill: a hung
			// launcher's child (a wedged Python holding stdout) must not turn
			// a bounded probe back into an unbounded one.
			cmd.WaitDelay = 5 * time.Second
			out, err := cmd.CombinedOutput()
			if errors.Is(err, exec.ErrWaitDelay) && cmd.Process != nil {
				// A descendant held the pipes past the parent's exit; the
				// context never expired, so Cancel never ran.
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return strings.TrimSpace(string(out)), err
		},
	}
}

// versionDirName identifies one exact toolchain composition. Human-readable
// on purpose: an operator listing the directory should see what is installed.
// It is a prefix, not an identity: each build appends a random suffix because
// the launchers embed absolute paths, so a directory can never be renamed and
// a rebuild of the same pins still needs a name of its own. Freshness is
// judged by the manifest, never the name.
func versionDirName() string {
	return fmt.Sprintf("uv%s-py%s-borgmatic%s", uvVersion, pythonVersion, BorgmaticVersion)
}

// PinnedVersionDirName is a directory name matching the current pins.
// Exported for tests that seed a provisioned-looking toolchain; pair it with
// PinnedManifest, which is what freshness is actually judged by.
func PinnedVersionDirName() string { return versionDirName() }

// PinnedManifest is the manifest a provisioning run under the current pins
// writes. Exported for tests seeding a toolchain that must read as fresh.
func PinnedManifest() Manifest {
	return Manifest{
		UVVersion:        uvVersion,
		PythonVersion:    pythonVersion,
		BorgmaticVersion: BorgmaticVersion,
		ProvisionedAt:    time.Now().UTC(),
	}
}

// Exists reports whether a toolchain was ever provisioned here, judged by the
// "current" symlink alone (even dangling). A toolchain whose launcher or whole
// target directory vanished is repair evidence, not absence: falling back to a
// host install on it would silently re-couple the daemon to the host's Python
// packaging, which is exactly what the toolchain exists to prevent.
func (t *Toolchain) Exists() bool {
	_, err := os.Lstat(filepath.Join(t.root, "current"))
	return err == nil
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

// Fresh reports whether the current toolchain matches the compiled-in pins,
// judged by the manifest "current" points at. The manifest is written only
// after the smoke test, so a directory without one is by definition not a
// finished install, and a directory name proves nothing a manifest can't.
func (t *Toolchain) Fresh() bool {
	m, err := t.currentManifest()
	return err == nil &&
		m.UVVersion == uvVersion &&
		m.PythonVersion == pythonVersion &&
		m.BorgmaticVersion == BorgmaticVersion
}

// currentManifest reads the manifest of whatever "current" points at.
func (t *Toolchain) currentManifest() (Manifest, error) {
	var m Manifest
	raw, err := os.ReadFile(filepath.Join(t.root, "current", "manifest.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	return m, nil
}

// currentTargetBase is the directory name "current" points at, or "".
func (t *Toolchain) currentTargetBase() string {
	target, err := os.Readlink(filepath.Join(t.root, "current"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
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
	m, err := t.currentManifest()
	if err != nil {
		return m, false, err
	}
	return m, t.Fresh(), nil
}

// healthy reports whether the toolchain borgmatic at p actually runs and
// reports a plausible version. Used for the degrade fallback, where a stale
// (older-versioned) toolchain is acceptable, so the version is not compared
// to the pin; but it must exist and parse. A launcher truncated into a
// zero-exit no-op would otherwise degrade-pass, sail through preflight's
// floor (which deliberately passes unparseable versions), and record no-op
// invocations as successful backups that created nothing.
func (t *Toolchain) healthy(ctx context.Context, p string) bool {
	ctx, cancel := context.WithTimeout(ctx, t.probeTimeout)
	defer cancel()
	out, err := t.binVersion(ctx, p)
	if err != nil {
		t.logger.Warn("toolchain borgmatic failed its health check", "borgmatic", p, "error", err, "output", out)
		return false
	}
	if !plausibleVersion(reportedVersion(out)) {
		t.logger.Warn("toolchain borgmatic reports no usable version; treating it as broken", "borgmatic", p, "reported", out)
		return false
	}
	return true
}

// plausibleVersion reports whether v looks like a release version: at least
// two dot-separated numeric components ("2.1.7", and beta forms like
// "2.0.0b23" whose numeric prefix qualifies). "1.x" is not a version, and a
// shim printing one must not ride versionAtLeast's unparseable-passes rule.
func plausibleVersion(v string) bool {
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(v) || v[i] != '.' {
		return false
	}
	j := i + 1
	for j < len(v) && v[j] >= '0' && v[j] <= '9' {
		j++
	}
	return j > i+1
}

// PlausibleReportedVersion reports whether --version output carries a version
// a real borgmatic would print. Exported for callers probing a toolchain
// launcher themselves: exit status alone accepts a truncated no-op script.
func PlausibleReportedVersion(out string) bool {
	return plausibleVersion(reportedVersion(out))
}

// freshAndHealthy accepts the current toolchain only when its name matches
// the pins AND its binary runs AND it reports the pinned version. Freshness
// alone is a symlink name: a deleted managed Python or a partially replaced
// launcher keeps the name while every exec fails, or exits zero reporting
// some other version, and trusting either would return a broken toolchain on
// every launch without ever reprovisioning.
func (t *Toolchain) freshAndHealthy(ctx context.Context) (string, bool) {
	p, ok := t.BorgmaticPath()
	if !ok || !t.Fresh() {
		return "", false
	}
	pctx, cancel := context.WithTimeout(ctx, t.probeTimeout)
	defer cancel()
	out, err := t.binVersion(pctx, p)
	if err != nil {
		t.logger.Warn("toolchain borgmatic failed its health check", "borgmatic", p, "error", err, "output", out)
		return "", false
	}
	if reportedVersion(out) != BorgmaticVersion {
		t.logger.Warn("toolchain borgmatic reports the wrong version; reprovisioning",
			"borgmatic", p, "reported", out, "expected", BorgmaticVersion)
		return "", false
	}
	return p, true
}

// reportedVersion extracts the version token from --version output ("2.1.7"
// bare or "borgmatic 2.1.7" prefixed). Compared exactly: a substring check
// would accept "2.1.70" as the pinned "2.1.7" and never repair it.
func reportedVersion(out string) string {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// SanitizedEnviron is the process environment minus the host's Python
// configuration, which the managed interpreter must never read. Exported so
// callers probing a toolchain binary can sanitize their probe without
// mutating the process environment before the probe's verdict is in.
func SanitizedEnviron() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "PYTHONHOME" || k == "PYTHONPATH" || k == "PYTHONSTARTUP" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Ensure returns a working toolchain borgmatic, provisioning or refreshing
// first when the current one is missing, does not match the pins, or no
// longer runs. A failed refresh degrades to the existing toolchain (if it
// still works) rather than failing the launch: a stale borgmatic that works
// beats no borgmatic at all. Only "nothing usable and provisioning failed" is
// an error.
func (t *Toolchain) Ensure(ctx context.Context) (string, error) {
	if p, ok := t.freshAndHealthy(ctx); ok {
		t.tryCleanOldVersions()
		return p, nil
	}
	if err := os.MkdirAll(t.root, 0o700); err != nil {
		return "", fmt.Errorf("creating toolchain directory: %w", err)
	}

	// One provisioner at a time, across processes: a daemon start and a
	// one-shot run racing here would both build the same version directory.
	lock, err := t.acquireProvisionLock(ctx)
	if err != nil {
		return "", fmt.Errorf("locking toolchain for provisioning: %w", err)
	}
	defer lock.Release()

	// Whoever held the lock may have provisioned exactly what was needed.
	if p, ok := t.freshAndHealthy(ctx); ok {
		return p, nil
	}

	pctx, cancel := context.WithTimeout(ctx, t.provisionTimeout)
	defer cancel()
	if err := t.provision(pctx); err != nil {
		// The degrade probe runs on the caller's context (healthy bounds it
		// itself): a provisioning attempt that died by its own deadline must
		// not take the fallback's health check down with it, or a stalled
		// download turns into a failed launch even though the existing
		// toolchain still works.
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

// acquireProvisionLock takes the cross-process provisioning lock without
// going deaf to cancellation: a blocking flock cannot be interrupted by a
// context, so the lock is polled. SIGTERM arriving while another process sits
// in a stalled download must stop this waiter now, not when that download's
// deadline finally expires.
func (t *Toolchain) acquireProvisionLock(ctx context.Context) (*lockfile.Lock, error) {
	path := filepath.Join(t.root, "provision.lock")
	for {
		lock, acquired, err := lockfile.TryExclusive(path)
		if err != nil {
			return nil, err
		}
		if acquired {
			return lock, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// provision builds a new version directory and flips "current" to it.
func (t *Toolchain) provision(ctx context.Context) (err error) {
	// Clear retired directories and crash debris first: under
	// Restart=on-failure a transient outage retries here every few seconds,
	// and each abandoned attempt would otherwise pile up until the disk ran
	// out. provision.lock is held, so nothing here is another live
	// provisioner's build; the in-use guard skips anything a running
	// borgmatic still maps.
	t.cleanOldVersions(t.currentTargetBase())

	versions := filepath.Join(t.root, "versions")
	if mkErr := os.MkdirAll(versions, 0o700); mkErr != nil {
		return fmt.Errorf("creating toolchain versions directory: %w", mkErr)
	}
	// A fresh random-suffixed directory every build: the launchers embed
	// absolute paths, so a directory is never renamed or reused once built,
	// rebuilds of the same pins included.
	vdir, mkErr := os.MkdirTemp(versions, versionDirName()+"-")
	if mkErr != nil {
		return fmt.Errorf("creating toolchain version directory: %w", mkErr)
	}
	name := filepath.Base(vdir)
	// This attempt's directory is nothing anyone uses until the flip; on any
	// failure it is removed rather than left as debris.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(vdir)
		}
	}()
	if mkErr := os.Mkdir(filepath.Join(vdir, "bin"), 0o700); mkErr != nil {
		return fmt.Errorf("creating toolchain version directory: %w", mkErr)
	}

	uvPath := filepath.Join(vdir, "uv")
	if dlErr := t.downloadUV(ctx, uvPath); dlErr != nil {
		return dlErr
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
	if reportedVersion(got) != BorgmaticVersion {
		return fmt.Errorf("smoke test: %s --version reported %q, expected exactly %s", bin, got, BorgmaticVersion)
	}

	// The wheel cache only helps re-installs, which build a fresh directory
	// anyway; drop it rather than carry megabytes per version.
	_ = os.RemoveAll(filepath.Join(vdir, "cache"))

	manifest, err := json.Marshal(PinnedManifest())
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
	// Rename atomically replaces a symlink but refuses to replace a real
	// directory, which "current" can have become through a backup restore or
	// a manual copy. Under provision.lock, and only for that malformed
	// shape, clear it so the repair this whole rebuild exists for can
	// actually commit; the ordinary flip stays rename-over-symlink.
	currentPath := filepath.Join(t.root, "current")
	if info, err := os.Lstat(currentPath); err == nil && info.Mode()&os.ModeSymlink == 0 {
		t.logger.Warn("current toolchain entry is not a symlink; replacing the malformed entry", "path", currentPath)
		if err := os.RemoveAll(currentPath); err != nil {
			return fmt.Errorf("clearing malformed current entry: %w", err)
		}
	}
	if err := os.Rename(tmp, currentPath); err != nil {
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

// tryCleanOldVersions retires old version directories only when the
// provisioning lock is free. Without the lock, a provisioner mid-build has an
// unflipped directory that would look deletable, and a keep target read here
// could predate another process's flip and delete the newly current
// directory. No lock, no cleanup: whoever holds it cleans up afterwards
// anyway.
func (t *Toolchain) tryCleanOldVersions() {
	lock, acquired, err := lockfile.TryExclusive(filepath.Join(t.root, "provision.lock"))
	if err != nil || !acquired {
		return
	}
	defer lock.Release()
	// The keep target is read only under the lock: a value read before it
	// could predate another process's flip and mark the newly current
	// generation as superseded.
	t.cleanOldVersions(t.currentTargetBase())
}

// cleanOldVersions removes every version directory except keep: retired
// installs and crash debris alike. Callers hold provision.lock, so nothing
// here is another live provisioner's unflipped build. The one thing the lock
// cannot see is a borgmatic still RUNNING from a retired directory (Python
// imports lazily, so deleting its environment breaks it mid-run); such a
// directory is skipped and removed by a later pass once the run has ended.
// Best effort throughout: a leftover costs disk, not correctness.
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
		if generationInUse(dir) {
			t.logger.Info("retired toolchain directory is still in use by a running borgmatic; keeping it for a later pass", "version", e.Name())
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			t.logger.Warn("could not remove old toolchain directory", "version", e.Name(), "error", err)
		} else {
			t.logger.Info("removed old toolchain directory", "version", e.Name())
		}
	}
}

// generationInUse reports whether any live process maps files from dir (a
// borgmatic started from that generation keeps its interpreter and modules
// mapped for its whole run). Best effort: unreadable /proc entries are
// treated as not using the directory, and the scan only runs when a deletion
// is already due.
func generationInUse(dir string) bool {
	prefix := dir + string(os.PathSeparator)
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, p := range procs {
		name := p.Name()
		if name[0] < '0' || name[0] > '9' {
			continue
		}
		maps, err := os.ReadFile("/proc/" + name + "/maps") // #nosec G304 -- name is a numeric pid from /proc
		if err != nil {
			continue
		}
		if strings.Contains(string(maps), prefix) {
			return true
		}
	}
	return false
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
