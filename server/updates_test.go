package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"altnet-relay/updater"
)

func TestUpdateNoticeIsMirroredOnlyWhenSigned(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	var notice []byte
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if notice == nil {
			w.WriteHeader(404)
			return
		}
		w.Write(notice)
	}))
	defer gh.Close()

	r := newRig(t)
	m := &UpdateMirror{Source: gh.URL, Key: pub}
	r.srv.SetUpdates(m)

	m.refresh(context.Background())
	if c, _ := r.do("GET", "/v1/update", "", nil); c != 404 {
		t.Fatalf("nothing released yet: %d", c)
	}

	env, _ := updater.Sign(&updater.Manifest{Version: "1.0.6", Files: map[string]updater.File{}}, other)
	notice, _ = json.Marshal(env)
	m.refresh(context.Background())
	if c, _ := r.do("GET", "/v1/update", "", nil); c != 404 {
		t.Fatalf("a notice signed with another key must not be passed on: %d", c)
	}

	env, _ = updater.Sign(&updater.Manifest{Version: "1.0.6", Files: map[string]updater.File{}}, priv)
	notice, _ = json.Marshal(env)
	m.refresh(context.Background())
	c, out := r.do("GET", "/v1/update", "", nil) // no sign-in needed
	if c != 200 || out["sig"] == nil {
		t.Fatalf("the signed notice should be served to anyone: %d %v", c, out)
	}
	if got, err := updater.Open(r.lastBody, pub); err != nil || got.Version != "1.0.6" {
		t.Fatalf("served notice doesn't verify: %v", err)
	}
}
