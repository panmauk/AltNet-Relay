package main

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"

	"altnet-relay/engine"
)

// Signing in on another computer: the window's side. See engine/roaming.go.

// afterFlag tells a restarted copy of the app to wait for the old one to
// finish, so the two never hold the data folder at the same time.
const afterFlag = "--after"

// SignIn signs in. Besides "ok" it can answer:
//   - "restart": the account's copy was brought here; the app restarts itself
//     in a moment to load it;
//   - "no_copy" / "old_copy": the account lives on another computer and its
//     copy can't be brought here; the window offers to start fresh.
func (a *App) SignIn(email, password string) (string, error) {
	e, err := a.ready()
	if err != nil {
		return "", err
	}
	err = e.SignIn(email, password)
	switch {
	case errors.Is(err, engine.ErrRestart):
		go func() {
			time.Sleep(1500 * time.Millisecond) // let the window say what's happening
			a.restart()
		}()
		return "restart", nil
	case errors.Is(err, engine.ErrNoCopy):
		return "no_copy", nil
	case errors.Is(err, engine.ErrOldCopy):
		return "old_copy", nil
	case err != nil:
		return "", err
	}
	return "ok", nil
}

// StartFresh moves the account to this computer's own keys: same @name,
// earlier conversations left behind.
func (a *App) StartFresh(email, password string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	return e.StartFresh(email, password)
}

// CopyView is the Account screen's line about the account's copy.
type CopyView struct {
	On            bool   `json:"on"`
	NeedsPassword bool   `json:"needsPassword"`
	SavedAt       int64  `json:"savedAt"`
	Problem       string `json:"problem"`
}

func (a *App) Copy() (CopyView, error) {
	e, err := a.ready()
	if err != nil {
		return CopyView{}, err
	}
	c := e.Copy()
	return CopyView{On: c.On, NeedsPassword: c.NeedsPassword, SavedAt: c.SavedAt, Problem: c.Problem}, nil
}

// SetCopy turns the account's copy on or off (off deletes it from the server).
func (a *App) SetCopy(on bool) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	return e.SetCopy(on)
}

// UnlockCopy takes the password once, for a computer that signed in before
// copies existed.
func (a *App) UnlockCopy(password string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	return e.UnlockCopy(password)
}

// restart starts a fresh copy of the app and quits this one. The new copy
// waits for this one to be gone before it opens the data folder.
func (a *App) restart() {
	exe, err := os.Executable()
	if err == nil {
		cmd := exec.Command(exe, afterFlag, strconv.Itoa(os.Getpid()))
		if err = cmd.Start(); err == nil {
			_ = cmd.Process.Release()
		}
	}
	a.Quit()
}

// waitForPrevious is the new copy's side of restart: it waits (a little
// while at most) for the old copy to exit.
func waitForPrevious(args []string) {
	for i, arg := range args {
		if arg != afterFlag || i+1 >= len(args) {
			continue
		}
		pid, err := strconv.Atoi(args[i+1])
		if err != nil || pid <= 0 {
			return
		}
		waitForExit(pid, 20*time.Second)
		return
	}
}
