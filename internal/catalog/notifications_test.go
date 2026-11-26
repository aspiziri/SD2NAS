package catalog

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

var notifT0 = time.Date(2026, 3, 4, 5, 6, 7, 123_000_000, time.UTC)

func addN(t *testing.T, d *DB, key string, at time.Time) {
	t.Helper()
	added, err := d.AddNotification(t.Context(), Notification{Key: key, Level: NoticeWarning, Message: "m " + key, CreatedAt: at})
	if err != nil || !added {
		t.Fatalf("add %s = %v, %v", key, added, err)
	}
}

func TestAddNotificationOnce(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	n := Notification{Key: "k", Level: NoticeAlert, Message: "hello", CreatedAt: notifT0.Add(456 * time.Microsecond)}
	for i, want := range []bool{true, false} {
		added, err := d.AddNotification(ctx, n)
		if err != nil || added != want {
			t.Fatalf("add #%d = %v, %v; want %v", i+1, added, err, want)
		}
	}
	got, err := d.PendingNotifications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Notification{{ID: 1, Key: "k", Level: NoticeAlert, Message: "hello", CreatedAt: notifT0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pending = %+v, want %+v", got, want)
	}
}

func TestAddNotificationRejects(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	for name, n := range map[string]Notification{
		"empty key":     {Key: "", Level: NoticeNormal, Message: "x", CreatedAt: notifT0},
		"empty message": {Key: "a", Level: NoticeNormal, Message: "", CreatedAt: notifT0},
		"control only":  {Key: "b", Level: NoticeNormal, Message: "\n\t \r", CreatedAt: notifT0},
		"bad level":     {Key: "c", Level: "loud", Message: "x", CreatedAt: notifT0},
	} {
		added, err := d.AddNotification(ctx, n)
		if err == nil || added {
			t.Errorf("%s: added = %v, err = %v; want an error", name, added, err)
		}
	}
	got, err := d.PendingNotifications(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("pending = %+v, %v; want none", got, err)
	}
}

func TestAddNotificationSanitizes(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	long := strings.Repeat("é€😀", 250) // 2+3+4 bytes per repeat = 2250 bytes
	if len(long) < 1500 {
		t.Fatalf("test message is %d bytes", len(long))
	}
	for key, msg := range map[string]string{"a": "a\nb\tc ", "b": long} {
		if _, err := d.AddNotification(ctx, Notification{Key: key, Level: NoticeNormal, Message: msg, CreatedAt: notifT0}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.PendingNotifications(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("pending = %+v, %v", got, err)
	}
	byKey := map[string]string{got[0].Key: got[0].Message, got[1].Key: got[1].Message}
	if byKey["a"] != "a b c" {
		t.Errorf("message a = %q, want %q", byKey["a"], "a b c")
	}
	b := byKey["b"]
	if len(b) > 1000 || !utf8.ValidString(b) || !strings.HasSuffix(b, "…") {
		t.Errorf("message b: %d bytes, valid %v, suffix %q", len(b), utf8.ValidString(b), b[max(0, len(b)-6):])
	}
	if !strings.HasPrefix(b, "é€😀é€😀") {
		t.Errorf("message b lost its start: %q", b[:20])
	}
}

func TestTakeNotifications(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	keys := []string{"x", "y", "z"}
	for _, k := range keys {
		addN(t, d, k, notifT0)
	}
	got, err := d.TakeNotifications(ctx, notifT0)
	if err != nil || len(got) != 3 {
		t.Fatalf("take = %+v, %v", got, err)
	}
	for i, n := range got {
		if n.ID != int64(i+1) || n.Key != keys[i] {
			t.Errorf("take[%d] = %+v", i, n)
		}
	}
	again, err := d.TakeNotifications(ctx, notifT0)
	if err != nil || again == nil || len(again) != 0 {
		t.Fatalf("second take = %#v, %v; want empty non-nil", again, err)
	}
	for _, k := range keys {
		if ok, err := d.HasNotification(ctx, k); err != nil || !ok {
			t.Errorf("HasNotification(%s) = %v, %v; want true", k, ok, err)
		}
	}
	pending, err := d.PendingNotifications(ctx)
	if err != nil || pending == nil || len(pending) != 0 {
		t.Fatalf("pending = %#v, %v; want empty non-nil", pending, err)
	}
}

func TestTakeNotificationsCap(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	for i := 1; i <= 105; i++ {
		addN(t, d, fmt.Sprintf("k%d", i), notifT0)
	}
	first, err := d.TakeNotifications(ctx, notifT0)
	if err != nil || len(first) != MaxTake || first[0].ID != 1 || first[MaxTake-1].ID != MaxTake {
		t.Fatalf("first take: %d notices, %v", len(first), err)
	}
	second, err := d.TakeNotifications(ctx, notifT0)
	if err != nil || len(second) != 5 || second[0].ID != 101 || second[4].ID != 105 {
		t.Fatalf("second take = %+v, %v", second, err)
	}
}

func TestNotificationsPruned(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	now := notifT0
	addN(t, d, "old", now.Add(-31*24*time.Hour))
	if ok, err := d.HasNotification(ctx, "old"); err != nil || !ok {
		t.Fatalf("old before prune = %v, %v", ok, err)
	}
	addN(t, d, "new", now)
	if ok, err := d.HasNotification(ctx, "old"); err != nil || ok {
		t.Errorf("old after prune = %v, %v; want gone", ok, err)
	}
	if ok, err := d.HasNotification(ctx, "new"); err != nil || !ok {
		t.Errorf("new = %v, %v; want kept", ok, err)
	}
	addN(t, d, "old", now) // a pruned key can be queued again
}

func TestTakeConcurrent(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	for i := 1; i <= 20; i++ {
		addN(t, d, fmt.Sprintf("k%d", i), notifT0)
	}
	var wg sync.WaitGroup
	results := make([][]Notification, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = d.TakeNotifications(ctx, notifT0)
		}()
	}
	wg.Wait()
	seen := map[int64]int{}
	for i := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		for _, n := range results[i] {
			seen[n.ID]++
		}
	}
	if len(seen) != 20 {
		t.Errorf("took %d distinct notices, want 20", len(seen))
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("notice %d returned %d times", id, c)
		}
	}
}

func TestLatestNotificationTime(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	if got, err := d.LatestNotificationTime(ctx, "missing:"); err != nil || !got.IsZero() {
		t.Fatalf("none = %v, %v; want zero, nil", got, err)
	}
	addN(t, d, "missing:2026-01-01T00:00:00.5Z", notifT0)
	addN(t, d, "missing:2026-01-02T00:00:00Z", notifT0)
	addN(t, d, "backup:2026-01-03T00:00:00Z", notifT0)
	want := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if got, err := d.LatestNotificationTime(ctx, "missing:"); err != nil || !got.Equal(want) {
		t.Errorf("latest = %v, %v; want %v", got, err, want)
	}
	// the prefix is matched literally: _ is no wildcard
	if got, err := d.LatestNotificationTime(ctx, "missin_:"); err != nil || !got.IsZero() {
		t.Errorf("missin_: = %v, %v; want zero, nil", got, err)
	}
	addN(t, d, "missing:soon", notifT0)
	if _, err := d.LatestNotificationTime(ctx, "missing:"); err == nil || !strings.Contains(err.Error(), `"missing:soon"`) {
		t.Errorf("unparsable key: err = %v, want one naming the key", err)
	}
}
