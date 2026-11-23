package staging

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

var (
	ErrBadTitle   = errors.New("staging: bad folder title")
	ErrBadPattern = errors.New("staging: bad promote pattern")
	ErrBadRequest = errors.New("staging: bad request")
)

// MaxNameBytes caps a folder name, and each element of a promote target,
// well below every filesystem's 255.
const MaxNameBytes = 200

// DefaultPattern promotes into the archive root, matching its flat layout.
const DefaultPattern = "{name}"

// badNameChars can't appear in a folder name: "/" separates paths, and the
// rest are what Windows refuses, so the owner's SMB clients can open it.
const badNameChars = `/\<>:"|?*`

// checkName says why s can't be a folder name, or returns nil.
func checkName(s string) error {
	switch {
	case s == "":
		return errors.New("is empty")
	case s == "." || s == "..":
		return errors.New("is not a folder name")
	case strings.HasPrefix(s, "."):
		return errors.New("starts with a dot (it would be hidden)")
	case strings.HasSuffix(s, ".") || strings.HasSuffix(s, " "):
		return errors.New("ends with a dot or a space, which Windows can't use")
	case len(s) > MaxNameBytes:
		return fmt.Errorf("is longer than %d bytes", MaxNameBytes)
	case !utf8.ValidString(s):
		return errors.New("is not valid UTF-8")
	}
	for _, r := range s {
		if unicode.IsControl(r) || strings.ContainsRune(badNameChars, r) {
			// %q of the one-rune string prints "/" (with double quotes), as
			// naming.js's JSON.stringify does, so the UI and the API agree.
			return fmt.Errorf("contains %q, which can't be used in a folder name", string(r))
		}
	}
	return nil
}

// CleanTitle trims a folder title and checks it can be part of a folder
// name. An empty title is fine: the folder is then named by its date.
func CleanTitle(title string) (string, error) {
	t := strings.TrimSpace(title)
	if t == "" {
		return "", nil
	}
	if err := checkName(t); err != nil {
		return "", fmt.Errorf("%w: the title %v", ErrBadTitle, err)
	}
	return t, nil
}

// FolderName joins a date and a title the archive's way (§8):
// "2007-11-10 - SMU Homecoming", or just the date, or just the title.
func FolderName(date, title string) string {
	switch {
	case date == "":
		return title
	case title == "":
		return date
	}
	return date + " - " + title
}

// folderDate is the date a folder is filed under: its name's leading date,
// else the earliest date it covers (Dates is sorted), else "".
func folderDate(f catalog.StagingFolder) string {
	if d, _ := SplitName(f.Name); d != "" {
		return d
	}
	if len(f.Dates) > 0 {
		return f.Dates[0]
	}
	return ""
}

var tokenRE = regexp.MustCompile(`\{[^{}]*\}`)

// ExpandPattern returns the folder below Archive/ that f is promoted to, as
// a slash path. Tokens: {name}, {year}, {month} (from folderDate). Every
// element of the result must be a usable folder name.
func ExpandPattern(pattern string, f catalog.StagingFolder) (string, error) {
	p := strings.TrimSpace(pattern)
	if p == "" {
		p = DefaultPattern
	}
	date := folderDate(f)
	var bad error
	out := tokenRE.ReplaceAllStringFunc(p, func(tok string) string {
		switch tok {
		case "{name}":
			return f.Name
		case "{year}", "{month}":
			if date == "" {
				bad = fmt.Errorf("%s needs a date, and %q has none", tok, f.Name)
				return ""
			}
			if tok == "{year}" {
				return date[:4]
			}
			return date[5:7]
		}
		bad = fmt.Errorf("unknown token %s (use {name}, {year} or {month})", tok)
		return ""
	})
	if bad != nil {
		return "", fmt.Errorf("%w: %v", ErrBadPattern, bad)
	}
	if strings.ContainsAny(out, "{}") {
		return "", fmt.Errorf("%w: unbalanced braces in %q", ErrBadPattern, pattern)
	}
	for _, el := range strings.Split(out, "/") {
		if err := checkName(el); err != nil {
			return "", fmt.Errorf("%w: in %q a folder name %v", ErrBadPattern, out, err)
		}
	}
	return out, nil
}
