package staging

import (
	"errors"
	"strings"
	"testing"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

func TestCleanTitle(t *testing.T) {
	for in, want := range map[string]string{"  Trip ": "Trip", "": "", "   ": "", "Zürich & Co": "Zürich & Co", strings.Repeat("é", 100): strings.Repeat("é", 100)} {
		got, err := CleanTitle(in)
		if err != nil || got != want {
			t.Errorf("CleanTitle(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"Zürich & Co.", `a/b`, `a\b`, `a:b`, `a*b`, `a?b`, `a"b`, `a<b`, `a>b`, `a|b`, "a\tb", ".hidden", "trailing.", strings.Repeat("é", 101)}
	for _, in := range bad {
		if _, err := CleanTitle(in); !errors.Is(err, ErrBadTitle) {
			t.Errorf("CleanTitle(%q) err = %v, want ErrBadTitle", in, err)
		}
	}
}

func TestFolderName(t *testing.T) {
	for _, c := range [][3]string{{"2025-05-30", "Trip", "2025-05-30 - Trip"}, {"2025-05-30", "", "2025-05-30"}, {"", "Trip", "Trip"}} {
		if got := FolderName(c[0], c[1]); got != c[2] {
			t.Errorf("FolderName(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestExpandPattern(t *testing.T) {
	f := catalog.StagingFolder{Name: "2025-05-30 - Trip", Dates: []string{"2025-05-30", "2025-05-31"}}
	for pat, want := range map[string]string{"": f.Name, "{name}": f.Name, "{year}/{month}/{name}": "2025/05/2025-05-30 - Trip"} {
		got, err := ExpandPattern(pat, f)
		if err != nil || got != want {
			t.Errorf("ExpandPattern(%q) = %q, %v; want %q", pat, got, err, want)
		}
	}
	for _, pat := range []string{"{year}/{name", "{day}", "../{name}", "{year}//{name}"} {
		if _, err := ExpandPattern(pat, f); !errors.Is(err, ErrBadPattern) {
			t.Errorf("ExpandPattern(%q) err = %v, want ErrBadPattern", pat, err)
		}
	}
	if _, err := ExpandPattern("{day}", f); err == nil || !strings.Contains(err.Error(), "{day}") {
		t.Errorf("error should name {day}: %v", err)
	}
	h := catalog.StagingFolder{Name: "Holiday", Dates: []string{"2025-06-01"}}
	if got, err := ExpandPattern("{year}", h); err != nil || got != "2025" {
		t.Errorf("{year} for Holiday = %q, %v", got, err)
	}
	if _, err := ExpandPattern("{year}", catalog.StagingFolder{Name: "Holiday"}); !errors.Is(err, ErrBadPattern) || !strings.Contains(err.Error(), "needs a date") {
		t.Errorf("no-date err = %v", err)
	}
	if _, err := ExpandPattern("{year}", catalog.StagingFolder{Name: "Holiday", Dates: []string{"25"}}); !errors.Is(err, ErrBadPattern) {
		t.Errorf("malformed date err = %v", err)
	}
	if _, err := ExpandPattern("{name}", catalog.StagingFolder{Name: "What?"}); !errors.Is(err, ErrBadPattern) {
		t.Errorf("What? err = %v", err)
	}
}
