package staging_test

import (
	"testing"

	"github.com/aspiziri/SD2NAS/internal/ingest"
	"github.com/aspiziri/SD2NAS/internal/staging"
)

func TestDirMatchesIngest(t *testing.T) {
	if staging.Dir != ingest.StagingDir {
		t.Errorf("staging.Dir = %q, ingest.StagingDir = %q", staging.Dir, ingest.StagingDir)
	}
}
