package staging

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func wantBusy(t *testing.T, err error, id int64, name string, h Holder) {
	t.Helper()
	var be *BusyError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v, want *BusyError", err)
	}
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("errors.Is(%v, ErrBusy) = false", err)
	}
	if be.ID != id || be.Name != name || be.Holder != h {
		t.Fatalf("BusyError = %+v, want ID %d Name %q Holder %q", *be, id, name, h)
	}
}

func mustTry(t *testing.T, l *Locks, h Holder, ids ...int64) func() {
	t.Helper()
	r, err := l.TryLock(h, ids...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTryLockAllOrNothing(t *testing.T) {
	l := NewLocks()
	rel := mustTry(t, l, HolderOperation, 1, 2)
	_, err := l.TryLock(HolderIngest, 2, 3)
	wantBusy(t, err, 2, "", HolderOperation)
	mustTry(t, l, HolderIngest, 3)() // 3 was not taken by the failed call
	rel()

	r4 := mustTry(t, l, HolderOperation, 4, 4)
	if _, err := l.TryLock(HolderIngest, 4); err == nil {
		t.Fatal("4 should be held")
	}
	r4() // one release frees it
	mustTry(t, l, HolderIngest, 4)()
}

func TestReleaseIdempotent(t *testing.T) {
	l := NewLocks()
	first := mustTry(t, l, HolderOperation, 1)
	first()
	second := mustTry(t, l, HolderIngest, 1)
	first() // must not free the second holder's lock
	_, err := l.TryLock(HolderRescan, 1)
	wantBusy(t, err, 1, "", HolderIngest)
	second()
	second()
	mustTry(t, l, HolderRescan, 1)()
}

func TestLockWaitsForRelease(t *testing.T) {
	l := NewLocks()
	a := mustTry(t, l, HolderOperation, 7)
	got := make(chan func(), 1)
	go func() {
		r, err := l.Lock(t.Context(), HolderIngest, 7)
		if err != nil {
			t.Error(err)
		}
		got <- r
	}()
	select {
	case <-got:
		t.Fatal("Lock returned while the folder was held")
	case <-time.After(100 * time.Millisecond): // negative check: it must still be waiting
	}
	a()
	select {
	case r := <-got:
		if r == nil {
			t.Fatal("no release")
		}
		_, err := l.TryLock(HolderOperation, 7)
		wantBusy(t, err, 7, "", HolderIngest)
		r()
	case <-time.After(10 * time.Second):
		t.Fatal("Lock did not return after the release")
	}
}

func TestLockContextCancel(t *testing.T) {
	l := NewLocks()
	a := mustTry(t, l, HolderOperation, 7)
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		_, err := l.Lock(ctx, HolderIngest, 7)
		errc <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<20)
	for !strings.Contains(string(buf[:runtime.Stack(buf, true)]), "(*Locks).Lock(") { // wait until it is waiting
		if time.Now().After(deadline) {
			t.Fatal("Lock never started waiting")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Lock did not return after cancel")
	}
	a()
	mustTry(t, l, HolderIngest, 7)() // the cancelled Lock holds nothing
}

func TestTryLockNames(t *testing.T) {
	l := NewLocks()
	rel, err := l.TryLockSet(HolderOperation, LockSet{Names: []string{"Staging/2025-05-30 - Trip"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.TryLockSet(HolderRescan, LockSet{Names: []string{"staging/2025-05-30 - TRIP"}})
	wantBusy(t, err, 0, "staging/2025-05-30 - TRIP", HolderOperation)

	other, err := l.TryLockSet(HolderRescan, LockSet{IDs: []int64{1}, Names: []string{"Staging/other"}})
	if err != nil {
		t.Fatal(err)
	}
	other()

	_, err = l.TryLockSet(HolderRescan, LockSet{IDs: []int64{5}, Names: []string{"STAGING/2025-05-30 - trip"}})
	wantBusy(t, err, 0, "STAGING/2025-05-30 - trip", HolderOperation)
	mustTry(t, l, HolderIngest, 5)() // the free id was not taken
	rel()
}

func TestTryLockGroups(t *testing.T) {
	l := NewLocks()
	groups := []LockSet{{IDs: []int64{1}, Names: []string{"Archive/A"}}, {IDs: []int64{2}, Names: []string{"Archive/A"}}}
	rs, err := l.TryLockGroups(HolderPromote, groups)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("%d releases, want 2", len(rs))
	}
	rs[0]() // frees 1 and Archive/A, not 2
	mustTry(t, l, HolderIngest, 1)()
	_, err = l.TryLock(HolderIngest, 2)
	wantBusy(t, err, 2, "", HolderPromote)
	rn, err := l.TryLockSet(HolderOperation, LockSet{Names: []string{"archive/a"}})
	if err != nil {
		t.Fatal(err)
	}
	rn()
	rs[1]()

	held := mustTry(t, l, HolderOperation, 2)
	_, err = l.TryLockGroups(HolderPromote, groups)
	wantBusy(t, err, 2, "", HolderOperation)
	mustTry(t, l, HolderIngest, 1)() // nothing was taken
	rn2, err := l.TryLockSet(HolderIngest, LockSet{Names: []string{"Archive/A"}})
	if err != nil {
		t.Fatal(err)
	}
	rn2()
	held()
}

func TestLocksConcurrent(t *testing.T) {
	l := NewLocks()
	var counters [6]atomic.Int32
	var wg sync.WaitGroup
	for g := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				id := int64((g+i)%5 + 1)
				var release func()
				var err error
				switch (g + i) % 3 {
				case 0:
					release, err = l.TryLock(HolderOperation, id)
				case 1:
					release, err = l.TryLockSet(HolderRescan, LockSet{IDs: []int64{id}, Names: []string{"n" + strconv.FormatInt(id, 10)}})
				default:
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
					release, err = l.Lock(ctx, HolderIngest, id)
					cancel()
				}
				if err != nil {
					continue
				}
				if n := counters[id].Add(1); n > 1 {
					t.Errorf("folder %d held by %d holders", id, n)
				}
				counters[id].Add(-1)
				release()
			}
		}()
	}
	wg.Wait()
}
