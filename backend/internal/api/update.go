package api

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"

	lbupdate "github.com/laserbridgeos/laserbridgeos/backend/internal/update"
)

func (s *Server) updateStatus(w http.ResponseWriter, _ *http.Request) {
	if s.Updater == nil {
		writeError(w, http.StatusServiceUnavailable, "system updates are unavailable")
		return
	}
	writeJSON(w, http.StatusOK, s.Updater.Status())
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
	bundlePath, signaturePath, err := receiveUpdateParts(reader, uploadDir)
	if bundlePath != "" {
		defer os.Remove(bundlePath)
	}
	if signaturePath != "" {
		defer os.Remove(signaturePath)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	state, err := s.Updater.Install(bundlePath, signaturePath)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Printf("update rejected: %v", err)
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "update-staged", "update": state})
}

func receiveUpdateParts(reader *multipart.Reader, directory string) (bundlePath, signaturePath string, resultErr error) {
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return bundlePath, signaturePath, fmt.Errorf("read update upload: %w", err)
		}
		name := part.FormName()
		var limit int64
		var destination *string
		switch name {
		case "bundle":
			limit = lbupdate.MaxBundleSize
			destination = &bundlePath
		case "signature":
			limit = lbupdate.MaxSignatureSize
			destination = &signaturePath
		default:
			part.Close()
			return bundlePath, signaturePath, fmt.Errorf("unexpected upload field %q", name)
		}
		if *destination != "" {
			part.Close()
			return bundlePath, signaturePath, fmt.Errorf("duplicate upload field %q", name)
		}
		file, err := os.CreateTemp(directory, name+"-*")
		if err != nil {
			part.Close()
			return bundlePath, signaturePath, err
		}
		path := file.Name()
		_ = os.Chmod(path, 0600)
		written, copyErr := io.Copy(file, io.LimitReader(part, limit+1))
		syncErr := file.Sync()
		closeErr := file.Close()
		partCloseErr := part.Close()
		if err := errors.Join(copyErr, syncErr, closeErr, partCloseErr); err != nil {
			_ = os.Remove(path)
			return bundlePath, signaturePath, err
		}
		if written > limit {
			_ = os.Remove(path)
			return bundlePath, signaturePath, fmt.Errorf("%s exceeds the upload limit", name)
		}
		*destination = filepath.Clean(path)
	}
	if bundlePath == "" || signaturePath == "" {
		return bundlePath, signaturePath, errors.New("both bundle and detached SSH signature are required")
	}
	return bundlePath, signaturePath, nil
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
