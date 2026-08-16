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
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/atomicfile"
)

const (
	MaxBundleSize    = 300 << 20
	MaxSignatureSize = 128 << 10
	// Must match ROOT_SECTORS in build/build-image.sh; check-boot-policy.sh
	// asserts the two agree.
	rootSlotSize = 192 << 20
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
	// Slots is what each slot holds, as far as anyone has been able to tell:
	// filled in for the target when an update is written, and for the running
	// slot every time a boot is confirmed.
	//
	// A slot's version lives in its own read-only filesystem, which is not
	// mounted while it is the inactive one. Reading it would mean mounting a
	// squashfs on every status request, several times a minute, to answer a
	// question that only changes when something is installed - so it is
	// written down at the two moments when it is known for free.
	Slots map[string]string `json:"slots,omitempty"`
}

type Status struct {
	CurrentSlot    string `json:"current_slot"`
	PreviousSlot   string `json:"previous_slot"`
	CurrentVersion string `json:"current_version"`
	// PreviousVersion is what the other slot holds - what a rollback would
	// boot into. Empty until something has been installed or confirmed while
	// this appliance was keeping the record.
	PreviousVersion string `json:"previous_version,omitempty"`
	// StagedVersion and StagedSlot describe an update that is waiting for a
	// reboot, and nothing else. They used to describe the last installation
	// whether or not it had already booted, so an appliance running the update
	// it installed an hour ago reported the same version as running and as
	// staged - which reads as though something were still pending.
	StagedVersion  string `json:"staged_version,omitempty"`
	StagedSlot     string `json:"staged_slot,omitempty"`
	RebootRequired bool   `json:"reboot_required"`
	// SignatureAccepted is constant: a detached signature from an authorized
	// key always authorises an update.
	SignatureAccepted bool `json:"signature_accepted"`
	// PasswordAccepted and DefaultPasswordInUse are filled in by the API,
	// which is the layer that knows about the account database.
	PasswordAccepted     bool `json:"password_accepted"`
	DefaultPasswordInUse bool `json:"default_password_in_use"`
}

// rollbackVersion is what the record holds for "boot the other slot", where
// there is no bundle and therefore no version of its own.
const rollbackVersion = "previous-slot"

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
	if err := atomicfile.Write(allowedPath, []byte(allowed.String()), 0600); err != nil {
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

// Install writes a verified bundle into the inactive slot.
//
// An empty signaturePath means the caller has already established that this
// update is authorised - the API accepts the appliance password as an
// alternative, because an appliance set up without a key file has no private
// key to sign with. The bundle's own integrity is checked either way: the
// manifest digests and the SquashFS magic are never skipped.
func (m *Manager) Install(bundlePath, signaturePath string) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := withinSize(bundlePath, MaxBundleSize, "update bundle"); err != nil {
		return State{}, err
	}
	if err := os.MkdirAll(m.runtimeDir(), 0700); err != nil {
		return State{}, err
	}
	if signaturePath != "" {
		if err := withinSize(signaturePath, MaxSignatureSize, "update signature"); err != nil {
			return State{}, err
		}
		verifier := m.Verifier
		if verifier == nil {
			verifier = OpenSSHVerifier{}
		}
		if err := verifier.Verify(bundlePath, signaturePath, m.authorizedKeysPath(), m.runtimeDir()); err != nil {
			return State{}, err
		}
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
	// Both slots are known at this moment: the one being written holds what the
	// bundle says, and the one this is running from holds what this is running.
	state.Slots = m.slotsWith(map[string]string{
		target:  manifest.Version,
		current: readTrimmed(m.versionPath()),
	})
	if err := m.writeState(state); err != nil {
		return State{}, err
	}
	// This is the commit point. Root and boot payloads are fully synced first.
	if err := atomicfile.Write(filepath.Join(bootDir, "boot", "active-slot.cfg"), []byte("set laserbridge_slot="+target+"\n"), 0644); err != nil {
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
	previous := otherSlot(current)
	status := Status{CurrentSlot: current, PreviousSlot: previous, CurrentVersion: readTrimmed(m.versionPath()), SignatureAccepted: true}
	state := m.readState()
	status.PreviousVersion = state.Slots[previous]
	// Staged means waiting for a reboot. An installation that has already been
	// booted into is not staged, it is running - saying it twice was the whole
	// of the confusion this answers.
	if state.TargetSlot != "" && state.TargetSlot != current {
		status.RebootRequired = true
		status.StagedSlot = state.TargetSlot
		status.StagedVersion = state.Version
		if known := state.Slots[state.TargetSlot]; status.StagedVersion == rollbackVersion && known != "" {
			// A rollback records no version of its own - it is asking for
			// whatever is over there. Name it if it is known.
			status.StagedVersion = known
		}
	}
	return status
}

func (m *Manager) readState() State {
	var state State
	data, err := os.ReadFile(filepath.Join(m.dataDir(), "update", "state.json"))
	if err != nil {
		return state
	}
	if json.Unmarshal(data, &state) != nil {
		return State{}
	}
	return state
}

// slotsWith merges what is now known into what was known before, so that one
// slot being written does not erase what the record says about the other.
func (m *Manager) slotsWith(known map[string]string) map[string]string {
	slots := map[string]string{}
	for slot, version := range m.readState().Slots {
		slots[slot] = version
	}
	for slot, version := range known {
		if version != "" {
			slots[slot] = version
		}
	}
	return slots
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
	state := State{Version: rollbackVersion, TargetSlot: target, SourceSlot: m.CurrentSlot(), InstalledAt: now.Format(time.RFC3339)}
	state.Slots = m.slotsWith(map[string]string{m.CurrentSlot(): readTrimmed(m.versionPath())})
	if err := m.writeState(state); err != nil {
		return "", err
	}
	if err := atomicfile.Write(filepath.Join(bootDir, "boot", "active-slot.cfg"), []byte("set laserbridge_slot="+target+"\n"), 0644); err != nil {
		return "", err
	}
	return target, nil
}

// ConfirmBoot records that the running slot reached a fully started system.
//
// GRUB raises a boot-attempt counter before handing over to the kernel and
// switches to the other slot once it has counted three attempts that were
// never confirmed. Resetting the counter here closes that loop: an update
// that panics, loses its initramfs, or never brings up the services is undone
// by the bootloader without anyone having to attach a console to a headless
// appliance.
//
// When GRUB has already fallen back, the running slot differs from the one
// recorded in active-slot.cfg. That slot has now proven itself, so it becomes
// the recorded one and the staged update that failed to boot is dropped
// instead of asking for yet another reboot into it.
func (m *Manager) ConfirmBoot() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.CurrentSlot()
	bootDir, _, _, cleanup, err := m.resolveTargets()
	if err != nil {
		return err
	}
	defer cleanup()
	environment := map[string]string{"laserbridge_try": "0", "laserbridge_override": ""}
	if err := writeGrubEnvironment(filepath.Join(bootDir, "boot", "grubenv"), environment); err != nil {
		return fmt.Errorf("reset boot counter: %w", err)
	}
	// What this slot holds is known for free at exactly this moment, and this
	// is the only place that learns it without mounting anything. Written down
	// here, the interface can say what a rollback would boot into rather than
	// asking somebody to remember.
	state := m.readState()
	state.Slots = m.slotsWith(map[string]string{current: readTrimmed(m.versionPath())})

	activePath := filepath.Join(bootDir, "boot", "active-slot.cfg")
	if readTrimmed(activePath) == "set laserbridge_slot="+current {
		return m.writeState(state)
	}
	if err := atomicfile.Write(activePath, []byte("set laserbridge_slot="+current+"\n"), 0644); err != nil {
		return fmt.Errorf("record recovered slot %s: %w", current, err)
	}
	// GRUB fell back, so the update that was waiting for a reboot is the one
	// that could not boot. Forget that it was asked for - but not what the
	// slots hold, which is the thing worth knowing afterwards.
	return m.writeState(State{Slots: state.Slots})
}

func (m *Manager) writeState(state State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(m.dataDir(), "update"), 0700); err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(m.dataDir(), "update", "state.json"), append(data, '\n'), 0600)
}

const grubEnvironmentSize = 1024

// writeGrubEnvironment writes a GRUB environment block: a fixed 1024-byte
// file holding a header, one key=value line per variable, and '#' padding.
// GRUB rewrites this file in place from the bootloader, so the size is part
// of the format and must not change.
func writeGrubEnvironment(path string, values map[string]string) error {
	var content strings.Builder
	content.WriteString("# GRUB Environment Block\n")
	content.WriteString("# WARNING: Do not edit this file by tools other than grub-editenv!!!\n")
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if strings.ContainsAny(name, "\n=") || strings.Contains(values[name], "\n") {
			return fmt.Errorf("invalid GRUB environment entry %q", name)
		}
		content.WriteString(name + "=" + values[name] + "\n")
	}
	if content.Len() > grubEnvironmentSize {
		return errors.New("GRUB environment block is too large")
	}
	block := make([]byte, grubEnvironmentSize)
	for i := copy(block, content.String()); i < len(block); i++ {
		block[i] = '#'
	}
	return atomicfile.Write(path, block, 0644)
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

func readTrimmed(path string) string {
	data, _ := os.ReadFile(path)
	return strings.TrimSpace(string(data))
}
