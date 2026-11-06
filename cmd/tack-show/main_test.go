package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/store"
)

// The first-format record in core's fixtures is a real conversation to read.
func openFixture(t *testing.T) (*store.Store, *core.Core) {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../internal/core/testdata/record-v1")); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := core.New(core.Config{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	return st, c
}

func output(t *testing.T, st *store.Store, c *core.Core, query string, sent, events bool) string {
	t.Helper()
	var b bytes.Buffer
	if err := run(&b, st, c, query, 1500, sent, events); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestListAndFind(t *testing.T) {
	st, c := openFixture(t)
	if got := output(t, st, c, "", false, false); !strings.Contains(got, "957a7c942ae967f3") || !strings.Contains(got, "Hello") {
		t.Errorf("list = %s", got)
	}
	for _, query := range []string{"last", "957a7c", "hello", "HELLO"} {
		got := output(t, st, c, query, false, false)
		if !strings.Contains(got, "Conversation 957a7c942ae967f3") || !strings.Contains(got, "Prefer very short words.") {
			t.Errorf("%s: %s", query, got)
		}
	}
	var b bytes.Buffer
	if err := run(&b, st, c, "no such thing", 1500, false, false); err == nil {
		t.Error("expected an error")
	}
}

func TestSentShowsRealRoles(t *testing.T) {
	st, c := openFixture(t)
	got := output(t, st, c, "last", true, false)
	for _, want := range []string{"=== call 3", "system: Prefer very short words.", "user: One more"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestEventsAreShown(t *testing.T) {
	st, c := openFixture(t)
	if got := output(t, st, c, "last", false, true); !strings.Contains(got, `"type":"model_call_started"`) {
		t.Errorf("events = %s", got)
	}
}

func TestShorten(t *testing.T) {
	long := strings.Repeat("a", 1000) + strings.Repeat("b", 1000)
	got := shorten(long, 300)
	if !strings.Contains(got, "1700 bytes cut") || !strings.HasPrefix(got, "aaa") || !strings.HasSuffix(got, "bbb") {
		t.Errorf("got %s", got)
	}
	if shorten(long, 0) != long || shorten("short", 300) != "short" {
		t.Error("shouldn't have shortened")
	}
}
