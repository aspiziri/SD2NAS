package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSettingsDefaults(t *testing.T) {
	d := openTest(t)
	got, err := d.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{DayStartsAt: 4 * time.Hour, VerifyKnown: VerifyQuick, ReserveBytes: 50_000_000_000, StagingWarnDays: 30, ArchiveRescanAt: 3 * time.Hour, ArchiveSkip: DefaultArchiveSkip}
	if got != want || got != DefaultSettings() {
		t.Errorf("Settings = %+v, want %+v", got, want)
	}
}

func TestSetSettingRoundTrip(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	set := func(key, value string) Settings {
		t.Helper()
		if err := d.SetSetting(ctx, key, value); err != nil {
			t.Fatalf("SetSetting(%s, %q): %v", key, value, err)
		}
		s, err := d.Settings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := set(KeyDayStartsAt, "05:30"); s.DayStartsAt != 5*time.Hour+30*time.Minute {
		t.Errorf("DayStartsAt = %v", s.DayStartsAt)
	}
	if s := set(KeyDayStartsAt, "00:00"); s.DayStartsAt != 0 {
		t.Errorf("DayStartsAt = %v, want 0 (upsert)", s.DayStartsAt)
	}
	if s := set(KeyVerifyKnown, "full"); s.VerifyKnown != VerifyFull {
		t.Errorf("VerifyKnown = %v", s.VerifyKnown)
	}
	if s := set(KeyFreeSpaceReserveGB, "0"); s.ReserveBytes != 0 {
		t.Errorf("ReserveBytes = %d, want 0", s.ReserveBytes)
	}
	if s := set(KeyFreeSpaceReserveGB, "120"); s.ReserveBytes != 120_000_000_000 {
		t.Errorf("ReserveBytes = %d", s.ReserveBytes)
	}
}

func TestSetSettingRejects(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	if err := d.SetSetting(ctx, KeyVerifyKnown, "full"); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ key, value string }{
		{"nope", "1"},
		{KeyDayStartsAt, "24:00"}, {KeyDayStartsAt, "4:00"}, {KeyDayStartsAt, "04:60"}, {KeyDayStartsAt, ""},
		{KeyVerifyKnown, "fast"},
		{KeyFreeSpaceReserveGB, "-1"}, {KeyFreeSpaceReserveGB, "1.5"}, {KeyFreeSpaceReserveGB, "abc"},
		{KeyFreeSpaceReserveGB, "99999999999"},
	}
	for _, c := range cases {
		if err := d.SetSetting(ctx, c.key, c.value); err == nil {
			t.Errorf("SetSetting(%s, %q): expected error", c.key, c.value)
		}
	}
	// Only the one accepted row exists, and it is unchanged.
	var n int
	if err := d.db.QueryRowContext(ctx, `SELECT count(*) FROM settings`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d rows stored, want 1", n)
	}
	s, err := d.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := DefaultSettings(); s.DayStartsAt != want.DayStartsAt || s.ReserveBytes != want.ReserveBytes || s.VerifyKnown != VerifyFull {
		t.Errorf("Settings = %+v after rejected values", s)
	}
}

func TestSettingsReportsBadStoredValue(t *testing.T) {
	d := openTest(t)
	if _, err := d.db.Exec(`INSERT INTO settings(key, value) VALUES ('day_starts_at', 'noon')`); err != nil {
		t.Fatal(err)
	}
	_, err := d.Settings(context.Background())
	if err == nil || !strings.Contains(err.Error(), "day_starts_at") {
		t.Fatalf("error = %v, want one naming day_starts_at", err)
	}
}

func TestStagingWarnDaysSetting(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	s, err := d.Settings(ctx)
	if err != nil || s.StagingWarnDays != 30 {
		t.Fatalf("default = %d, %v; want 30", s.StagingWarnDays, err)
	}
	for value, want := range map[string]int{"0": 0, "45": 45} {
		if err := d.SetSetting(ctx, KeyStagingWarnDays, value); err != nil {
			t.Fatalf("SetSetting %q: %v", value, err)
		}
		if s, err = d.Settings(ctx); err != nil || s.StagingWarnDays != want {
			t.Errorf("after %q: %d, %v; want %d", value, s.StagingWarnDays, err, want)
		}
	}
	for _, bad := range []string{"-1", "x", "3651"} {
		if err := d.SetSetting(ctx, KeyStagingWarnDays, bad); err == nil {
			t.Errorf("SetSetting %q: want error", bad)
		}
	}
}

func TestArchiveSettingsRoundTrip(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	dir := t.TempDir()
	if err := d.SetSettings(ctx, map[string]string{
		KeyArchiveRescanAt: "02:30",
		KeyBackupDir:       dir,
		KeyArchiveSkip:     " Output/ ,,Exports/, *.xmp",
	}); err != nil {
		t.Fatal(err)
	}
	s, err := d.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.ArchiveRescanAt != 2*time.Hour+30*time.Minute || s.BackupDir != dir || s.ArchiveSkip != "Output/, Exports/, *.xmp" {
		t.Errorf("Settings = %+v", s)
	}
	if err := d.SetSetting(ctx, KeyArchiveSkip, ""); err != nil {
		t.Fatal(err)
	}
	if s, err = d.Settings(ctx); err != nil || s.ArchiveSkip != "" {
		t.Errorf("empty skip: %q, %v", s.ArchiveSkip, err)
	}
}

func TestArchiveSettingsReject(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	many := strings.TrimSuffix(strings.Repeat("a, ", 51), ", ")
	cases := []struct{ key, value string }{
		{KeyArchiveRescanAt, "24:00"}, {KeyArchiveRescanAt, "3:00"},
		{KeyBackupDir, "relative/dir"}, {KeyBackupDir, filepath.Join(t.TempDir(), "a\nb")},
		{KeyArchiveSkip, "a/b"}, {KeyArchiveSkip, "Output\\"}, {KeyArchiveSkip, "[x"}, {KeyArchiveSkip, "Out[/"},
		{KeyArchiveSkip, many}, {KeyArchiveSkip, "a\x01b"},
		{KeyArchiveSkip, strings.Repeat("a", 2049)},
		{KeyBackupDir, filepath.Join(t.TempDir(), strings.Repeat("d", 1030))},
	}
	for _, c := range cases {
		if err := d.SetSetting(ctx, c.key, c.value); err == nil {
			t.Errorf("SetSetting(%s, %q): expected error", c.key, c.value)
		}
	}
	var n int
	if err := d.db.QueryRowContext(ctx, `SELECT count(*) FROM settings`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d rows stored, %v; want 0", n, err)
	}
	// 50 entries is fine.
	if err := d.SetSetting(ctx, KeyArchiveSkip, strings.TrimSuffix(strings.Repeat("a, ", 50), ", ")); err != nil {
		t.Errorf("50 entries: %v", err)
	}
}

func TestParseArchiveSkip(t *testing.T) {
	got, err := ParseArchiveSkip(" Output/ ,,*.cosessiondb ")
	if err != nil || !reflect.DeepEqual(got, []string{"Output/", "*.cosessiondb"}) {
		t.Errorf("got %q, %v", got, err)
	}
	if got, err = ParseArchiveSkip(""); err != nil || len(got) != 0 {
		t.Errorf("empty: %q, %v", got, err)
	}
}

func TestSettingErrorFields(t *testing.T) {
	d := openTest(t)
	var se *SettingError
	err := d.SetSetting(t.Context(), KeyFreeSpaceReserveGB, "abc")
	if !errors.As(err, &se) {
		t.Fatalf("error %v is not a *SettingError", err)
	}
	if se.Key != KeyFreeSpaceReserveGB || se.Value != "abc" || se.Reason != "want a whole number of GB" {
		t.Errorf("SettingError = %+v", se)
	}
	if want := `setting free_space_reserve_gb = "abc": want a whole number of GB`; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	for _, bad := range []string{"x", "-1", "3651"} {
		err = d.SetSetting(t.Context(), KeyStagingWarnDays, bad)
		if !errors.As(err, &se) || se.Reason != "want 0 (off) to 3650 days" {
			t.Errorf("staging_warn_days %q: %v", bad, err)
		}
	}
	err = d.SetSetting(t.Context(), "nope", "1")
	if err == nil || errors.As(err, &se) || err.Error() != `unknown setting "nope"` {
		t.Errorf("unknown key: %v", err)
	}
}

func TestSettingsStrings(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	def := DefaultSettings()
	m := def.Strings()
	keys := SettingKeys()
	if len(keys) != 7 || len(m) != 7 {
		t.Fatalf("%d keys, %d strings; want 7", len(keys), len(m))
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("Strings() lacks %s", k)
		}
	}
	want := map[string]string{
		KeyDayStartsAt: "04:00", KeyVerifyKnown: "quick", KeyFreeSpaceReserveGB: "50",
		KeyStagingWarnDays: "30", KeyArchiveRescanAt: "03:00", KeyBackupDir: "", KeyArchiveSkip: DefaultArchiveSkip,
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("Strings() = %v, want %v", m, want)
	}
	if err := d.SetSettings(ctx, m); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Settings(ctx); err != nil || got != def {
		t.Errorf("after feeding Strings back: %+v, %v", got, err)
	}
}

func TestSetSettingsIsAtomic(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	err := d.SetSettings(ctx, map[string]string{KeyDayStartsAt: "05:00", KeyVerifyKnown: "fast"})
	var se *SettingError
	if !errors.As(err, &se) || se.Key != KeyVerifyKnown {
		t.Fatalf("error = %v, want a SettingError for verify_known", err)
	}
	var n int
	if err := d.db.QueryRowContext(ctx, `SELECT count(*) FROM settings`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d rows stored, %v; want 0", n, err)
	}
	if err := d.SetSettings(ctx, map[string]string{}); err == nil {
		t.Error("empty map: want error")
	}
	if err := d.SetSettings(ctx, map[string]string{"nope": "1"}); err == nil {
		t.Error("unknown key: want error")
	}
}
