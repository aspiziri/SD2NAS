package web

import (
	"io/fs"
	"testing"
)

func TestDistHasPlaceholder(t *testing.T) {
	if _, err := fs.Stat(Dist(), ".placeholder"); err != nil {
		t.Fatal(err)
	}
}
