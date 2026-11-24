package staging

import (
	"slices"
	"testing"
)

func TestStatusAndTouch(t *testing.T) {
	e := newOpsEnv(t)
	if st := e.svc.Status(); st.Version != 0 {
		t.Fatalf("new service status = %+v", st)
	}
	e.svc.Touch()
	e.svc.Touch()
	if v := e.svc.Status().Version; v != 2 {
		t.Fatalf("version = %d, want 2", v)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var got []uint64
	for _, st := range e.changes {
		got = append(got, st.Version)
	}
	if !slices.Equal(got, []uint64{1, 2}) {
		t.Fatalf("OnChange saw %v, want [1 2]", got)
	}
}

func TestCloseIdempotent(t *testing.T) {
	e := newOpsEnv(t)
	e.svc.Close()
	e.svc.Close()
	e.svc.mu.Lock()
	defer e.svc.mu.Unlock()
	if !e.svc.closed {
		t.Fatal("service not closed")
	}
}
