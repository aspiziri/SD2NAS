package ingest

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

// Levels of the card script's notification.
const (
	LevelNormal  = "normal"
	LevelWarning = "warning"
	LevelAlert   = "alert"
)

func formatGB(b uint64) string {
	return fmt.Sprintf("%.1f GB", float64(b)/1e9)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// oneLine replaces control characters with spaces and trims the result.
func oneLine(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}

// CardName is the card's display name: the label (with control characters
// replaced by spaces and trimmed), else the last element of CardPath, else "card".
func CardName(in catalog.Ingest) string {
	if name := oneLine(in.CardLabel); name != "" {
		return name
	}
	if in.CardPath != "" {
		if name := oneLine(filepath.Base(in.CardPath)); name != "" && name != "." && name != string(filepath.Separator) {
			return name
		}
	}
	return "card"
}

// Describe returns the two lines the card script turns into an Unraid
// notification: a level and a one-line message (no control characters).
func Describe(in catalog.Ingest) (level, message string) {
	level, body := describeBody(in)
	return level, oneLine(CardName(in) + ": " + body)
}

func describeBody(in catalog.Ingest) (level, body string) {
	c := in.Counts
	switch in.Status {
	case catalog.StatusQueued:
		return LevelNormal, "waiting for another import to finish."
	case catalog.StatusRunning:
		return LevelNormal, "importing."
	case catalog.StatusRefused:
		return LevelAlert, "not imported, " + in.Error + ". Card left mounted."
	case catalog.StatusFailed:
		body = "import failed: " + in.Error + "."
		if c.New > 0 {
			body += fmt.Sprintf(" %d new copied before it stopped.", c.New)
		}
		return LevelAlert, body + " Card left mounted."
	case catalog.StatusCancelled:
		body = "import cancelled because SD2NAS stopped."
		if c.New > 0 {
			body += fmt.Sprintf(" %d new copied.", c.New)
		}
		return LevelAlert, body + " Card left mounted."
	case catalog.StatusDone:
		return describeDone(in)
	}
	return LevelAlert, fmt.Sprintf("unexpected import status %q.", in.Status)
}

func describeDone(in catalog.Ingest) (level, body string) {
	c := in.Counts
	if c.Seen == 0 && c.Unsupported == 0 {
		return LevelWarning, "no photos or videos found. Do not format the card."
	}
	known := c.InArchive + c.InStaging + c.SeenBefore
	body = fmt.Sprintf("%d new, %d already known", c.New, known)
	if c.Failed > 0 {
		body += fmt.Sprintf(", %d failed", c.Failed)
	}
	if c.Unsupported > 0 {
		body += fmt.Sprintf(", %d %s not imported", c.Unsupported, plural(c.Unsupported, "file", "files"))
		body += describeExts(in.UnsupportedExts)
	}
	body += "."
	if !in.SafeToFormat {
		return LevelWarning, body + " Do not format the card."
	}
	body += " Card safe to format."
	if in.ClockWarnings > 0 {
		return LevelWarning, body + fmt.Sprintf(" Check the camera clock: %d %s suspicious capture times.",
			in.ClockWarnings, plural(in.ClockWarnings, "file has", "files have"))
	}
	return LevelNormal, body
}

// maxExts is how many unsupported extensions the message names.
const maxExts = 5

// describeExts names the unsupported extensions, e.g. " (.360, .mkv)", so
// the owner can decide whether formatting would lose anything.
func describeExts(exts []string) string {
	if len(exts) == 0 {
		return ""
	}
	names := make([]string, 0, maxExts)
	for i, e := range exts {
		if i == maxExts {
			break
		}
		if e == "" {
			e = "no extension"
		}
		names = append(names, e)
	}
	s := strings.Join(names, ", ")
	if len(exts) > maxExts {
		s += fmt.Sprintf(" and %d more", len(exts)-maxExts)
	}
	return " (" + s + ")"
}
