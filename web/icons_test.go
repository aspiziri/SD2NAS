package web

import (
	"encoding/json"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"path"
	"strings"
	"testing"
)

// TestIcons reads web/public straight from disk, so it runs without npm.
func TestIcons(t *testing.T) {
	raw, err := os.ReadFile("public/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name     string `json:"name"`
		Display  string `json:"display"`
		StartURL string `json:"start_url"`
		Icons    []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if m.Name != "SD2NAS" {
		t.Errorf("manifest name = %q, want SD2NAS", m.Name)
	}
	if m.Display != "standalone" {
		t.Errorf("manifest display = %q, want standalone", m.Display)
	}
	if m.StartURL != "/" {
		t.Errorf("manifest start_url = %q, want /", m.StartURL)
	}

	maskable := false
	for _, icon := range m.Icons {
		if strings.Contains(icon.Purpose, "maskable") {
			maskable = true
		}
		if !strings.HasPrefix(icon.Src, "/") {
			t.Errorf("icon src %q is not root-relative", icon.Src)
			continue
		}
		file := path.Clean(strings.TrimPrefix(icon.Src, "/"))
		if icon.Type != "image/png" {
			continue
		}
		var want int
		if _, err := fmt.Sscanf(icon.Sizes, "%dx%d", &want, &want); err != nil {
			t.Errorf("icon %s has unparsable sizes %q", icon.Src, icon.Sizes)
			continue
		}
		f, err := os.Open("public/" + file)
		if err != nil {
			t.Errorf("manifest icon %s: %v", icon.Src, err)
			continue
		}
		cfg, _, err := image.DecodeConfig(f)
		f.Close()
		if err != nil {
			t.Errorf("manifest icon %s is not a PNG: %v", icon.Src, err)
			continue
		}
		if cfg.Width != want || cfg.Height != want {
			t.Errorf("manifest icon %s is %dx%d, sizes says %d", icon.Src, cfg.Width, cfg.Height, want)
		}
	}
	if !maskable {
		t.Error("manifest has no maskable icon")
	}

	apple := decodeAt(t, "public/apple-touch-icon.png")
	if b := apple.Bounds(); b.Dx() != 180 || b.Dy() != 180 {
		t.Errorf("apple-touch-icon.png is %v, want 180x180", b)
	}
	if _, _, _, a := apple.At(0, 0).RGBA(); a != 0xffff {
		t.Errorf("apple-touch-icon.png corner alpha = %d, want opaque (iOS turns transparency black)", a>>8)
	}

	if _, _, _, a := decodeAt(t, "public/icon-192.png").At(0, 0).RGBA(); a != 0 {
		t.Errorf("icon-192.png corner alpha = %d, want 0 (rounded corner)", a>>8)
	}

	svg, err := os.ReadFile("public/icon.svg")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(svg), "<svg") {
		t.Errorf("icon.svg does not start with <svg: %.40q", svg)
	}
}

func decodeAt(t *testing.T, name string) image.Image {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return img
}
