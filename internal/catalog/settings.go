package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
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
	KeyStagingWarnDays    = "staging_warn_days"
	KeyArchiveRescanAt    = "archive_rescan_at"
	KeyBackupDir          = "backup_dir"
	KeyArchiveSkip        = "archive_skip"
	KeyStagingNoticeAt    = "staging_notice_at"
)

// DefaultArchiveSkip is the archive indexer's default skip list, as the
// archive_skip setting's canonical text.
const DefaultArchiveSkip = "Output/, CaptureOne/, *.cosessiondb"

// SettingError is a value that a known setting does not accept.
type SettingError struct{ Key, Value, Reason string }

func (e *SettingError) Error() string {
	return fmt.Sprintf("setting %s = %q: %s", e.Key, e.Value, e.Reason)
}

// settingKeys is every known key, in the order Settings and Strings use.
var settingKeys = []string{
	KeyDayStartsAt, KeyVerifyKnown, KeyFreeSpaceReserveGB, KeyStagingWarnDays,
	KeyArchiveRescanAt, KeyBackupDir, KeyArchiveSkip, KeyStagingNoticeAt,
}

// SettingKeys returns every known setting key, in a fixed order.
func SettingKeys() []string { return append([]string(nil), settingKeys...) }

// Settings are the user-tunable options, with defaults for unset keys.
type Settings struct {
	DayStartsAt     time.Duration // folder-day cutoff, from "HH:MM"
	VerifyKnown     VerifyMode
	ReserveBytes    uint64        // free-space reserve on the photos share
	StagingWarnDays int           // days in staging before a folder is flagged; 0 = never
	ArchiveRescanAt time.Duration // nightly archive pass, since local midnight
	BackupDir       string        // "" = <photos>/Management/SD2NAS (the indexer resolves it)
	ArchiveSkip     string        // canonical archive_skip text
	StagingNoticeAt time.Duration // daily stale-staging check, since local midnight
}

// DefaultSettings returns the values used when a key is unset.
func DefaultSettings() Settings {
	return Settings{DayStartsAt: 4 * time.Hour, VerifyKnown: VerifyQuick, ReserveBytes: 50_000_000_000, StagingWarnDays: 30,
		ArchiveRescanAt: 3 * time.Hour, ArchiveSkip: DefaultArchiveSkip, StagingNoticeAt: 9 * time.Hour}
}

var dayStartRE = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// parseClock parses "HH:MM" as a duration since midnight.
func parseClock(value string) (time.Duration, bool) {
	if !dayStartRE.MatchString(value) {
		return 0, false
	}
	h, _ := strconv.Atoi(value[:2])
	m, _ := strconv.Atoi(value[3:])
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute, true
}

const clockReason = "want HH:MM, 00:00 to 23:59"

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

const archiveSkipReason = `want a comma-separated list such as "Output/, CaptureOne/, *.cosessiondb"`

// ParseArchiveSkip splits and validates an archive_skip value: the entries in
// order (trimmed, empty ones dropped), as written. Matching is the indexer's.
func ParseArchiveSkip(s string) ([]string, error) {
	if len(s) > 2048 || hasControl(s) {
		return nil, errors.New(archiveSkipReason)
	}
	var entries []string
	for _, e := range strings.Split(s, ",") {
		if e = strings.TrimSpace(e); e != "" {
			entries = append(entries, e)
		}
	}
	if len(entries) > 50 {
		return nil, errors.New(archiveSkipReason)
	}
	for _, e := range entries {
		p := strings.TrimSuffix(e, "/")
		if p == "" || strings.ContainsAny(p, `/\`) {
			return nil, errors.New(archiveSkipReason)
		}
		if _, err := path.Match(p, ""); errors.Is(err, path.ErrBadPattern) {
			return nil, errors.New(archiveSkipReason)
		}
		if _, err := path.Match(p, "x"); errors.Is(err, path.ErrBadPattern) {
			return nil, errors.New(archiveSkipReason)
		}
	}
	return entries, nil
}

// apply parses value for key and stores the result in s. A bad value for a
// known key is a *SettingError.
func (s *Settings) apply(key, value string) error {
	var reason string
	switch key {
	case KeyDayStartsAt:
		if d, ok := parseClock(value); ok {
			s.DayStartsAt = d
		} else {
			reason = clockReason
		}
	case KeyArchiveRescanAt:
		if d, ok := parseClock(value); ok {
			s.ArchiveRescanAt = d
		} else {
			reason = clockReason
		}
	case KeyStagingNoticeAt:
		if d, ok := parseClock(value); ok {
			s.StagingNoticeAt = d
		} else {
			reason = clockReason
		}
	case KeyVerifyKnown:
		switch VerifyMode(value) {
		case VerifyQuick, VerifyFull:
			s.VerifyKnown = VerifyMode(value)
		default:
			reason = fmt.Sprintf("want %q or %q", VerifyQuick, VerifyFull)
		}
	case KeyFreeSpaceReserveGB:
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil || n > math.MaxUint64/1_000_000_000 {
			reason = "want a whole number of GB"
		} else {
			s.ReserveBytes = n * 1_000_000_000
		}
	case KeyStagingWarnDays:
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 3650 {
			reason = "want 0 (off) to 3650 days"
		} else {
			s.StagingWarnDays = n
		}
	case KeyBackupDir:
		if value == "" || (filepath.IsAbs(value) && len(value) <= 1024 && !hasControl(value)) {
			s.BackupDir = value
		} else {
			reason = `want an absolute path, or "" for the default`
		}
	case KeyArchiveSkip:
		entries, err := ParseArchiveSkip(value)
		if err != nil {
			reason = err.Error()
		} else {
			s.ArchiveSkip = strings.Join(entries, ", ")
		}
	default:
		return fmt.Errorf("unknown setting %q", key)
	}
	if reason != "" {
		return &SettingError{Key: key, Value: value, Reason: reason}
	}
	return nil
}

// Strings returns the canonical text of every key, in the form SetSettings
// accepts.
func (s Settings) Strings() map[string]string {
	clock := func(d time.Duration) string {
		return fmt.Sprintf("%02d:%02d", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return map[string]string{
		KeyDayStartsAt:        clock(s.DayStartsAt),
		KeyVerifyKnown:        string(s.VerifyKnown),
		KeyFreeSpaceReserveGB: strconv.FormatUint(s.ReserveBytes/1_000_000_000, 10),
		KeyStagingWarnDays:    strconv.Itoa(s.StagingWarnDays),
		KeyArchiveRescanAt:    clock(s.ArchiveRescanAt),
		KeyBackupDir:          s.BackupDir,
		KeyArchiveSkip:        s.ArchiveSkip,
		KeyStagingNoticeAt:    clock(s.StagingNoticeAt),
	}
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
	for _, key := range settingKeys {
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
	return d.SetSettings(ctx, map[string]string{key: value})
}

// SetSettings validates every pair, then stores them all in one transaction.
// Nothing is stored if any key or value is invalid.
func (d *DB) SetSettings(ctx context.Context, kv map[string]string) error {
	if len(kv) == 0 {
		return errors.New("no settings given")
	}
	canon := make(map[string]string, len(kv))
	for _, key := range slices.Sorted(maps.Keys(kv)) {
		var scratch Settings
		if err := scratch.apply(key, kv[key]); err != nil {
			return err
		}
		canon[key] = kv[key]
		if key == KeyArchiveSkip {
			canon[key] = scratch.ArchiveSkip
		}
	}
	err := d.tx(ctx, func(tx *sql.Tx) error {
		for key, value := range canon {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO settings(key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
				return fmt.Errorf("store setting %s: %w", key, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store settings: %w", err)
	}
	return nil
}
