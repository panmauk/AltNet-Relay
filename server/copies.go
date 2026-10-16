package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Signing in on another computer.
//
// The app keeps a locked copy of the account here -- its keys and its saved
// state, sealed on the person's computer with their password (package
// backup) -- so signing in elsewhere can bring everything along. This server
// stores the bytes and never sees the password, so it can't open them.
//
// An account is in use on one computer at a time: two computers writing to
// the same conversations would put messages out of order. Each install has a
// random device ID; signing in claims the account for that device
// (POST /v1/device), /v1/me reports which device holds it, and the computer
// that lost it signs itself out. A copy may only be replaced by the device
// that holds the account, so a computer that has been signed out elsewhere
// can't overwrite the newer copy.
const (
	// MaxCopySize bounds one locked copy (backup.MaxSize on the app side).
	MaxCopySize = 8 << 20
	// deviceHeader names the device a copy comes from.
	deviceHeader = "X-Relay-Device"
)

// CopyMinGap is the shortest gap between two saves of one account's copy.
// A variable so tests elsewhere can shorten it.
var CopyMinGap = 15 * time.Second

func (s *Server) copyPath(email string) string {
	sum := sha256.Sum256([]byte("relay-copy:" + email))
	return filepath.Join(s.store.Dir(), "copies", hex.EncodeToString(sum[:])+".bin")
}

func validDevice(d string) bool { return len(d) >= 16 && len(d) <= 64 && isHex(strings.ToLower(d)) }

// handleDevice makes this device the one the account is in use on.
func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request, p Principal) {
	var req struct {
		Device string `json:"device"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Device = strings.ToLower(strings.TrimSpace(req.Device))
	if !validDevice(req.Device) {
		writeError(w, http.StatusBadRequest, "that isn't a valid device")
		return
	}
	var was string
	err := s.store.Update(func(d *fileData) error {
		a := d.accountFor(p.Email)
		if a.Banned {
			return errBanned(a)
		}
		was = a.Device
		a.Device, a.DeviceAt = req.Device, s.now().Unix()
		return nil
	})
	if s.fail(w, err) {
		return
	}
	if was != req.Device {
		s.store.Audit("device", map[string]any{"email": p.Email, "device": req.Device[:8]})
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleGetBackup hands back the account's locked copy.
func (s *Server) handleGetBackup(w http.ResponseWriter, _ *http.Request, p Principal) {
	blob, err := os.ReadFile(s.copyPath(p.Email))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "there's no copy of this account")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

// handlePutBackup replaces the locked copy, if it comes from the device the
// account is in use on.
func (s *Server) handlePutBackup(w http.ResponseWriter, r *http.Request, p Principal) {
	device := strings.ToLower(strings.TrimSpace(r.Header.Get(deviceHeader)))
	if !validDevice(device) {
		writeError(w, http.StatusBadRequest, "which computer is this copy from?")
		return
	}
	var refuse error
	s.store.View(func(d *fileData) {
		a := d.Accounts[p.Email]
		switch {
		case a == nil:
			refuse = errConflict("sign in on this computer first")
		case a.Banned:
			refuse = errBanned(a)
		case a.Device != device:
			refuse = errConflict("this account is now in use on another computer")
		case a.BackupAt != 0 && s.now().Sub(time.Unix(a.BackupAt, 0)) < CopyMinGap:
			refuse = errTooMany("saved a moment ago; try again shortly")
		}
	})
	if s.fail(w, refuse) {
		return
	}
	blob, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxCopySize))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "this account is too large to copy")
		return
	}
	if len(blob) < 64 || !strings.HasPrefix(string(blob), "ANRB") {
		writeError(w, http.StatusBadRequest, "that isn't a locked copy")
		return
	}
	path := s.copyPath(p.Email)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		s.fail(w, err)
		return
	}
	if err := os.WriteFile(path+".tmp", blob, 0o600); err != nil {
		s.fail(w, err)
		return
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		s.fail(w, err)
		return
	}
	_ = s.store.Update(func(d *fileData) error {
		if a := d.Accounts[p.Email]; a != nil {
			a.BackupAt, a.BackupSize = s.now().Unix(), int64(len(blob))
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeleteBackup removes the copy: the person turned it off.
func (s *Server) handleDeleteBackup(w http.ResponseWriter, _ *http.Request, p Principal) {
	s.removeCopy(p.Email)
	s.store.Audit("copy removed", map[string]any{"email": p.Email})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) removeCopy(email string) {
	_ = os.Remove(s.copyPath(email))
	_ = s.store.Update(func(d *fileData) error {
		if a := d.Accounts[email]; a != nil {
			a.BackupAt, a.BackupSize = 0, 0
		}
		return nil
	})
}
