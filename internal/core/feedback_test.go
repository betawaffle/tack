package core

import (
	"errors"
	"testing"

	"github.com/betawaffle/tack/internal/store"
)

func TestFeedbackIsKeptUntilReadAndThenStays(t *testing.T) {
	f := newFake(t)
	c := f.open()
	zero, five := 0, 5
	whole, err := c.AddFeedback(store.Feedback{Revision: "r1", Text: "Generally good."})
	if err != nil {
		t.Fatal(err)
	}
	ranged, err := c.AddFeedback(store.Feedback{Revision: "r1", Path: "main.go", StartLine: 3, EndLine: 3, StartColumn: &zero, EndColumn: &five, Quote: "func ", Text: "Rename this."})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := c.AddFeedback(store.Feedback{Revision: "r1", Path: "main.go", Text: "Never mind."})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveFeedback(gone); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]store.Feedback{
		"empty":          {Text: " "},
		"lines, no file": {StartLine: 1, EndLine: 1, Text: "x"},
		"backwards":      {Path: "a", StartLine: 3, EndLine: 2, Text: "x"},
		"one column":     {Path: "a", StartLine: 1, EndLine: 1, StartColumn: &zero, Text: "x"},
		"empty range":    {Path: "a", StartLine: 1, EndLine: 1, StartColumn: &five, EndColumn: &five, Text: "x"},
		"no start line":  {Path: "a", EndLine: 2, Text: "x"},
		"bad text":       {Text: "\xff"},
	} {
		if _, err := c.AddFeedback(bad); !errors.Is(err, ErrBadFeedback) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := c.MarkFeedbackRead([]string{ranged}, "claude"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveFeedback(ranged); !errors.Is(err, ErrBadFeedback) {
		t.Errorf("removing what's been read: %v", err)
	}
	if err := c.RemoveFeedback("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing nothing: %v", err)
	}
	check := func(c *Core) {
		t.Helper()
		got := c.Feedback()
		if len(got) != 2 || got[0].ID != whole || got[0].Read != nil || got[1].ID != ranged || got[1].Read == nil || *got[1].EndColumn != 5 || got[1].Quote != "func " {
			t.Errorf("feedback = %+v", got)
		}
	}
	check(c)
	check(f.open())
}
