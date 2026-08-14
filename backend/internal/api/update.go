package api

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	lbupdate "github.com/laserbridgeos/laserbridgeos/backend/internal/update"
)

func (s *Server) updateStatus(w http.ResponseWriter, _ *http.Request) {
	if s.Updater == nil {
		writeError(w, http.StatusServiceUnavailable, "system updates are unavailable")
		return
	}
	status := s.Updater.Status()
	// The web interface needs to know which authorisation it can offer, and
	// to say why the password option is missing when it is.
	status.PasswordAccepted = s.Runtime != nil && !s.Runtime.PasswordIsDefault()
	status.DefaultPasswordInUse = s.Runtime != nil && s.Runtime.PasswordIsDefault()
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) installUpdate(w http.ResponseWriter, r *http.Request) {
	if s.Updater == nil {
		writeError(w, http.StatusServiceUnavailable, "system updates are unavailable")
		return
	}
	cfg, err := s.Store.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read appliance setup state")
		return
	}
	if !cfg.System.SetupComplete {
		writeError(w, http.StatusConflict, "complete first-boot setup before installing updates")
		return
	}
	if !s.updateMu.TryLock() {
		writeError(w, http.StatusConflict, "another update is already running")
		return
	}
	defer s.updateMu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, lbupdate.MaxBundleSize+lbupdate.MaxSignatureSize+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected multipart update upload")
		return
	}
	uploadDir := s.dataPath("update", "uploads")
	if err := os.MkdirAll(uploadDir, 0700); err != nil {
		writeError(w, http.StatusInternalServerError, "could not prepare update storage")
		return
	}
	upload, err := receiveUpdateParts(reader, uploadDir, s.checkUpdatePassword)
	if upload.bundle != "" {
		defer os.Remove(upload.bundle)
	}
	if upload.signature != "" {
		defer os.Remove(upload.signature)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// An update is authorised either by a signature the appliance can verify
	// against an authorized key, or by the appliance password. Requiring a
	// signature outright would leave an appliance set up with a password only
	// unable to update at all - it has no private key to sign with.
	if upload.signature == "" && !upload.passwordAccepted {
		writeError(w, http.StatusUnauthorized,
			"provide the appliance password, or a detached signature from an authorized key")
		return
	}
	state, err := s.Updater.Install(upload.bundle, upload.signature)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Printf("update rejected: %v", err)
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "update-staged", "update": state})
}

type updateUpload struct {
	bundle           string
	signature        string
	passwordAccepted bool
}

// checkUpdatePassword decides whether a password may authorise an update.
// The shipped default is refused: it is printed in the README, so accepting
// it would let anyone on the network install firmware.
func (s *Server) checkUpdatePassword(candidate string) error {
	if s.Runtime == nil {
		return errors.New("password authorisation is unavailable")
	}
	if s.Runtime.PasswordIsDefault() {
		return errors.New("this appliance still has its default password; change it before installing updates with one")
	}
	ok, err := s.Runtime.VerifyPassword(candidate)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Printf("verify update password: %v", err)
		}
		return errors.New("could not check the password")
	}
	if !ok {
		// Slow down guessing a little. The endpoint is reachable by anyone on
		// the local network, and the password is the only thing guarding it.
		time.Sleep(time.Second)
		return errors.New("wrong password")
	}
	return nil
}

// receiveUpdateParts streams the multipart upload to disk. The password field
// is checked the moment it arrives, so a wrong one fails before the caller
// has pushed a few hundred megabytes of bundle across the network - provided
// the client sends it first, which the web interface does.
func receiveUpdateParts(reader *multipart.Reader, directory string, checkPassword func(string) error) (updateUpload, error) {
	var upload updateUpload
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return upload, fmt.Errorf("read update upload: %w", err)
		}
		name := part.FormName()

		if name == "password" {
			value, readErr := io.ReadAll(io.LimitReader(part, 1<<10))
			part.Close()
			if readErr != nil {
				return upload, readErr
			}
			if err := checkPassword(strings.TrimRight(string(value), "\r\n")); err != nil {
				return upload, err
			}
			upload.passwordAccepted = true
			continue
		}

		var limit int64
		var destination *string
		switch name {
		case "bundle":
			limit = lbupdate.MaxBundleSize
			destination = &upload.bundle
		case "signature":
			limit = lbupdate.MaxSignatureSize
			destination = &upload.signature
		default:
			part.Close()
			return upload, fmt.Errorf("unexpected upload field %q", name)
		}
		if *destination != "" {
			part.Close()
			return upload, fmt.Errorf("duplicate upload field %q", name)
		}
		file, err := os.CreateTemp(directory, name+"-*")
		if err != nil {
			part.Close()
			return upload, err
		}
		path := file.Name()
		_ = os.Chmod(path, 0600)
		written, copyErr := io.Copy(file, io.LimitReader(part, limit+1))
		syncErr := file.Sync()
		closeErr := file.Close()
		partCloseErr := part.Close()
		if err := errors.Join(copyErr, syncErr, closeErr, partCloseErr); err != nil {
			_ = os.Remove(path)
			return upload, err
		}
		if written > limit {
			_ = os.Remove(path)
			return upload, fmt.Errorf("%s exceeds the upload limit", name)
		}
		*destination = filepath.Clean(path)
	}
	if upload.bundle == "" {
		return upload, errors.New("the update bundle is required")
	}
	return upload, nil
}

func (s *Server) rollbackUpdate(w http.ResponseWriter, _ *http.Request) {
	if s.Updater == nil {
		writeError(w, http.StatusServiceUnavailable, "system updates are unavailable")
		return
	}
	target, err := s.Updater.StageRollback()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "rollback-staged", "target_slot": target})
}
