package update

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	MaxBundleSize    = 300 << 20
	MaxSignatureSize = 128 << 10
	rootSlotSize     = 256 << 20
)

var (
	versionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]{0,63}$`)
	digestRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type FileSpec struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	FormatVersion int                 `json:"format_version"`
	Architecture  string              `json:"architecture"`
	Version       string              `json:"version"`
	Files         map[string]FileSpec `json:"files"`
}

type State struct {
	Version     string `json:"version"`
	TargetSlot  string `json:"target_slot"`
	SourceSlot  string `json:"source_slot"`
	InstalledAt string `json:"installed_at"`
}

type Status struct {
	CurrentSlot      string `json:"current_slot"`
	PreviousSlot     string `json:"previous_slot"`
	CurrentVersion   string `json:"current_version"`
	StagedVersion    string `json:"staged_version,omitempty"`
	StagedSlot       string `json:"staged_slot,omitempty"`
	RebootRequired   bool   `json:"reboot_required"`
	SignedByOwnerKey bool   `json:"signed_updates_required"`
}

type SignatureVerifier interface {
	Verify(bundlePath, signaturePath, authorizedKeysPath, workDir string) error
}

type OpenSSHVerifier struct{}

func (OpenSSHVerifier) Verify(bundlePath, signaturePath, authorizedKeysPath, workDir string) error {
	authorized, err := os.ReadFile(authorizedKeysPath)
	if err != nil {
		return fmt.Errorf("read authorized update keys: %w", err)
	}
	var allowed strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(string(authorized)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && (strings.HasPrefix(fields[0], "ssh-") || strings.HasPrefix(fields[0], "ecdsa-")) {
			allowed.WriteString("laserbridge-update ")
			allowed.WriteString(line)
			allowed.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if allowed.Len() == 0 {
		return errors.New("no plain SSH public key is available to authorize updates")
	}
	allowedPath := filepath.Join(workDir, "update_allowed_signers")
	if err := atomicWrite(allowedPath, []byte(allowed.String()), 0600); err != nil {
		return err
	}
	bundle, err := os.Open(bundlePath)
	if err != nil {
		return err
	}
	defer bundle.Close()
	command := exec.Command("ssh-keygen", "-Y", "verify", "-f", allowedPath, "-I", "laserbridge-update", "-n", "laserbridge-update", "-s", signaturePath)
	command.Stdin = bundle
	output, err := command.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if len(detail) > 300 {
			detail = detail[:300]
		}
		return fmt.Errorf("update signature rejected: %s", detail)
	}
	return nil
}

type Manager struct {
	DataDir            string
	RuntimeDir         string
	VersionPath        string
	CmdlinePath        string
	BootDir            string
	RootAPath          string
	RootBPath          string
	AuthorizedKeysPath string
	Verifier           SignatureVerifier
	Now                func() time.Time
	mu                 sync.Mutex
}

func (m *Manager) Install(bundlePath, signaturePath string) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := withinSize(bundlePath, MaxBundleSize, "update bundle"); err != nil {
		return State{}, err
	}
	if err := withinSize(signaturePath, MaxSignatureSize, "update signature"); err != nil {
		return State{}, err
	}
	if err := os.MkdirAll(m.runtimeDir(), 0700); err != nil {
		return State{}, err
	}
	verifier := m.Verifier
	if verifier == nil {
		verifier = OpenSSHVerifier{}
	}
	if err := verifier.Verify(bundlePath, signaturePath, m.authorizedKeysPath(), m.runtimeDir()); err != nil {
		return State{}, err
	}
	archive, manifest, entries, err := openAndValidateBundle(bundlePath)
	if err != nil {
		return State{}, err
	}
	defer archive.Close()

	current := m.CurrentSlot()
	target := otherSlot(current)
	bootDir, rootA, rootB, cleanup, err := m.resolveTargets()
	if err != nil {
		return State{}, err
	}
	defer cleanup()
	rootTarget := rootA
	if target == "b" {
		rootTarget = rootB
	}
	if err := writeRootSlot(rootTarget, entries["root.squashfs"]); err != nil {
		return State{}, fmt.Errorf("write root slot %s: %w", target, err)
	}
	slotDir := filepath.Join(bootDir, "boot", "slot-"+target)
	if err := os.MkdirAll(slotDir, 0755); err != nil {
		return State{}, err
	}
	for _, name := range []string{"vmlinuz-lts", "initramfs-lts"} {
		if err := writeZipEntry(filepath.Join(slotDir, name), entries[name], 0644); err != nil {
			return State{}, fmt.Errorf("install %s for slot %s: %w", name, target, err)
		}
	}
	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now().UTC()
	}
	state := State{Version: manifest.Version, TargetSlot: target, SourceSlot: current, InstalledAt: now.Format(time.RFC3339)}
	stateData, _ := json.Marshal(state)
	if err := os.MkdirAll(filepath.Join(m.dataDir(), "update"), 0700); err != nil {
		return State{}, err
	}
	if err := atomicWrite(filepath.Join(m.dataDir(), "update", "state.json"), append(stateData, '\n'), 0600); err != nil {
		return State{}, err
	}
	// This is the commit point. Root and boot payloads are fully synced first.
	if err := atomicWrite(filepath.Join(bootDir, "boot", "active-slot.cfg"), []byte("set laserbridge_slot="+target+"\n"), 0644); err != nil {
		return State{}, fmt.Errorf("activate slot %s: %w", target, err)
	}
	return state, nil
}

func withinSize(path string, limit int64, label string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() < 1 || info.Size() > limit {
		return fmt.Errorf("%s size is invalid", label)
	}
	return nil
}

func (m *Manager) Status() Status {
	current := m.CurrentSlot()
	status := Status{CurrentSlot: current, PreviousSlot: otherSlot(current), CurrentVersion: readTrimmed(m.versionPath()), SignedByOwnerKey: true}
	data, err := os.ReadFile(filepath.Join(m.dataDir(), "update", "state.json"))
	if err == nil {
		var state State
		if json.Unmarshal(data, &state) == nil {
			status.StagedVersion = state.Version
			status.StagedSlot = state.TargetSlot
			status.RebootRequired = state.TargetSlot != "" && state.TargetSlot != current
		}
	}
	return status
}

func (m *Manager) StageRollback() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	target := otherSlot(m.CurrentSlot())
	bootDir, _, _, cleanup, err := m.resolveTargets()
	if err != nil {
		return "", err
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(bootDir, "boot", "slot-"+target, "vmlinuz-lts")); err != nil {
		return "", fmt.Errorf("previous slot %s has no boot payload", target)
	}
	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now().UTC()
	}
	state := State{Version: "previous-slot", TargetSlot: target, SourceSlot: m.CurrentSlot(), InstalledAt: now.Format(time.RFC3339)}
	stateData, _ := json.Marshal(state)
	if err := atomicWrite(filepath.Join(m.dataDir(), "update", "state.json"), append(stateData, '\n'), 0600); err != nil {
		return "", err
	}
	if err := atomicWrite(filepath.Join(bootDir, "boot", "active-slot.cfg"), []byte("set laserbridge_slot="+target+"\n"), 0644); err != nil {
		return "", err
	}
	return target, nil
}

func (m *Manager) CurrentSlot() string {
	data, _ := os.ReadFile(m.cmdlinePath())
	for _, field := range strings.Fields(string(data)) {
		if field == "laserbridge.slot=b" {
			return "b"
		}
	}
	return "a"
}

func openAndValidateBundle(path string) (*zip.ReadCloser, Manifest, map[string]*zip.File, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, Manifest{}, nil, fmt.Errorf("open update bundle: %w", err)
	}
	fail := func(err error) (*zip.ReadCloser, Manifest, map[string]*zip.File, error) {
		archive.Close()
		return nil, Manifest{}, nil, err
	}
	allowed := map[string]bool{"manifest.json": true, "root.squashfs": true, "vmlinuz-lts": true, "initramfs-lts": true}
	entries := map[string]*zip.File{}
	for _, entry := range archive.File {
		if !allowed[entry.Name] || entries[entry.Name] != nil || entry.FileInfo().IsDir() {
			return fail(fmt.Errorf("unexpected or duplicate bundle entry %q", entry.Name))
		}
		entries[entry.Name] = entry
	}
	if len(entries) != len(allowed) {
		return fail(errors.New("update bundle is incomplete"))
	}
	if entries["manifest.json"].UncompressedSize64 > 64<<10 {
		return fail(errors.New("update manifest is too large"))
	}
	manifestReader, err := entries["manifest.json"].Open()
	if err != nil {
		return fail(err)
	}
	decoder := json.NewDecoder(io.LimitReader(manifestReader, 64<<10))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	err = decoder.Decode(&manifest)
	if err == nil {
		var extra any
		if secondErr := decoder.Decode(&extra); !errors.Is(secondErr, io.EOF) {
			err = errors.New("manifest must contain one JSON object")
		}
	}
	manifestReader.Close()
	if err != nil {
		return fail(fmt.Errorf("invalid update manifest: %w", err))
	}
	if manifest.FormatVersion != 1 || manifest.Architecture != "x86_64" || !versionRE.MatchString(manifest.Version) {
		return fail(errors.New("unsupported update format, architecture, or version"))
	}
	limits := map[string]int64{"root.squashfs": rootSlotSize, "vmlinuz-lts": 64 << 20, "initramfs-lts": 128 << 20}
	if len(manifest.Files) != len(limits) {
		return fail(errors.New("update manifest file list is invalid"))
	}
	for name, limit := range limits {
		spec, ok := manifest.Files[name]
		entry := entries[name]
		if !ok || !digestRE.MatchString(spec.SHA256) || spec.Size < 4 || spec.Size > limit || uint64(spec.Size) != entry.UncompressedSize64 {
			return fail(fmt.Errorf("invalid manifest metadata for %s", name))
		}
		if err := verifyZipEntry(entry, spec); err != nil {
			return fail(err)
		}
	}
	root, err := entries["root.squashfs"].Open()
	if err != nil {
		return fail(err)
	}
	magic := make([]byte, 4)
	_, err = io.ReadFull(root, magic)
	root.Close()
	if err != nil || string(magic) != "hsqs" {
		return fail(errors.New("root.squashfs has no SquashFS magic"))
	}
	return archive, manifest, entries, nil
}

func verifyZipEntry(entry *zip.File, spec FileSpec) error {
	reader, err := entry.Open()
	if err != nil {
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(hash, reader)
	closeErr := reader.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != spec.Size || hex.EncodeToString(hash.Sum(nil)) != spec.SHA256 {
		return fmt.Errorf("SHA-256 mismatch for %s", entry.Name)
	}
	return nil
}

func writeRootSlot(path string, entry *zip.File) error {
	target, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if info, statErr := target.Stat(); statErr == nil && info.Mode().IsRegular() {
		if err := target.Truncate(0); err != nil {
			target.Close()
			return err
		}
	}
	reader, err := entry.Open()
	if err != nil {
		target.Close()
		return err
	}
	_, copyErr := io.Copy(target, reader)
	closeReadErr := reader.Close()
	syncErr := target.Sync()
	closeErr := target.Close()
	return errors.Join(copyErr, closeReadErr, syncErr, closeErr)
}

func writeZipEntry(path string, entry *zip.File, mode os.FileMode) error {
	reader, err := entry.Open()
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	target, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		reader.Close()
		return err
	}
	_, copyErr := io.Copy(target, reader)
	closeReadErr := reader.Close()
	syncErr := target.Sync()
	closeErr := target.Close()
	if err := errors.Join(copyErr, closeReadErr, syncErr, closeErr); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func (m *Manager) resolveTargets() (bootDir, rootA, rootB string, cleanup func(), err error) {
	if m.BootDir != "" && m.RootAPath != "" && m.RootBPath != "" {
		return m.BootDir, m.RootAPath, m.RootBPath, func() {}, nil
	}
	output, err := exec.Command("findfs", "LABEL=LBBOOT").CombinedOutput()
	if err != nil {
		return "", "", "", func() {}, fmt.Errorf("locate boot partition: %w: %s", err, strings.TrimSpace(string(output)))
	}
	bootDevice := strings.TrimSpace(string(output))
	if !strings.HasPrefix(bootDevice, "/dev/") || !strings.HasSuffix(bootDevice, "1") {
		return "", "", "", func() {}, errors.New("unexpected boot partition device")
	}
	base := strings.TrimSuffix(bootDevice, "1")
	bootDir = filepath.Join(m.runtimeDir(), "update-boot")
	if err := os.MkdirAll(bootDir, 0700); err != nil {
		return "", "", "", func() {}, err
	}
	output, err = exec.Command("mount", "-t", "vfat", "-o", "rw,nosuid,nodev,noexec", bootDevice, bootDir).CombinedOutput()
	if err != nil {
		return "", "", "", func() {}, fmt.Errorf("mount boot partition: %w: %s", err, strings.TrimSpace(string(output)))
	}
	cleanup = func() { _ = exec.Command("umount", bootDir).Run() }
	return bootDir, base + "2", base + "3", cleanup, nil
}

func (m *Manager) dataDir() string {
	if m.DataDir != "" {
		return m.DataDir
	}
	return "/data"
}

func (m *Manager) runtimeDir() string {
	if m.RuntimeDir != "" {
		return m.RuntimeDir
	}
	return "/run/laserbridge"
}

func (m *Manager) versionPath() string {
	if m.VersionPath != "" {
		return m.VersionPath
	}
	return "/etc/laserbridge/version"
}

func (m *Manager) cmdlinePath() string {
	if m.CmdlinePath != "" {
		return m.CmdlinePath
	}
	return "/proc/cmdline"
}

func (m *Manager) authorizedKeysPath() string {
	if m.AuthorizedKeysPath != "" {
		return m.AuthorizedKeysPath
	}
	return filepath.Join(m.dataDir(), "ssh", "authorized_keys")
}

func otherSlot(slot string) string {
	if slot == "b" {
		return "a"
	}
	return "b"
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func readTrimmed(path string) string {
	data, _ := os.ReadFile(path)
	return strings.TrimSpace(string(data))
}
