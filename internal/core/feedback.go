package core

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/betawaffle/tack/internal/store"
)

// Comments about tack's own code and documentation are kept in the record, so they last
// and say when they were written. Someone writes them as they browse, and they're read
// later as a batch, at which point they're marked read.

// MaxFeedbackBytes is the most a comment, or what it quotes, can be.
const MaxFeedbackBytes = 64 << 10

// FeedbackItem is a comment and what's happened to it.
type FeedbackItem struct {
	store.Feedback
	Time time.Time `json:"time"`
	// Read is when it was read, if it has been.
	Read *time.Time `json:"read,omitzero"`
}

// ErrBadFeedback is a comment that can't be kept as it is.
var ErrBadFeedback = errors.New("bad feedback")

// The caller holds mu, or is New.
func (c *Core) applyFeedback(ev *store.Event) {
	switch ev.Type {
	case store.FeedbackAdded:
		if ev.Feedback != nil {
			c.feedback = append(c.feedback, &FeedbackItem{Feedback: *ev.Feedback, Time: ev.Time})
		}
	case store.FeedbackRemoved:
		if ev.Feedback != nil {
			c.feedback = slices.DeleteFunc(c.feedback, func(f *FeedbackItem) bool { return f.ID == ev.Feedback.ID })
		}
	case store.FeedbackRead:
		for _, f := range c.feedback {
			if slices.Contains(ev.FeedbackIDs, f.ID) && f.Read == nil {
				when := ev.Time
				f.Read = &when
			}
		}
	}
}

// AddFeedback keeps a comment, and returns its ID.
func (c *Core) AddFeedback(f store.Feedback) (string, error) {
	if strings.TrimSpace(f.Text) == "" {
		return "", fmt.Errorf("%w: it says nothing", ErrBadFeedback)
	}
	if len(f.Text) > MaxFeedbackBytes || len(f.Quote) > MaxFeedbackBytes {
		return "", fmt.Errorf("%w: a comment and its quote can each be at most %d bytes", ErrBadFeedback, MaxFeedbackBytes)
	}
	if !utf8.ValidString(f.Text) || !utf8.ValidString(f.Quote) || !utf8.ValidString(f.Path) {
		return "", fmt.Errorf("%w: it isn't valid UTF-8", ErrBadFeedback)
	}
	switch {
	case f.Path == "" && (f.StartLine != 0 || f.EndLine != 0 || f.Quote != ""):
		return "", fmt.Errorf("%w: lines need a file", ErrBadFeedback)
	case f.StartLine < 0 || f.EndLine < f.StartLine || f.StartLine == 0 && f.EndLine != 0:
		return "", fmt.Errorf("%w: the lines run from start to end, from 1", ErrBadFeedback)
	case (f.StartColumn == nil) != (f.EndColumn == nil) || f.StartColumn != nil && (f.StartLine == 0 || *f.StartColumn < 0 || *f.EndColumn < 0):
		return "", fmt.Errorf("%w: a range needs lines, and both columns", ErrBadFeedback)
	case f.StartColumn != nil && f.StartLine == f.EndLine && *f.EndColumn <= *f.StartColumn:
		return "", fmt.Errorf("%w: a range ends after it starts", ErrBadFeedback)
	}
	f.ID = store.NewID()
	return f.ID, c.record(&store.Event{Type: store.FeedbackAdded, Feedback: &f})
}

// RemoveFeedback takes back a comment that hasn't been read.
func (c *Core) RemoveFeedback(id string) error {
	c.mu.Lock()
	i := slices.IndexFunc(c.feedback, func(f *FeedbackItem) bool { return f.ID == id })
	read := i >= 0 && c.feedback[i].Read != nil
	c.mu.Unlock()
	switch {
	case i < 0:
		return ErrNotFound
	case read:
		return fmt.Errorf("%w: it's been read", ErrBadFeedback)
	}
	return c.record(&store.Event{Type: store.FeedbackRemoved, Feedback: &store.Feedback{ID: id}})
}

// MarkFeedbackRead marks comments read, saying by whom.
func (c *Core) MarkFeedbackRead(ids []string, by string) error {
	c.mu.Lock()
	var unread []string
	for _, f := range c.feedback {
		if slices.Contains(ids, f.ID) && f.Read == nil {
			unread = append(unread, f.ID)
		}
	}
	c.mu.Unlock()
	if len(unread) == 0 {
		return nil
	}
	return c.record(&store.Event{Type: store.FeedbackRead, FeedbackIDs: unread, By: by})
}

// Feedback is every comment, oldest first.
func (c *Core) Feedback() []FeedbackItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []FeedbackItem{}
	for _, f := range c.feedback {
		out = append(out, *f)
	}
	return out
}
