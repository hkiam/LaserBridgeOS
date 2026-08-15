package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/atomicfile"
)

type Store struct {
	path string
	mu   sync.RWMutex
	// notes is what the last Ensure could not make sense of, kept so the web
	// interface can say it out loud. Ensure runs at boot and again when the
	// backend starts, so this is never older than the running process.
	notes []string
}

func NewStore(path string) *Store { return &Store{path: path} }

func (s *Store) Path() string { return s.path }

// Ensure makes sure the store holds a configuration that Load can read. A
// missing file is created from Default.
//
// A file that no longer parses or validates is moved aside; Ensure then returns
// the path it was moved to and keeps what Salvage could rescue from it. This
// appliance is headless with no console login, and every service depends on the
// initializer that calls Ensure. Refusing to start on an unreadable
// configuration - which a stricter validation rule in a later release is
// enough to cause - would take away the web interface and SSH along with it,
// leaving no way in at all. Losing customised settings is recoverable;
// losing all access is not.
func (s *Store) Ensure() (quarantined string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if _, notes, parseErr := ParseYAML(data); parseErr == nil {
			s.notes = notes
			// Deliberately not rewritten, even when there were notes. Whatever
			// this version did not understand was written by one that did, and
			// leaving it in place is what lets the newer slot find its settings
			// where it left them when it is booted again.
			return "", nil
		}
		quarantined = s.path + ".broken"
		if err := atomicfile.Write(quarantined, data, 0640); err != nil {
			return "", err
		}
		// Everything the file still says, judged afterwards rather than
		// discarded because one line was wrong.
		broken, _, structureErr := parseYAML(data)
		if structureErr != nil {
			broken = Default()
		}
		if err := s.saveLocked(Salvage(broken)); err != nil {
			return quarantined, err
		}
		// After the save, which clears the notes of the file it replaced.
		s.notes = []string{"the stored configuration could not be used and was moved to " +
			quarantined + "; the network settings were kept and everything else is back to its default"}
		return quarantined, nil
	case errors.Is(err, os.ErrNotExist):
	default:
		return "", err
	}
	s.notes = nil
	return quarantined, s.saveLocked(Default())
}

// Notes is what the stored configuration contained that this version did not
// understand. The web interface reports it: a setting that is being ignored is
// worth saying out loud, and after a rollback it is the difference between "the
// appliance forgot my settings" and "this version is older than the file".
func (s *Store) Notes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.notes...)
}

func (s *Store) Load() (Config, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	c, _, err := ParseYAML(data)
	if err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return c, nil
}

func (s *Store) Save(c Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(c)
}

func (s *Store) saveLocked(c Config) error {
	data, err := MarshalYAML(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0750); err != nil {
		return err
	}
	if err := atomicfile.Write(s.path, data, 0640); err != nil {
		return err
	}
	// A file this version just wrote holds nothing this version does not
	// understand, including the keys it dropped on the way, so the warning goes
	// with them. After the write, not before: a save that failed leaves the old
	// file in place, and its warning is still true.
	s.notes = nil
	return nil
}
