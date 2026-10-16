package server

import (
	"context"
	"crypto/ed25519"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"altnet-relay/updater"
)

// UpdateMirror keeps the latest signed release notice (update.json from the
// newest GitHub release) and hands it to apps at GET /v1/update. Apps check
// here rather than at GitHub, so GitHub doesn't see every running app every
// few hours; they only go to GitHub to download an update they've decided
// to install.
//
// The notice is checked against the release key before it is kept, so a
// mistaken upload is never passed on (the apps check it again anyway).
type UpdateMirror struct {
	Source string // e.g. https://github.com/OWNER/REPO/releases/latest/download/update.json
	Every  time.Duration
	Log    *slog.Logger
	HTTP   *http.Client
	Key    ed25519.PublicKey // the release key; nil means the one built in

	mu   sync.Mutex
	body []byte
}

// Run refreshes the notice until ctx ends.
func (m *UpdateMirror) Run(ctx context.Context) {
	every := m.Every
	if every == 0 {
		every = 10 * time.Minute
	}
	for {
		m.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (m *UpdateMirror) refresh(ctx context.Context) {
	client := m.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Source, nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		m.logf("couldn't fetch the release notice", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode != http.StatusNotFound { // nothing released yet is normal
			m.logf("release notice fetch failed", "status", resp.StatusCode)
		}
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, updater.MaxManifest+1))
	if err != nil || len(raw) > updater.MaxManifest {
		return
	}
	key := m.Key
	if key == nil {
		key = updater.ReleaseKey()
	}
	man, err := updater.Open(raw, key)
	if err != nil {
		m.logf("ignoring a release notice that doesn't verify", "err", err)
		return
	}
	m.mu.Lock()
	changed := string(m.body) != string(raw)
	m.body = raw
	m.mu.Unlock()
	if changed && m.Log != nil {
		m.Log.Info("release notice", "version", man.Version)
	}
}

func (m *UpdateMirror) logf(msg string, args ...any) {
	if m.Log != nil {
		m.Log.Warn(msg, args...)
	}
}

// ServeHTTP answers GET /v1/update.
func (m *UpdateMirror) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	body := m.body
	m.mu.Unlock()
	if body == nil {
		writeError(w, http.StatusNotFound, "no release published yet")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(body)
}

// SetUpdates serves the mirror at /v1/update.
func (s *Server) SetUpdates(m *UpdateMirror) { s.updates = m }
