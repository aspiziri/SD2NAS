package catalog

import (
	"context"
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
	want := Settings{4 * time.Hour, VerifyQuick, 50_000_000_000}
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
