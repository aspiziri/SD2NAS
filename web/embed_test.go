package web

import (
	"errors"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestDistHasPlaceholder(t *testing.T) {
	if _, err := fs.Stat(Dist(), ".placeholder"); err != nil {
		t.Fatal(err)
	}
}

// TestBuiltUI checks a real `npm run build` when one is present. CI's web
// job builds first and sets SD2NAS_REQUIRE_UI=1, so it can't be skipped there.
func TestBuiltUI(t *testing.T) {
	d := Dist()
	index, err := fs.ReadFile(d, "index.html")
	if errors.Is(err, fs.ErrNotExist) {
		if os.Getenv("SD2NAS_REQUIRE_UI") == "1" {
			t.Fatal("web/dist/index.html is missing: run npm run build in web/")
		}
		t.Skip("the UI is not built (npm run build in web/)")
	}
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`(?:src|href)="/(assets/[^"]+)"`).FindAllStringSubmatch(string(index), -1)
	if len(refs) == 0 {
		t.Fatalf("index.html references no /assets/ files:\n%s", index)
	}
	hashed := regexp.MustCompile(`-[A-Za-z0-9_-]{6,}\.(js|css)$`)
	for _, m := range refs {
		if _, err := fs.Stat(d, m[1]); err != nil {
			t.Errorf("index.html references %s, which is not in dist: %v", m[1], err)
		}
		if !hashed.MatchString(m[1]) {
			t.Errorf("%s has no content hash, so it can't be cached as immutable", m[1])
		}
	}
	if !strings.Contains(string(index), `<div id="app"></div>`) {
		t.Error(`index.html has no <div id="app"></div>`)
	}
	if _, err := fs.Stat(d, ".placeholder"); err != nil {
		t.Errorf("the build removed dist/.placeholder; go:embed fails on a fresh checkout without it: %v", err)
	}
}
