package update

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeVerifier struct {
	err    error
	called bool
}

func (v *fakeVerifier) Verify(_, _, _, _ string) error {
	v.called = true
	return v.err
}

func createBundle(t *testing.T, directory string, corrupt bool) (string, map[string][]byte) {
	t.Helper()
	payloads := map[string][]byte{
		"root.squashfs": []byte("hsqs-new-root"),
		"vmlinuz-lts":   []byte("new-kernel"),
		"initramfs-lts": []byte("new-initramfs"),
	}
	manifest := Manifest{FormatVersion: 1, Architecture: "x86_64", Version: "0.2.0", Files: map[string]FileSpec{}}
	for name, payload := range payloads {
		digest := sha256.Sum256(payload)
		manifest.Files[name] = FileSpec{SHA256: hex.EncodeToString(digest[:]), Size: int64(len(payload))}
	}
	if corrupt {
		spec := manifest.Files["vmlinuz-lts"]
		spec.SHA256 = string(make([]byte, 64))
		manifest.Files["vmlinuz-lts"] = spec
	}
	manifestData, _ := json.Marshal(manifest)
	path := filepath.Join(directory, "update.lbu")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	entries := map[string][]byte{"manifest.json": manifestData}
	for name, payload := range payloads {
		entries[name] = payload
	}
	for _, name := range []string{"manifest.json", "root.squashfs", "vmlinuz-lts", "initramfs-lts"} {
		writer, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entries[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path, payloads
}

func testManager(t *testing.T, verifier SignatureVerifier) (*Manager, string, string, string) {
	t.Helper()
	directory := t.TempDir()
	dataDir := filepath.Join(directory, "data")
	bootDir := filepath.Join(directory, "boot-volume")
	rootA := filepath.Join(directory, "root-a")
	rootB := filepath.Join(directory, "root-b")
	for _, slot := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(bootDir, "boot", "slot-"+slot), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bootDir, "boot", "slot-"+slot, "vmlinuz-lts"), []byte("old-kernel-"+slot), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(rootA, []byte("old-root-a"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootB, []byte("old-root-b"), 0600); err != nil {
		t.Fatal(err)
	}
	cmdline := filepath.Join(directory, "cmdline")
	if err := os.WriteFile(cmdline, []byte("root=LASERBRIDGE_ROOT_A laserbridge.slot=a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	version := filepath.Join(directory, "version")
	if err := os.WriteFile(version, []byte("0.1.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		DataDir: dataDir, RuntimeDir: filepath.Join(directory, "run"), VersionPath: version,
		CmdlinePath: cmdline, BootDir: bootDir, RootAPath: rootA, RootBPath: rootB,
		AuthorizedKeysPath: filepath.Join(directory, "authorized_keys"), Verifier: verifier,
		Now: func() time.Time { return time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC) },
	}
	return manager, directory, rootA, rootB
}

func TestInstallWritesOnlyInactiveSlotAndCommitsLast(t *testing.T) {
	verifier := &fakeVerifier{}
	manager, directory, rootA, rootB := testManager(t, verifier)
	bundle, payloads := createBundle(t, directory, false)
	signature := filepath.Join(directory, "update.lbu.sig")
	if err := os.WriteFile(signature, []byte("signature"), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := manager.Install(bundle, signature)
	if err != nil {
		t.Fatal(err)
	}
	if !verifier.called || state.TargetSlot != "b" || state.SourceSlot != "a" || state.Version != "0.2.0" {
		t.Fatalf("state = %#v, verifier called = %t", state, verifier.called)
	}
	oldRoot, _ := os.ReadFile(rootA)
	newRoot, _ := os.ReadFile(rootB)
	if string(oldRoot) != "old-root-a" || string(newRoot) != string(payloads["root.squashfs"]) {
		t.Fatalf("root slots: A=%q B=%q", oldRoot, newRoot)
	}
	kernel, _ := os.ReadFile(filepath.Join(manager.BootDir, "boot", "slot-b", "vmlinuz-lts"))
	active, _ := os.ReadFile(filepath.Join(manager.BootDir, "boot", "active-slot.cfg"))
	if string(kernel) != string(payloads["vmlinuz-lts"]) || string(active) != "set laserbridge_slot=b\n" {
		t.Fatalf("kernel=%q active=%q", kernel, active)
	}
	status := manager.Status()
	if !status.RebootRequired || status.StagedSlot != "b" || status.CurrentSlot != "a" {
		t.Fatalf("status before reboot = %#v", status)
	}
}

func TestRejectedBundleDoesNotTouchInactiveSlot(t *testing.T) {
	manager, directory, _, rootB := testManager(t, &fakeVerifier{})
	bundle, _ := createBundle(t, directory, true)
	signature := filepath.Join(directory, "update.lbu.sig")
	_ = os.WriteFile(signature, []byte("signature"), 0600)
	if _, err := manager.Install(bundle, signature); err == nil {
		t.Fatal("corrupt update was accepted")
	}
	data, _ := os.ReadFile(rootB)
	if string(data) != "old-root-b" {
		t.Fatalf("inactive slot changed to %q", data)
	}
}

func TestSignatureFailureHappensBeforeBundleInstall(t *testing.T) {
	manager, directory, _, rootB := testManager(t, &fakeVerifier{err: errors.New("bad signature")})
	bundle, _ := createBundle(t, directory, false)
	signature := filepath.Join(directory, "update.lbu.sig")
	_ = os.WriteFile(signature, []byte("signature"), 0600)
	if _, err := manager.Install(bundle, signature); err == nil {
		t.Fatal("unsigned update was accepted")
	}
	data, _ := os.ReadFile(rootB)
	if string(data) != "old-root-b" {
		t.Fatalf("inactive slot changed to %q", data)
	}
}

func grubEnvironment(t *testing.T, manager *Manager) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(manager.BootDir, "boot", "grubenv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != grubEnvironmentSize {
		t.Fatalf("grubenv is %d bytes, want %d; GRUB rewrites it in place", len(data), grubEnvironmentSize)
	}
	return string(data)
}

func TestConfirmBootResetsCounterAndKeepsStagedUpdate(t *testing.T) {
	manager, _, _, _ := testManager(t, &fakeVerifier{})
	activePath := filepath.Join(manager.BootDir, "boot", "active-slot.cfg")
	if err := os.WriteFile(activePath, []byte("set laserbridge_slot=a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(manager.DataDir, "update", "state.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
		t.Fatal(err)
	}
	// Staged for the other slot: the pending update has simply not been
	// rebooted into yet and must survive an ordinary healthy boot.
	if err := os.WriteFile(statePath, []byte(`{"version":"0.2.0","target_slot":"b"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.ConfirmBoot(); err != nil {
		t.Fatal(err)
	}
	environment := grubEnvironment(t, manager)
	for _, expected := range []string{"# GRUB Environment Block\n", "laserbridge_try=0\n", "laserbridge_override=\n"} {
		if !strings.Contains(environment, expected) {
			t.Fatalf("grubenv lacks %q:\n%s", expected, environment)
		}
	}
	active, _ := os.ReadFile(activePath)
	if string(active) != "set laserbridge_slot=a\n" {
		t.Fatalf("active slot changed to %q", active)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("staged update was dropped on a healthy boot: %v", err)
	}
}

func TestConfirmBootAdoptsSlotAfterGrubFellBack(t *testing.T) {
	manager, _, _, _ := testManager(t, &fakeVerifier{})
	// An update was staged into slot B and activated, but B never booted, so
	// GRUB fell back to A - which is the slot now running per CmdlinePath.
	activePath := filepath.Join(manager.BootDir, "boot", "active-slot.cfg")
	if err := os.WriteFile(activePath, []byte("set laserbridge_slot=b\n"), 0644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(manager.DataDir, "update", "state.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(`{"version":"0.2.0","target_slot":"b"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.ConfirmBoot(); err != nil {
		t.Fatal(err)
	}
	active, _ := os.ReadFile(activePath)
	if string(active) != "set laserbridge_slot=a\n" {
		t.Fatalf("active slot = %q, want the recovered slot a", active)
	}
	// The record stays, without the target: an update that could not boot must
	// not be asked for again, but what the slots hold is worth keeping - it is
	// what the interface shows next to "boot previous slot".
	var state State
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("the record of what the slots hold was thrown away: %v", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.TargetSlot != "" {
		t.Fatalf("the update that could not boot is still staged: %+v", state)
	}
	if state.Slots["a"] != "0.1.0" {
		t.Errorf("the running slot was not recorded: %+v", state.Slots)
	}
	if status := manager.Status(); status.RebootRequired {
		t.Fatalf("status still asks for a reboot into the broken slot: %#v", status)
	}
}

// The question this answers came from the workshop: why does the page show the
// same version as running and as staged?
//
// Because "staged" described the last installation rather than a pending one.
// An appliance that installed 0.1.7 into slot B and rebooted into it is running
// 0.1.7 in slot B, and there is nothing waiting for anybody.
func TestAnInstallationThatHasBootedIsNotStaged(t *testing.T) {
	manager, _, _, _ := testManager(t, &fakeVerifier{})
	statePath := filepath.Join(manager.DataDir, "update", "state.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
		t.Fatal(err)
	}
	// Installed into a, and a is what is running.
	if err := os.WriteFile(statePath, []byte(`{"version":"0.2.0","target_slot":"a","slots":{"a":"0.2.0","b":"0.1.9"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if status.RebootRequired {
		t.Error("a reboot is asked for although the installation has already booted")
	}
	if status.StagedVersion != "" || status.StagedSlot != "" {
		t.Errorf("staged = %q in slot %q, want nothing pending", status.StagedVersion, status.StagedSlot)
	}
	// And the question behind the question: what would a rollback boot into?
	if status.PreviousVersion != "0.1.9" {
		t.Errorf("previous slot holds %q, want 0.1.9", status.PreviousVersion)
	}
}

func TestARollbackNamesTheVersionItWouldBootInto(t *testing.T) {
	manager, _, _, _ := testManager(t, &fakeVerifier{})
	statePath := filepath.Join(manager.DataDir, "update", "state.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(`{"version":"previous-slot","target_slot":"b","slots":{"a":"0.2.0","b":"0.1.9"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if !status.RebootRequired || status.StagedSlot != "b" {
		t.Fatalf("status = %#v, want a pending switch to slot b", status)
	}
	// "previous-slot" is what the record holds, because a rollback carries no
	// bundle. It is not what anybody wants to read on a page.
	if status.StagedVersion != "0.1.9" {
		t.Errorf("staged version = %q, want the version that is actually over there", status.StagedVersion)
	}
}

func TestRollbackSelectsOtherBootableSlot(t *testing.T) {
	manager, _, _, _ := testManager(t, &fakeVerifier{})
	target, err := manager.StageRollback()
	if err != nil {
		t.Fatal(err)
	}
	active, _ := os.ReadFile(filepath.Join(manager.BootDir, "boot", "active-slot.cfg"))
	if target != "b" || string(active) != "set laserbridge_slot=b\n" {
		t.Fatalf("target=%q active=%q", target, active)
	}
}
