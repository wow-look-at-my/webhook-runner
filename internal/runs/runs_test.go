package runs

import (
	"strings"
	"testing"
)

func TestRunOutputBounded(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	for i := 0; i < MaxOutputLines+50; i++ {
		r.AppendOutput("line")
	}
	if got := len(r.Output); got != MaxOutputLines {
		t.Errorf("len = %d, want %d", got, MaxOutputLines)
	}
}

func TestNewIDUniqueAndShape(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		id := newID()
		if len(id) != 26 {
			t.Fatalf("len(%q) = %d, want 26", id, len(id))
		}
		if strings.ContainsAny(id, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			t.Errorf("uppercase letters in %q", id)
		}
		if _, dup := seen[id]; dup {
			t.Errorf("duplicate id: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestFinishIsTerminal(t *testing.T) {
	tr := NewTracker()
	r := tr.New("h")
	r.Finish(StatusSuccess, 0, "")
	r.Finish(StatusFailure, 1, "")
	if r.Status != StatusSuccess {
		t.Errorf("Status changed after Finish: %v", r.Status)
	}
	select {
	case <-r.Done():
	default:
		t.Errorf("Done channel not closed")
	}
}

func TestPerHookEviction(t *testing.T) {
	tr := NewTracker()
	tr.maxByHook = 3
	for i := 0; i < 10; i++ {
		tr.New("h")
	}
	if got := len(tr.ListByHook("h", 0)); got != 3 {
		t.Errorf("retained = %d, want 3", got)
	}
}
