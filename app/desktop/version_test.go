package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestVersionMatchesWailsJSON(t *testing.T) {
	raw, err := os.ReadFile("wails.json")
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Info struct {
			ProductVersion string `json:"productVersion"`
		} `json:"info"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	if w.Info.ProductVersion != appVersion {
		t.Fatalf("version.go says %s, wails.json says %s", appVersion, w.Info.ProductVersion)
	}
}
