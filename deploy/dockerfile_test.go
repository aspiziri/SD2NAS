package deploy

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var pinnedFrom = regexp.MustCompile(`^FROM [a-z0-9./-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}( AS [a-z]+)?$`)

// TestDockerfilePinned keeps every base image pinned by tag and digest, so a
// release builds from the same bytes it was tested on.
func TestDockerfilePinned(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	var froms []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "FROM ") {
			froms = append(froms, line)
		}
	}
	if len(froms) != 3 {
		t.Fatalf("Dockerfile has %d FROM lines, want 3: %q", len(froms), froms)
	}
	for _, line := range froms {
		if !pinnedFrom.MatchString(line) {
			t.Errorf("FROM line is not pinned by tag and digest: %q", line)
		}
	}
	if last := froms[len(froms)-1]; !strings.HasPrefix(last, "FROM alpine:3.22") {
		t.Errorf("runtime stage = %q, want Alpine 3.22", last)
	}
}
