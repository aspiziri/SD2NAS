package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/media"
)

// t0 is declared in assets_test.go.
var (
	t1 = t0.Add(time.Minute)
	t2 = t0.Add(time.Hour)
)

func newIngest(t *testing.T, d *DB) Ingest {
	t.Helper()
	in, err := d.CreateIngest(context.Background(), "/cards/SONY_A7IV", "SONY_A7IV", "1234-ABCD", t0)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestIngestLifecycle(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	in := newIngest(t, d)
	if in.ID <= 0 || in.Status != StatusQueued || !in.StartedAt.Equal(t0) || !in.FinishedAt.IsZero() {
		t.Fatalf("created = %+v", in)
	}
	if err := d.StartIngest(ctx, in.ID, t1); err != nil {
		t.Fatal(err)
	}
	got, err := d.Ingest(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning || !got.StartedAt.Equal(t1) {
		t.Fatalf("started = %+v", got)
	}
	counts := Counts{Seen: 3, New: 1, InArchive: 1, SeenBefore: 1}
	err = d.FinishIngest(ctx, Ingest{ID: in.ID, Status: StatusDone, FinishedAt: t2, Counts: counts,
		ClockWarnings: 2, CardSize: 1234, SafeToFormat: true, UnsupportedExts: []string{".360", ".mkv", ""}})
	if err != nil {
		t.Fatal(err)
	}
	got, err = d.Ingest(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != in.ID || got.CardPath != "/cards/SONY_A7IV" || got.CardLabel != "SONY_A7IV" || got.CardUUID != "1234-ABCD" ||
		got.CardSize != 1234 || !got.StartedAt.Equal(t1) || !got.FinishedAt.Equal(t2) || got.Status != StatusDone ||
		got.Counts != counts || got.ClockWarnings != 2 || !got.SafeToFormat || got.Error != "" ||
		!reflect.DeepEqual(got.UnsupportedExts, []string{".360", ".mkv", ""}) {
		t.Fatalf("finished = %+v", got)
	}
}

func TestIngestNoUnsupportedExtsReadsNil(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	in := newIngest(t, d)
	if in.UnsupportedExts != nil {
		t.Errorf("created UnsupportedExts = %q, want nil", in.UnsupportedExts)
	}
	if err := d.FinishIngest(ctx, Ingest{ID: in.ID, Status: StatusDone, FinishedAt: t2, UnsupportedExts: []string{}}); err != nil {
		t.Fatal(err)
	}
	got, err := d.Ingest(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UnsupportedExts != nil {
		t.Errorf("finished UnsupportedExts = %q, want nil", got.UnsupportedExts)
	}
}

func TestStartIngestTwice(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	in := newIngest(t, d)
	if err := d.StartIngest(ctx, in.ID, t1); err != nil {
		t.Fatal(err)
	}
	if err := d.StartIngest(ctx, in.ID, t2); err == nil {
		t.Error("second StartIngest succeeded")
	}
	if err := d.StartIngest(ctx, 999, t1); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v, want ErrNotFound", err)
	}
}

func TestFinishIngestRejects(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	in := newIngest(t, d)
	if err := d.FinishIngest(ctx, Ingest{ID: in.ID, Status: StatusRunning, FinishedAt: t2}); err == nil {
		t.Error("non-terminal status accepted")
	}
	if err := d.FinishIngest(ctx, Ingest{ID: in.ID, Status: StatusDone}); err == nil {
		t.Error("zero FinishedAt accepted")
	}
	if err := d.FinishIngest(ctx, Ingest{ID: in.ID, Status: StatusDone, FinishedAt: t2, CardSize: 7}); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishIngest(ctx, Ingest{ID: in.ID, Status: StatusFailed, FinishedAt: t2, CardSize: 9, Error: "x"}); err == nil {
		t.Error("finishing a finished row succeeded")
	}
	got, err := d.Ingest(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || got.CardSize != 7 || got.Error != "" {
		t.Errorf("finished row changed: %+v", got)
	}
	if err := d.FinishIngest(ctx, Ingest{ID: 999, Status: StatusDone, FinishedAt: t2}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v, want ErrNotFound", err)
	}
	if _, err := d.Ingest(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Ingest unknown: %v", err)
	}
}

func TestIngestsNewestFirst(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	for range 3 {
		newIngest(t, d)
	}
	check := func(limit int, before int64, want ...int64) {
		t.Helper()
		got, err := d.Ingests(ctx, limit, before)
		if err != nil {
			t.Fatal(err)
		}
		var g []int64
		for _, in := range got {
			g = append(g, in.ID)
		}
		if len(g) != len(want) {
			t.Fatalf("Ingests(%d,%d) = %v, want %v", limit, before, g, want)
		}
		for i := range g {
			if g[i] != want[i] {
				t.Fatalf("Ingests(%d,%d) = %v, want %v", limit, before, g, want)
			}
		}
	}
	check(2, 0, 3, 2)
	check(50, 2, 1)
	check(0, 0, 3, 2, 1)
}

func TestFailInterrupted(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	queued := newIngest(t, d)
	running := newIngest(t, d)
	done := newIngest(t, d)
	if err := d.StartIngest(ctx, running.ID, t1); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishIngest(ctx, Ingest{ID: done.ID, Status: StatusDone, FinishedAt: t1}); err != nil {
		t.Fatal(err)
	}
	n, err := d.FailInterrupted(ctx, t2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("FailInterrupted = %d, want 2", n)
	}
	for _, id := range []int64{queued.ID, running.ID} {
		got, err := d.Ingest(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusFailed || got.Error != "interrupted: SD2NAS stopped before this import finished" || !got.FinishedAt.Equal(t2) {
			t.Errorf("ingest %d = %+v", id, got)
		}
	}
	got, err := d.Ingest(ctx, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || !got.FinishedAt.Equal(t1) || got.Error != "" {
		t.Errorf("done row changed: %+v", got)
	}
}

func TestCountsAdd(t *testing.T) {
	var c Counts
	for _, o := range []Outcome{OutcomeNew, OutcomeInArchive, OutcomeInStaging, OutcomeSeenBefore, OutcomeFailed, OutcomeUnsupported} {
		c.Add(o)
	}
	want := Counts{Seen: 5, New: 1, InArchive: 1, InStaging: 1, SeenBefore: 1, Failed: 1, Unsupported: 1}
	if c != want {
		t.Errorf("counts = %+v, want %+v", c, want)
	}
}

func TestStatusTerminal(t *testing.T) {
	for s, want := range map[IngestStatus]bool{StatusQueued: false, StatusRunning: false, StatusDone: true,
		StatusFailed: true, StatusRefused: true, StatusCancelled: true} {
		if s.Terminal() != want {
			t.Errorf("%s.Terminal() = %v", s, !want)
		}
	}
}

func TestIngestFilesRoundTrip(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	in := newIngest(t, d)
	const rel = "Archive/2025-05-30 - Trip/x.JPG"
	if err := d.InsertAsset(ctx, Asset{SHA256: hexN(1), Size: 5, Kind: media.KindImage, State: StateArchived, RelPath: rel, UpdatedAt: t0}, ""); err != nil {
		t.Fatal(err)
	}
	files := []IngestFile{
		{CardPath: "DCIM/100MSDCF/A.JPG", Size: 10, Outcome: OutcomeFailed, Error: "boom"},
		{CardPath: "DCIM/100MSDCF/B.JPG", Size: 20, QuickFP: hexN(2), SHA256: hexN(1), Outcome: OutcomeInArchive},
	}
	if err := d.AddIngestFiles(ctx, in.ID, files); err != nil {
		t.Fatal(err)
	}
	got, err := d.IngestFiles(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d files", len(got))
	}
	if got[0] != files[0] {
		t.Errorf("first = %+v, want %+v", got[0], files[0])
	}
	want := files[1]
	want.RelPath = rel
	if got[1] != want {
		t.Errorf("second = %+v, want %+v", got[1], want)
	}

	for name, bad := range map[string]IngestFile{
		"outcome": {CardPath: "C.JPG", Size: 1, Outcome: "meh"},
		"hash":    {CardPath: "D.JPG", Size: 1, SHA256: "xyz", Outcome: OutcomeSeenBefore},
	} {
		if err := d.AddIngestFiles(ctx, in.ID, []IngestFile{{CardPath: "ok.JPG", Size: 1, Outcome: OutcomeFailed}, bad}); err == nil {
			t.Errorf("bad %s accepted", name)
		}
	}
	got, err = d.IngestFiles(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("failed batches wrote rows: %d files", len(got))
	}
}

func TestRecordNew(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	in := newIngest(t, d)
	const rel = "Staging/2025-05-30/2025-05-31 01.30.12.JPG"
	a := Asset{SHA256: hexN(1), Size: 598, Kind: media.KindImage, State: StateStaged, RelPath: rel, UpdatedAt: t0}
	f := IngestFile{CardPath: "DCIM/100MSDCF/DSC00001.JPG", Size: 598, Outcome: OutcomeNew}
	if err := d.RecordNew(ctx, in.ID, a, hexN(2), f); err != nil {
		t.Fatal(err)
	}
	got, err := d.AssetBySHA(ctx, hexN(1))
	if err != nil {
		t.Fatal(err)
	}
	if got.FirstIngest != in.ID {
		t.Errorf("FirstIngest = %d, want %d", got.FirstIngest, in.ID)
	}
	byFP, err := d.AssetsByFingerprint(ctx, hexN(2))
	if err != nil || len(byFP) != 1 || byFP[0].SHA256 != hexN(1) {
		t.Errorf("by fingerprint = %v, %v", byFP, err)
	}
	files, err := d.IngestFiles(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := IngestFile{CardPath: f.CardPath, Size: 598, QuickFP: hexN(2), SHA256: hexN(1), Outcome: OutcomeNew, RelPath: rel}
	if len(files) != 1 || files[0] != want {
		t.Errorf("files = %+v, want [%+v]", files, want)
	}
}

func TestRecordNewIsAtomic(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	in := newIngest(t, d)
	a := Asset{SHA256: hexN(1), Size: 5, Kind: media.KindImage, State: StateStaged, RelPath: "Staging/x.JPG", UpdatedAt: t0}
	if err := d.InsertAsset(ctx, a, ""); err != nil {
		t.Fatal(err)
	}
	f := IngestFile{CardPath: "A.JPG", Size: 5, Outcome: OutcomeNew}
	if err := d.RecordNew(ctx, in.ID, a, hexN(5), f); err == nil {
		t.Fatal("duplicate sha accepted")
	}
	files, err := d.IngestFiles(ctx, in.ID)
	if err != nil || len(files) != 0 {
		t.Errorf("files = %v, %v", files, err)
	}
	byFP, err := d.AssetsByFingerprint(ctx, hexN(5))
	if err != nil || byFP != nil {
		t.Errorf("fingerprint rows = %v, %v", byFP, err)
	}
	b := a
	b.SHA256 = hexN(3)
	f.Outcome = OutcomeFailed
	if err := d.RecordNew(ctx, in.ID, b, hexN(6), f); err == nil {
		t.Error("OutcomeFailed accepted")
	}
	if _, err := d.AssetBySHA(ctx, hexN(3)); !errors.Is(err, ErrNotFound) {
		t.Errorf("asset written despite bad outcome: %v", err)
	}

	// Fails after insertAsset succeeded: the malformed fingerprint is rejected
	// in step 2, so the asset written in step 1 must be rolled back.
	c := a
	c.SHA256 = hexN(4)
	f.Outcome = OutcomeNew
	if err := d.RecordNew(ctx, in.ID, c, "xyz", f); !errors.Is(err, ErrBadHash) {
		t.Errorf("bad fingerprint: %v, want ErrBadHash", err)
	}
	if _, err := d.AssetBySHA(ctx, hexN(4)); !errors.Is(err, ErrNotFound) {
		t.Errorf("asset not rolled back after fingerprint failure: %v", err)
	}
	files, err = d.IngestFiles(ctx, in.ID)
	if err != nil || len(files) != 0 {
		t.Errorf("files after rollback = %v, %v", files, err)
	}
}

func TestClockWarningsRoundTrip(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	in := newIngest(t, d)
	if err := d.InsertAsset(ctx, Asset{SHA256: hexN(1), Size: 5, Kind: media.KindImage, State: StateStaged, UpdatedAt: t0}, ""); err != nil {
		t.Fatal(err)
	}
	ws := []ClockWarning{
		{CardPath: "DCIM/A.JPG", SHA256: hexN(1), Kind: ClockFuture, Detail: "in the future"},
		{CardPath: "DCIM/B.JPG", Kind: ClockResetDefault, Detail: "2000-01-01"},
	}
	if err := d.AddClockWarnings(ctx, in.ID, ws); err != nil {
		t.Fatal(err)
	}
	got, err := d.ClockWarnings(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != ws[0] || got[1] != ws[1] {
		t.Errorf("got %+v, want %+v", got, ws)
	}
	if err := d.AddClockWarnings(ctx, in.ID, []ClockWarning{{CardPath: "C.JPG", Kind: "late", Detail: "x"}}); err == nil {
		t.Error("bad kind accepted")
	}
}

func TestIngestJSON(t *testing.T) {
	b, err := json.Marshal(Ingest{ID: 1, Status: StatusQueued, StartedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"counts":{"seen":0,`, `"safe_to_format":false`} {
		if !strings.Contains(s, want) {
			t.Errorf("%s lacks %s", s, want)
		}
	}
	if strings.Contains(s, `"finished_at"`) {
		t.Errorf("%s has finished_at", s)
	}
}

func TestMigrateV1ToV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sd2nas.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{schemaV1, "PRAGMA user_version = 1",
		`INSERT INTO ingests (card_path, started_at, status) VALUES ('/cards/X', '2026-01-01T12:00:00.000Z', 'done')`,
		`INSERT INTO staging_folders (id, name, dates, created_at) VALUES (7, '2025-05-30', '["2025-05-30"]', '2026-01-01T12:00:00.000Z')`} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := t.Context()
	if v, err := d.Version(ctx); err != nil || v != SchemaVersion {
		t.Fatalf("version = %d, %v; want %d", v, err, SchemaVersion)
	}
	f, err := d.StagingFolderForDate(ctx, "2025-05-30", t0)
	if err != nil || f.ID != 7 {
		t.Fatalf("migrated folder = %+v, %v; want id 7 kept", f, err)
	}
	g, err := d.StagingFolderForDate(ctx, "2025-06-01", t0)
	if err != nil || g.ID <= 7 {
		t.Fatalf("new folder = %+v, %v; want an id above 7", g, err)
	}
	in, err := d.Ingest(ctx, 1)
	if err != nil || !in.ForgottenAt.IsZero() {
		t.Fatalf("ingest = %+v, %v", in, err)
	}
	for _, want := range [][2]string{{"assets", "assets_first_ingest"}, {"fingerprints", "fingerprints_sha256"}, {"aliases", "aliases_sha256"}} {
		var n int
		err := d.db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_index_list(?) WHERE name = ?", want[0], want[1]).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("index %s on %s: count %d, %v; want 1", want[1], want[0], n, err)
		}
	}
}

func TestMigrateV2ToV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sd2nas.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{schemaV1, schemaV2, "PRAGMA user_version = 2",
		`INSERT INTO ingests (id, card_path, started_at, status) VALUES (1, '/cards/X', '2026-01-01T12:00:00.000Z', 'done')`,
		`INSERT INTO ingest_files (ingest_id, card_path, size, outcome) VALUES (1, 'DCIM/c.JPG', 3, 'new')`,
		`INSERT INTO ingest_files (ingest_id, card_path, size, outcome) VALUES (1, 'DCIM/a.JPG', 1, 'in_archive')`,
		`INSERT INTO ingest_files (ingest_id, card_path, size, outcome, error) VALUES (1, 'DCIM/b.JPG', 2, 'seen_before', 'x')`,
		`INSERT INTO assets (sha256, size, kind, state, rel_path, first_ingest, updated_at)
			VALUES (X'` + hexN(1) + `', 1, 'image', 'archived', 'Archive/a.JPG', 1, '2026-02-02T10:00:00.000Z')`,
		`INSERT INTO assets (sha256, size, kind, state, rel_path, updated_at)
			VALUES (X'` + hexN(2) + `', 1, 'image', 'archived', 'Archive/b.JPG', '2026-02-03T10:00:00.000Z')`} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := t.Context()
	if v, err := d.Version(ctx); err != nil || v != SchemaVersion {
		t.Fatalf("version = %d, %v; want %d", v, err, SchemaVersion)
	}
	files, err := d.IngestFiles(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := []IngestFile{
		{CardPath: "DCIM/c.JPG", Size: 3, Outcome: OutcomeNew},
		{CardPath: "DCIM/a.JPG", Size: 1, Outcome: OutcomeInArchive},
		{CardPath: "DCIM/b.JPG", Size: 2, Outcome: OutcomeSeenBefore, Error: "x"},
	}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %+v, want %+v", files, want)
	}
	err = d.AddIngestFiles(ctx, 1, []IngestFile{{CardPath: "DCIM/m.JPG", Size: 4, Outcome: OutcomeMissing}})
	if err != nil {
		t.Fatalf("add missing file: %v", err)
	}
	if in := mustIngest(t, d, 1); in.Counts.Missing != 0 {
		t.Errorf("migrated ingest missing = %d, want 0", in.Counts.Missing)
	}
	for _, name := range []string{"ingest_files_ingest", "ingest_files_sha256"} {
		var n int
		err := d.db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_index_list('ingest_files') WHERE name = ?", name).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("index %s: count %d, %v; want 1", name, n, err)
		}
	}
	for n, wantPromoted := range map[byte]sql.NullString{
		1: {String: "2026-02-02T10:00:00.000Z", Valid: true},
		2: {},
	} {
		var promoted, checked sql.NullString
		err := d.db.QueryRowContext(ctx, "SELECT promoted_at, checked_at FROM assets WHERE sha256 = ?", mustBlob(t, hexN(n))).Scan(&promoted, &checked)
		if err != nil {
			t.Fatal(err)
		}
		if promoted != wantPromoted || checked.Valid {
			t.Errorf("asset %d: promoted_at %v checked_at %v; want promoted_at %v, checked_at NULL", n, promoted, checked, wantPromoted)
		}
	}
}

func TestMigrateV3ToV4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sd2nas.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{schemaV1, schemaV2, schemaV3, "PRAGMA user_version = 3",
		`INSERT INTO settings (key, value) VALUES ('staging_warn_days', '7')`} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := t.Context()
	if v, err := d.Version(ctx); err != nil || v != 4 {
		t.Fatalf("version = %d, %v; want 4", v, err)
	}
	var val string
	if err := d.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = 'staging_warn_days'").Scan(&val); err != nil || val != "7" {
		t.Fatalf("setting = %q, %v; want 7", val, err)
	}
	var n int
	err = d.db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_index_list('notifications') WHERE name = 'notifications_pending'").Scan(&n)
	if err != nil || n != 1 {
		t.Errorf("index notifications_pending: count %d, %v; want 1", n, err)
	}
}

func mustBlob(t *testing.T, hexs string) []byte {
	t.Helper()
	b, err := hexToBlob(hexs)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCountsAddMissing(t *testing.T) {
	var c Counts
	c.Add(OutcomeMissing)
	if want := (Counts{Seen: 1, Missing: 1}); c != want {
		t.Errorf("counts = %+v, want %+v", c, want)
	}
}

func TestFinishIngestStoresMissing(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	in := newIngest(t, d)
	if err := d.StartIngest(ctx, in.ID, t1); err != nil {
		t.Fatal(err)
	}
	counts := Counts{Seen: 2, Missing: 2}
	if err := d.FinishIngest(ctx, Ingest{ID: in.ID, Status: StatusDone, FinishedAt: t2, Counts: counts}); err != nil {
		t.Fatal(err)
	}
	if got := mustIngest(t, d, in.ID); got.Counts != counts {
		t.Errorf("counts = %+v, want %+v", got.Counts, counts)
	}
}

func TestMarkIngestForgotten(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	in, err := d.CreateIngest(ctx, "/cards/A", "", "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkIngestForgotten(ctx, in.ID, t0); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("queued ingest: err = %v, want a non-NotFound error", err)
	}
	if err := d.StartIngest(ctx, in.ID, t0); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishIngest(ctx, Ingest{ID: in.ID, Status: StatusDone, FinishedAt: t0}); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(mustIngest(t, d, in.ID))
	if err != nil || strings.Contains(string(b), "forgotten_at") {
		t.Errorf("never forgotten JSON = %s, %v", b, err)
	}
	if err := d.MarkIngestForgotten(ctx, in.ID, t0); err != nil {
		t.Fatal(err)
	}
	got := mustIngest(t, d, in.ID)
	if !got.ForgottenAt.Equal(t0) {
		t.Errorf("ForgottenAt = %v, want %v", got.ForgottenAt, t0)
	}
	b, err = json.Marshal(got)
	if err != nil || !strings.Contains(string(b), `"forgotten_at"`) {
		t.Errorf("JSON = %s, %v", b, err)
	}
	if err := d.MarkIngestForgotten(ctx, in.ID, t0); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("second call: err = %v, want a non-NotFound error", err)
	}
	if err := d.MarkIngestForgotten(ctx, 999, t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("id 999: err = %v, want ErrNotFound", err)
	}
}

func mustIngest(t *testing.T, d *DB, id int64) Ingest {
	t.Helper()
	in, err := d.Ingest(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return in
}
