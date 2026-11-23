package catalog

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"
)

// VerifyMode is how known files are re-checked on import.
type VerifyMode string

const (
	VerifyQuick VerifyMode = "quick"
	VerifyFull  VerifyMode = "full"
)

// Setting keys stored in the settings table.
const (
	KeyDayStartsAt        = "day_starts_at"
	KeyVerifyKnown        = "verify_known"
	KeyFreeSpaceReserveGB = "free_space_reserve_gb"
)

// Settings are the user-tunable options, with defaults for unset keys.
type Settings struct {
	DayStartsAt  time.Duration // folder-day cutoff, from "HH:MM"
	VerifyKnown  VerifyMode
	ReserveBytes uint64 // free-space reserve on the photos share
}

// DefaultSettings returns the values used when a key is unset.
func DefaultSettings() Settings {
	return Settings{DayStartsAt: 4 * time.Hour, VerifyKnown: VerifyQuick, ReserveBytes: 50_000_000_000}
}

var dayStartRE = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// apply parses value for key and stores the result in s.
func (s *Settings) apply(key, value string) error {
	var err error
	switch key {
	case KeyDayStartsAt:
		if !dayStartRE.MatchString(value) {
			err = fmt.Errorf("want HH:MM, 00:00 to 23:59")
			break
		}
		h, _ := strconv.Atoi(value[:2])
		m, _ := strconv.Atoi(value[3:])
		s.DayStartsAt = time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
	case KeyVerifyKnown:
		switch VerifyMode(value) {
		case VerifyQuick, VerifyFull:
			s.VerifyKnown = VerifyMode(value)
		default:
			err = fmt.Errorf("want %q or %q", VerifyQuick, VerifyFull)
		}
	case KeyFreeSpaceReserveGB:
		var n uint64
		n, err = strconv.ParseUint(value, 10, 64)
		if err == nil && n > math.MaxUint64/1_000_000_000 {
			err = fmt.Errorf("too large")
		}
		if err == nil {
			s.ReserveBytes = n * 1_000_000_000
		}
	default:
		return fmt.Errorf("unknown setting %q", key)
	}
	if err != nil {
		return fmt.Errorf("setting %s = %q: %w", key, value, err)
	}
	return nil
}

// Settings reads all settings, filling unset ones with defaults. Unknown keys
// are ignored so an older binary tolerates a newer one's rows.
func (d *DB) Settings(ctx context.Context) (Settings, error) {
	s := DefaultSettings()
	rows, err := d.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return s, fmt.Errorf("read settings: %w", err)
	}
	kv := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return s, errors.Join(fmt.Errorf("read settings: %w", err), rows.Close())
		}
		kv[k] = v
	}
	if err := rows.Err(); err != nil {
		return s, errors.Join(fmt.Errorf("read settings: %w", err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return s, fmt.Errorf("read settings: %w", err)
	}
	for _, key := range []string{KeyDayStartsAt, KeyVerifyKnown, KeyFreeSpaceReserveGB} {
		if v, ok := kv[key]; ok {
			if err := s.apply(key, v); err != nil {
				return DefaultSettings(), err
			}
		}
	}
	return s, nil
}

// SetSetting validates and stores one setting.
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	var scratch Settings
	if err := scratch.apply(key, value); err != nil {
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO settings(key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("store setting %s: %w", key, err)
	}
	return nil
}
