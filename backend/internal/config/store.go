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
}

func NewStore(path string) *Store { return &Store{path: path} }

func (s *Store) Path() string { return s.path }

// Ensure makes sure the store holds a configuration that Load can read. A
// missing file is created from Default.
//
// A file that no longer parses or validates is moved aside and replaced by
// the defaults; Ensure then returns the path it was moved to. This appliance
// is headless with no console login, and every service depends on the
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
		if _, parseErr := ParseYAML(data); parseErr == nil {
			return "", nil
		}
		quarantined = s.path + ".broken"
		if err := atomicfile.Write(quarantined, data, 0640); err != nil {
			return "", err
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return "", err
	}
	return quarantined, s.saveLocked(Default())
}

func (s *Store) Load() (Config, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	c, err := ParseYAML(data)
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
	return atomicfile.Write(s.path, data, 0640)
}
