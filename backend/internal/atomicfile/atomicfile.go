// Package atomicfile replaces files so that neither a concurrent reader nor a
// power loss can observe a partially written result.
//
// The appliance stores its configuration and the SSH keys that are the only
// way back into a headless device on flash that can lose power at any moment,
// so durability matters here as much as atomicity.
package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
)

// Write replaces path with data. The data goes to a uniquely named temporary
// file in the same directory, is flushed to stable storage, and is then
// renamed over path. A missing parent directory is created with mode 0700;
// existing directories keep their permissions.
func Write(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".tmp-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	// Harmless once the rename below succeeded; removes the leftover on any
	// error path.
	defer os.Remove(temporary)
	if err := writeAndSync(file, data, mode); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	syncDirectory(directory)
	return nil
}

func writeAndSync(file *os.File, data []byte, mode os.FileMode) error {
	// CreateTemp always uses 0600. Applying the caller's mode explicitly keeps
	// the result independent of the umask and of any pre-existing file.
	if err := file.Chmod(mode); err != nil {
		return errors.Join(err, file.Close())
	}
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	return file.Close()
}

// syncDirectory makes the rename itself durable. It is best effort: the FAT
// boot partition that carries the GRUB slot selection does not support
// fsync on a directory, and there is no journal to make it meaningful there.
func syncDirectory(path string) {
	directory, err := os.Open(path)
	if err != nil {
		return
	}
	_ = directory.Sync()
	_ = directory.Close()
}
