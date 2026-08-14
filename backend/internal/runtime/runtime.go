package runtime

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/atomicfile"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
)

type Runner interface {
	Run(name string, args ...string) ([]byte, error)
}

// StdinRunner is implemented by runners that can feed a command on stdin,
// which keeps secrets such as a password out of the process list.
type StdinRunner interface {
	RunWithInput(input, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func (ExecRunner) RunWithInput(input, name string, args ...string) ([]byte, error) {
	command := exec.Command(name, args...)
	command.Stdin = strings.NewReader(input)
	return command.CombinedOutput()
}

type Manager struct {
	Store   *config.Store
	Dir     string
	DataDir string
	Run     Runner
}

const SetupAPPassword = "laserbridge-setup"

func (m *Manager) dataDir() string {
	if m.DataDir != "" {
		return m.DataDir
	}
	return "/data"
}

func (m *Manager) DataPath(parts ...string) string {
	return filepath.Join(append([]string{m.dataDir()}, parts...)...)
}

func (m *Manager) Apply() error {
	cfg, err := m.Store.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Dir, 0755); err != nil {
		return err
	}
	ser2net := fmt.Sprintf("connection: &grbl\n  accepter: tcp,%d\n  connector: serialdev,%s,%dn81,local\n  options:\n    max-connections: %d\n    kickolduser: %t\n",
		cfg.GRBL.Port, cfg.GRBL.Device, cfg.GRBL.Baudrate, cfg.GRBL.MaxConnections, cfg.GRBL.KickOldUser)
	if err := atomicfile.Write(filepath.Join(m.Dir, "ser2net.yaml"), []byte(ser2net), 0644); err != nil {
		return err
	}
	passwordAuth := "no"
	if cfg.SSH.PasswordAuthentication {
		passwordAuth = "yes"
	}
	sshd := fmt.Sprintf(`Port 22
ListenAddress 0.0.0.0
HostKey /data/ssh/ssh_host_ed25519_key
AuthorizedKeysFile /data/ssh/authorized_keys
PermitRootLogin no
PasswordAuthentication %s
KbdInteractiveAuthentication no
PermitEmptyPasswords no
UsePAM no
AllowUsers laserbridge
PidFile /run/sshd.pid
Subsystem sftp internal-sftp
`, passwordAuth)
	if err := atomicfile.Write(filepath.Join(m.Dir, "sshd_config"), []byte(sshd), 0600); err != nil {
		return err
	}
	mode := "ethernet"
	if !cfg.System.SetupComplete {
		mode = "ap"
	} else if cfg.WiFi.Enabled {
		mode = "client"
	}
	if err := atomicfile.Write(filepath.Join(m.Dir, "network-mode"), []byte(mode+"\n"), 0644); err != nil {
		return err
	}
	apSSID := strings.TrimSpace(readFile(m.DataPath("setup", "ap_ssid")))
	if apSSID == "" {
		apSSID = "LaserBridge-Setup"
	}
	hostapd := fmt.Sprintf("country_code=%s\ndriver=nl80211\nssid=%s\nhw_mode=g\nchannel=6\nwmm_enabled=1\nauth_algs=1\nwpa=2\nwpa_passphrase=%s\nwpa_key_mgmt=WPA-PSK\nrsn_pairwise=CCMP\n",
		cfg.WiFi.Country, apSSID, SetupAPPassword)
	if err := atomicfile.Write(filepath.Join(m.Dir, "hostapd.conf"), []byte(hostapd), 0600); err != nil {
		return err
	}
	dnsmasq := "bind-interfaces\nport=53\ndhcp-range=10.42.0.10,10.42.0.100,255.255.255.0,12h\ndhcp-option=3,10.42.0.1\ndhcp-option=6,10.42.0.1\naddress=/#/10.42.0.1\n"
	if err := atomicfile.Write(filepath.Join(m.Dir, "dnsmasq.conf"), []byte(dnsmasq), 0644); err != nil {
		return err
	}
	wpa := fmt.Sprintf("country=%s\nctrl_interface=/run/wpa_supplicant\nupdate_config=0\nnetwork={\n  ssid=\"%s\"\n  psk=\"%s\"\n  scan_ssid=%d\n}\n",
		cfg.WiFi.Country, wpaQuote(cfg.WiFi.SSID), wpaQuote(cfg.WiFi.PSK), boolInt(cfg.WiFi.Hidden))
	if err := atomicfile.Write(filepath.Join(m.Dir, "wpa_supplicant.conf"), []byte(wpa), 0600); err != nil {
		return err
	}
	if m.Run != nil {
		if out, err := m.Run.Run("hostname", cfg.System.Hostname); err != nil {
			return fmt.Errorf("set hostname: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (m *Manager) InitData() error {
	for _, dir := range []string{m.DataPath("ssh"), m.DataPath("home", "laserbridge"), m.DataPath("setup"), m.Dir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	// sshd resolves AuthorizedKeysFile as the target user. Keep the directory
	// traversable while the host private key inside remains mode 0600.
	if err := os.Chmod(m.DataPath("ssh"), 0755); err != nil {
		return err
	}
	quarantined, err := m.Store.Ensure()
	if err != nil {
		return err
	}
	if quarantined != "" {
		fmt.Fprintf(os.Stderr, "laserbridge: unreadable configuration moved to %s; defaults restored\n", quarantined)
	}
	// /etc/shadow is a symlink into /data, so the account database has to
	// exist before anything tries to authenticate against it.
	shadow := m.DataPath("shadow")
	if _, err := os.Stat(shadow); os.IsNotExist(err) {
		template, readErr := os.ReadFile(ShadowTemplate)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", ShadowTemplate, readErr)
		}
		if err := atomicfile.Write(shadow, template, 0640); err != nil {
			return err
		}
	}

	key := m.DataPath("ssh", "ssh_host_ed25519_key")
	if _, err := os.Stat(key); os.IsNotExist(err) {
		if m.Run == nil {
			return fmt.Errorf("cannot generate missing SSH host key without command runner")
		}
		if out, err := m.Run.Run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key); err != nil {
			return fmt.Errorf("generate SSH host key: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	authorized := m.DataPath("ssh", "authorized_keys")
	if file, err := os.OpenFile(authorized, os.O_CREATE, 0644); err != nil {
		return err
	} else {
		_ = file.Close()
	}
	if err := os.Chmod(authorized, 0644); err != nil {
		return err
	}
	clientKey := m.DataPath("setup", "laserbridge_ed25519")
	clientPublicKey := clientKey + ".pub"
	if _, err := os.Stat(clientKey); os.IsNotExist(err) {
		if m.Run == nil {
			return fmt.Errorf("cannot generate missing SSH client key without command runner")
		}
		if out, err := m.Run.Run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "laserbridge-initial", "-f", clientKey); err != nil {
			return fmt.Errorf("generate initial SSH key: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	if existing, _ := os.ReadFile(authorized); len(strings.TrimSpace(string(existing))) == 0 {
		publicKey, err := os.ReadFile(clientPublicKey)
		if err != nil {
			return fmt.Errorf("read generated SSH public key: %w", err)
		}
		if err := atomicfile.Write(authorized, append([]byte(strings.TrimSpace(string(publicKey))), '\n'), 0644); err != nil {
			return err
		}
	}
	apSSIDPath := m.DataPath("setup", "ap_ssid")
	if _, err := os.Stat(apSSIDPath); os.IsNotExist(err) {
		random := make([]byte, 3)
		if _, err := rand.Read(random); err != nil {
			return fmt.Errorf("generate setup AP suffix: %w", err)
		}
		if err := atomicfile.Write(apSSIDPath, []byte("LaserBridge-"+strings.ToUpper(hex.EncodeToString(random))+"\n"), 0600); err != nil {
			return err
		}
	}
	if m.Run != nil {
		_, _ = m.Run.Run("chown", "-R", "laserbridge:laserbridge", m.DataPath("home", "laserbridge"))
	}
	return m.Apply()
}

// DefaultPassword is what the image ships with for the laserbridge account.
// It is documented, identical on every image, and therefore a convenience
// rather than a secret; the setup wizard offers to replace it.
const DefaultPassword = "laserbridge"

// DefaultPasswordHash is what prepare-rootfs.sh writes into the shipped
// account database. Recognising it is how the appliance can tell that nobody
// has chosen a password yet, which gates the operations that trust one.
const DefaultPasswordHash = "$6$LaserBridgeOS$5w7ZyIhzoG.NAVmvorgxpRKC.XGg6qvMpguJYnLjUmnnR6l/4K22zqUDEJzxDLz70DhXx89G6igxNYJLXeczQ0"

// StoredPasswordHash returns the crypt hash of the laserbridge account.
func (m *Manager) StoredPasswordHash() (string, error) {
	data, err := os.ReadFile(m.DataPath("shadow"))
	if err != nil {
		return "", fmt.Errorf("read account database: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= 2 && fields[0] == "laserbridge" {
			return fields[1], nil
		}
	}
	return "", errors.New("no laserbridge entry in the account database")
}

// PasswordIsDefault reports whether the account still carries the password
// every image ships with.
func (m *Manager) PasswordIsDefault() bool {
	hash, err := m.StoredPasswordHash()
	return err == nil && hash == DefaultPasswordHash
}

// VerifyPassword checks a candidate against the stored hash by recomputing it
// with the same salt. The comparison is constant time so a caller cannot
// learn the hash one byte at a time.
func (m *Manager) VerifyPassword(candidate string) (bool, error) {
	stored, err := m.StoredPasswordHash()
	if err != nil {
		return false, err
	}
	// $<id>$<salt>$<hash>
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[1] != "6" {
		return false, errors.New("stored password is not in a supported format")
	}
	runner, ok := m.Run.(StdinRunner)
	if !ok || m.Run == nil {
		return false, errors.New("cannot verify a password without a command runner")
	}
	out, err := runner.RunWithInput(candidate+"\n", "cryptpw", "-m", "sha512", "-S", parts[2], "-P", "0")
	if err != nil {
		return false, fmt.Errorf("hash password: %w: %s", err, strings.TrimSpace(string(out)))
	}
	computed := strings.TrimSpace(string(out))
	return subtle.ConstantTimeCompare([]byte(computed), []byte(stored)) == 1, nil
}

// ShadowTemplate is the account database shipped in the image. The first boot
// copies it to DataPath("shadow"), which /etc/shadow points at.
const ShadowTemplate = "/etc/laserbridge/shadow.default"

// SetPassword changes the laserbridge account's password.
//
// It does not use chpasswd: that tool rewrites /etc/shadow in place, and the
// root filesystem is read-only. The account database lives in /data instead,
// so the new hash is written there the same way every other piece of
// persistent state is written.
func (m *Manager) SetPassword(password string) error {
	runner, ok := m.Run.(StdinRunner)
	if !ok || m.Run == nil {
		return fmt.Errorf("cannot set a password without a command runner")
	}
	// The password goes to cryptpw on stdin so it never reaches the process
	// list. Without -S the salt is random, which is what we want at runtime.
	out, err := runner.RunWithInput(password+"\n", "cryptpw", "-m", "sha512", "-P", "0")
	if err != nil {
		return fmt.Errorf("hash password: %w: %s", err, strings.TrimSpace(string(out)))
	}
	hash := strings.TrimSpace(string(out))
	if !strings.HasPrefix(hash, "$6$") || strings.ContainsAny(hash, ": \n") {
		return fmt.Errorf("cryptpw returned an unusable hash")
	}

	path := m.DataPath("shadow")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read account database: %w", err)
	}
	updated, replaced := replaceShadowHash(string(data), "laserbridge", hash)
	if !replaced {
		return fmt.Errorf("no laserbridge entry in %s", path)
	}
	return atomicfile.Write(path, []byte(updated), 0640)
}

func replaceShadowHash(shadow, user, hash string) (string, bool) {
	lines := strings.Split(shadow, "\n")
	replaced := false
	for i, line := range lines {
		fields := strings.Split(line, ":")
		if len(fields) < 2 || fields[0] != user {
			continue
		}
		fields[1] = hash
		lines[i] = strings.Join(fields, ":")
		replaced = true
	}
	return strings.Join(lines, "\n"), replaced
}

func (m *Manager) UstreamerArgs(cfg config.Config, format string) []string {
	return []string{
		"--device=" + cfg.Camera.Device,
		"--format=" + format,
		"--resolution=" + cfg.Camera.Resolution,
		"--desired-fps=" + strconv.Itoa(cfg.Camera.FPS),
		"--host=0.0.0.0",
		"--port=" + strconv.Itoa(cfg.Camera.Port),
		"--device-timeout=5",
		"--device-error-delay=1",
		"--quality=" + strconv.Itoa(cfg.Camera.Quality),
	}
}

func (m *Manager) RunUstreamer() error {
	cfg, err := m.Store.Load()
	if err != nil {
		return err
	}
	format := cfg.Camera.Format
	if format == "MJPEG" && m.Run != nil {
		if out, err := m.Run.Run("v4l2-ctl", "--device", cfg.Camera.Device, "--list-formats-ext"); err == nil && !strings.Contains(strings.ToUpper(string(out)), "MJPG") {
			format = "YUYV"
		}
	}
	args := m.UstreamerArgs(cfg, format)
	return syscall.Exec("/usr/bin/ustreamer", append([]string{"ustreamer"}, args...), os.Environ())
}

func readFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func wpaQuote(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
