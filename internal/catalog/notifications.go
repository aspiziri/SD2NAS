package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Notification levels, as Unraid's notify command names them.
const (
	NoticeNormal  = "normal"
	NoticeWarning = "warning"
	NoticeAlert   = "alert"
)

// NoticeKeep is how long a notification is kept after it was created.
const NoticeKeep = 30 * 24 * time.Hour

// MaxTake is the most notifications one take returns.
const MaxTake = 100

// maxNoticeBytes is the longest stored message, the ellipsis included.
const maxNoticeBytes = 1000

// Notification is a notice for the Unraid host, queued until the card script takes it.
type Notification struct {
	ID        int64     `json:"id"`
	Key       string    `json:"key"`   // unique: the same event is never queued twice
	Level     string    `json:"level"` // NoticeNormal | NoticeWarning | NoticeAlert
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// oneLine turns control characters into spaces, trims, and cuts the result to
// maxNoticeBytes at a rune boundary, ending a cut message with "…".
func oneLine(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
	if len(s) <= maxNoticeBytes {
		return s
	}
	const ellipsis = "…"
	cut := maxNoticeBytes - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// AddNotification stores n unless one with n.Key exists (added = false). In
// the same transaction it deletes notifications created more than
// NoticeKeep before n.CreatedAt. It stores the message as one line
// (control characters become spaces, trimmed) of at most 1000 bytes, cut
// at a rune boundary and ended with "…"; the text take relies on it. An
// empty key, a message empty after that, or an unknown level is an error.
func (d *DB) AddNotification(ctx context.Context, n Notification) (added bool, err error) {
	switch n.Level {
	case NoticeNormal, NoticeWarning, NoticeAlert:
	default:
		return false, fmt.Errorf("add notification %q: unknown level %q", n.Key, n.Level)
	}
	if n.Key == "" {
		return false, errors.New("add notification: empty key")
	}
	msg := oneLine(n.Message)
	if msg == "" {
		return false, fmt.Errorf("add notification %q: empty message", n.Key)
	}
	err = d.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM notifications WHERE created_at < ?`, formatTime(n.CreatedAt.Add(-NoticeKeep))); err != nil {
			return fmt.Errorf("prune notifications: %w", err)
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO notifications(key, level, message, created_at) VALUES (?,?,?,?) ON CONFLICT(key) DO NOTHING`,
			n.Key, n.Level, msg, formatTime(n.CreatedAt))
		if err != nil {
			return err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return err
		}
		added = rows == 1
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("add notification %q: %w", n.Key, err)
	}
	return added, nil
}

// HasNotification reports whether a notification with key is stored, taken or not.
func (d *DB) HasNotification(ctx context.Context, key string) (bool, error) {
	var one int
	err := d.db.QueryRowContext(ctx, `SELECT 1 FROM notifications WHERE key = ?`, key).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up notification %q: %w", key, err)
	}
	return true, nil
}

// LatestNotificationTime returns the time in the key of the newest (highest
// id) notification whose key starts with prefix, the rest of the key read
// as RFC 3339; zero when there is none. A key whose rest is no time is an
// error naming the key.
func (d *DB) LatestNotificationTime(ctx context.Context, prefix string) (time.Time, error) {
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	var key string
	err := d.db.QueryRowContext(ctx, `SELECT key FROM notifications WHERE key LIKE ? ESCAPE '\' ORDER BY id DESC LIMIT 1`,
		esc+"%").Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("latest notification %q: %w", prefix, err)
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(key, prefix))
	if err != nil {
		return time.Time{}, fmt.Errorf("latest notification %q: %w", key, err)
	}
	return t, nil
}

// PendingNotifications: not taken yet, oldest (lowest id) first, at most MaxTake. Never nil.
func (d *DB) PendingNotifications(ctx context.Context) ([]Notification, error) {
	out := []Notification{}
	err := d.tx(ctx, func(tx *sql.Tx) (err error) {
		out, err = pendingNotifications(ctx, tx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list pending notifications: %w", err)
	}
	return out, nil
}

// TakeNotifications marks the pending ones (oldest first, at most MaxTake)
// taken at now and returns them, in one transaction. Never nil.
func (d *DB) TakeNotifications(ctx context.Context, now time.Time) ([]Notification, error) {
	out := []Notification{}
	err := d.tx(ctx, func(tx *sql.Tx) (err error) {
		out, err = pendingNotifications(ctx, tx)
		if err != nil || len(out) == 0 {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE notifications SET taken_at = ? WHERE taken_at IS NULL AND id <= ?`,
			formatTime(now), out[len(out)-1].ID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("take notifications: %w", err)
	}
	return out, nil
}

// pendingNotifications reads the untaken notices fully and closes the rows.
func pendingNotifications(ctx context.Context, tx *sql.Tx) ([]Notification, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, key, level, message, created_at FROM notifications WHERE taken_at IS NULL ORDER BY id LIMIT ?`, MaxTake)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var created string
		if err := rows.Scan(&n.ID, &n.Key, &n.Level, &n.Message, &created); err != nil {
			return nil, err
		}
		if n.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("notification %d: bad created_at %q: %w", n.ID, created, err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, rows.Close()
}
