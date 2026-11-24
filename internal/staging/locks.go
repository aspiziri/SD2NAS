package staging

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Holder says who holds a lock; the API turns it into the 409 message.
type Holder string

const (
	HolderIngest    Holder = "ingest"    // a card import writing into the folder
	HolderOperation Holder = "operation" // rename, merge, move, cull, discard, forget
	HolderRescan    Holder = "rescan"    // the reconcile pass
	HolderPromote   Holder = "promote"   // a queued or running promote job
)

// ErrBusy is what every *BusyError matches with errors.Is.
var ErrBusy = errors.New("staging: folder is busy")

// BusyError reports a folder (or a directory name) whose lock someone else
// holds.
type BusyError struct {
	ID     int64  // the busy folder's id; 0 when the busy key is a directory name
	Name   string // the folder's name (filled in by the operation) or the directory name; may be ""
	Holder Holder
	Queued bool // HolderPromote only: the job is still waiting (false: it is running)
}

func (e *BusyError) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("staging folder %q is busy (%s)", e.Name, e.Holder)
	}
	return fmt.Sprintf("staging folder %d is busy (%s)", e.ID, e.Holder)
}

func (e *BusyError) Is(target error) bool { return target == ErrBusy }

// LockSet is what one operation (or one folder of a promote job) locks:
// staging folder ids, and the directory names it is about to create or
// rename into, as slash paths below the photos root ("Staging/2025-05-30 -
// Trip", "Archive/2025/05/2025-05-30 - Trip"). Names are compared without
// case, so a case-only clash on a case-insensitive disk still collides.
type LockSet struct {
	IDs   []int64
	Names []string
}

// lockKey is one lockable thing: a folder id or a lower-cased name.
type lockKey struct {
	id   int64
	name string
}

func (s LockSet) keys() ([]lockKey, map[lockKey]string) {
	shown := map[lockKey]string{} // name keys → the name as the caller wrote it
	var out []lockKey
	for _, id := range uniqueIDs(s.IDs) {
		out = append(out, lockKey{id: id})
	}
	for _, n := range s.Names {
		k := lockKey{name: strings.ToLower(n)}
		if _, dup := shown[k]; !dup {
			shown[k] = n
			out = append(out, k)
		}
	}
	return out, shown
}

// Locks are §8's folder locks: every operation, and an import while it
// writes into a folder, holds the lock of each staging folder it touches
// and of each directory name it creates (Decision 1).
type Locks struct {
	mu   sync.Mutex
	held map[lockKey]*lockState
}

type lockState struct {
	holder   Holder
	released chan struct{} // closed on release; waiters then try again
}

func NewLocks() *Locks { return &Locks{held: map[lockKey]*lockState{}} }

func uniqueIDs(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// TryLock takes the locks of folders ids at once, or none. It never waits:
// a held lock fails it with a *BusyError naming that folder.
func (l *Locks) TryLock(h Holder, ids ...int64) (release func(), err error) {
	return l.TryLockSet(h, LockSet{IDs: ids})
}

// TryLockSet is TryLock for folder ids and directory names together.
func (l *Locks) TryLockSet(h Holder, set LockSet) (func(), error) {
	rs, err := l.TryLockGroups(h, []LockSet{set})
	if err != nil {
		return nil, err
	}
	return rs[0], nil
}

// TryLockGroups takes every key of every group at once, or none, and
// returns one release per group, so a promote job can give each folder
// back as soon as it is done with it. A key that appears in several groups
// belongs to the first.
func (l *Locks) TryLockGroups(h Holder, groups []LockSet) ([]func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := map[lockKey]bool{}
	perGroup := make([][]lockKey, len(groups))
	for i, g := range groups {
		keys, shown := g.keys()
		for _, k := range keys {
			if seen[k] {
				continue
			}
			seen[k] = true
			if st, ok := l.held[k]; ok {
				return nil, &BusyError{ID: k.id, Name: shown[k], Holder: st.holder}
			}
			perGroup[i] = append(perGroup[i], k)
		}
	}
	out := make([]func(), len(groups))
	for i, keys := range perGroup {
		rs := make([]func(), len(keys))
		for j, k := range keys {
			rs[j] = l.take(k, h)
		}
		out[i] = func() {
			for _, r := range rs {
				r()
			}
		}
	}
	return out, nil
}

// take records h as k's holder (l.mu held) and returns its release, which
// is safe to call more than once.
func (l *Locks) take(k lockKey, h Holder) func() {
	st := &lockState{holder: h, released: make(chan struct{})}
	l.held[k] = st
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if l.held[k] == st {
				delete(l.held, k)
			}
			l.mu.Unlock()
			close(st.released)
		})
	}
}

// Lock takes folder id's lock, waiting while someone else holds it, until
// ctx ends. Only an import waits (Decision 1); everything else tries.
func (l *Locks) Lock(ctx context.Context, h Holder, id int64) (func(), error) {
	k := lockKey{id: id}
	for {
		l.mu.Lock()
		st, ok := l.held[k]
		if !ok {
			release := l.take(k, h)
			l.mu.Unlock()
			return release, nil
		}
		l.mu.Unlock()
		select {
		case <-st.released:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// NameHeld reports whether directory name rel (a slash path below the
// photos root) or one of its parent directories is locked as a name, e.g.
// by a promote writing into Archive/<target>; folder-id locks don't count.
// Case is ignored, like TryLockSet's name keys. The archive indexer skips
// such directories (M5 Decision 12).
func (l *Locks) NameHeld(rel string) bool {
	k := strings.ToLower(strings.Trim(rel, "/"))
	l.mu.Lock()
	defer l.mu.Unlock()
	for k != "" {
		if _, ok := l.held[lockKey{name: k}]; ok {
			return true
		}
		i := strings.LastIndex(k, "/")
		if i < 0 {
			return false
		}
		k = k[:i]
	}
	return false
}
