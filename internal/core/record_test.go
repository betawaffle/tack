package core

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/betawaffle/tack/internal/openrouter"
	"github.com/betawaffle/tack/internal/store"
)

// The fixture is a record written by the first format, kept exactly as it was.
// Raw data is immutable, so every later build has to read it, and has to send
// the bytes it would have sent then, or live conversations would miss the cache.
const oldRecordConversation = "957a7c942ae967f3"

func (f *fake) openOldRecord() *Core {
	f.t.Helper()
	if err := os.CopyFS(f.dataDir, os.DirFS("testdata/record-v1")); err != nil {
		f.t.Fatal(err)
	}
	return f.open()
}

func TestOldRecordRendersTheBytesItSent(t *testing.T) {
	f := newFake(t)
	c := f.openOldRecord()
	var last []byte
	checked := 0
	for _, ev := range f.events() {
		if ev.Type != store.CallStarted {
			continue
		}
		messages, err := c.renderMessages(ev.Context)
		if err != nil {
			t.Fatal(err)
		}
		var routing openrouter.Routing
		routing.Order = []string{"morph/fp8"}
		body, err := openrouter.Render(openrouter.Request{Model: ev.Model, Routing: routing, Messages: messages, SessionID: ev.Conversation})
		if err != nil {
			t.Fatal(err)
		}
		sent, err := f.store.Get(ev.Request)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, sent) {
			t.Errorf("rendered differently:\n got %s\nsent %s", body, sent)
		}
		last = sent
		checked++
	}
	if checked != 4 {
		t.Errorf("checked %d calls", checked)
	}
	want, err := os.ReadFile("testdata/record-v1-last-request.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(last, want) {
		t.Errorf("last request:\n got %s\nwant %s", last, want)
	}
}

func TestOldRecordContinuesWithExpectedBytes(t *testing.T) {
	f := newFake(t)
	c := f.openOldRecord()
	conv, err := c.Conversation(oldRecordConversation)
	if err != nil {
		t.Fatal(err)
	}
	if conv.CanReply || conv.Busy || len(conv.Items) != 9 {
		t.Fatalf("view = %+v", conv)
	}
	updates, err := c.Send(oldRecordConversation, "Two")
	mustFinish(t, updates, err)

	last, err := os.ReadFile("testdata/record-v1-last-request.json")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(string(last), `{"role":"user","content":"One more"}]`,
		`{"role":"user","content":"One more"},{"role":"assistant","content":"Hello there"},{"role":"user","content":"Two"}]`, 1)
	if got := string(f.bodies[0]); got != want {
		t.Errorf("request:\n got %s\nwant %s", got, want)
	}
}

func TestAnUnknownEventTypeRefusesToStart(t *testing.T) {
	f := newFake(t)
	c := f.open()
	if _, err := c.NewConversation(""); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Append(&store.Event{Type: "from_the_future"}); err != nil {
		t.Fatal(err)
	}
	events := len(f.events())
	_, err := New(Config{Store: f.store})
	if err == nil || !strings.Contains(err.Error(), "from_the_future") {
		t.Fatalf("New = %v", err)
	}
	if got := len(f.events()); got != events {
		t.Errorf("reading the record wrote %d events", got-events)
	}
}

func TestEveryEventTypeThisBuildWritesIsKnown(t *testing.T) {
	for _, eventType := range []string{
		store.TackStarted, store.ConversationAdded, store.UserMessage, store.SystemMessage,
		store.EditProposed, store.EditApplied, store.EditDiscarded, store.CallStarted, store.CallFinished,
		store.DecisionRequested, store.DecisionMade, store.ToolStarted, store.ToolFinished,
		store.ConfigLoaded, store.SettingsProposed, store.SettingsApplied, store.SettingsDiscarded,
	} {
		if !store.Known(eventType) {
			t.Errorf("%s isn't known", eventType)
		}
	}
}
