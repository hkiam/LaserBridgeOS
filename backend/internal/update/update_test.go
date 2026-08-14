package update

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
