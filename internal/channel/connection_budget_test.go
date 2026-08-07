package channel

import "testing"

func TestBudgetHoldsExactlyItsMaximum(t *testing.T) {
	const max = 3
	b := NewConnectionBudget(max)
	releases := make([]func(), 0, max)
	for i := range max {
		release, ok := b.Acquire()
		if !ok {
			t.Fatalf("acquire %d failed, want the first %d to succeed", i, max)
		}
		releases = append(releases, release)
	}
	if _, ok := b.Acquire(); ok {
		t.Fatalf("acquire %d succeeded, want it refused at the limit", max+1)
	}

	releases[0]()
	if _, ok := b.Acquire(); !ok {
		t.Fatal("acquire after a release failed, want the freed slot taken")
	}
	if _, ok := b.Acquire(); ok {
		t.Fatal("a second acquire succeeded, want only one slot freed")
	}
}

func TestBudgetReportsItsMaximum(t *testing.T) {
	if got := NewConnectionBudget(7).Max(); got != 7 {
		t.Errorf("Max = %d, want 7", got)
	}
}
