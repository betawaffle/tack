package core

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/betawaffle/tack/internal/store"
)

func TestManualCallIsSentExactlyAndRecorded(t *testing.T) {
	f := newFake(t)
	c := f.open()
	body := []byte(`{"model":"test/model","messages":[{"role":"user","content":"Hi"},{"role":"assistant","content":"The"}],"provider":{"order":["morph/fp8"]},"stream":true,"temperature":0}`)
	id, updates, err := c.SendManual(body, "")
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for u := range updates {
		if u.Kind == "content" {
			text += u.Text
		}
		if u.Kind == "error" {
			t.Errorf("error update: %s", u.Text)
		}
	}
	if text != "Hello there" {
		t.Errorf("streamed %q", text)
	}
	if len(f.bodies) != 1 || string(f.bodies[0]) != string(body) {
		t.Fatalf("sent %q", f.bodies)
	}
	if f.auth[0] != "Bearer "+testKey {
		t.Error("the key wasn't sent")
	}

	check := func(c *Core) {
		t.Helper()
		list := c.ManualCalls()
		if len(list) != 1 || list[0].ID != id || list[0].Host != "Morph" || list[0].Status != 200 || list[0].FinishReason != "stop" || list[0].CostUSD != 0.00001 || list[0].Running || list[0].Interrupted {
			t.Fatalf("list = %+v", list)
		}
		d, err := c.ManualCall(id)
		if err != nil {
			t.Fatal(err)
		}
		if d.Request != string(body) || d.Response != replyStream || d.Text != "Hello there" || d.Reasoning != "Hmm." || d.GenerationID != "gen-1" {
			t.Errorf("detail = %+v", d)
		}
		got, err := c.RecordedRequest(id)
		if err != nil || string(got) != string(body) {
			t.Errorf("recorded request %q, %v", got, err)
		}
	}
	check(c)
	check(f.open()) // the same after a restart, from the record alone

	var types []string
	for _, ev := range f.events() {
		types = append(types, ev.Type)
	}
	if strings.Join(types, " ") != store.ManualCallStarted+" "+store.ManualCallFinished {
		t.Errorf("events %v", types)
	}
}

func TestManualCallRefusesWhatIsntARequest(t *testing.T) {
	f := newFake(t)
	c := f.open()
	for _, body := range []string{``, `[]`, `{"messages":[]}`, `{"model":"m"`, `"x"`} {
		if _, _, err := c.SendManual([]byte(body), ""); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%q: %v", body, err)
		}
	}
	if len(f.bodies) != 0 || len(f.events()) != 0 {
		t.Error("a refused request was sent or recorded")
	}
}

func TestManualCallCanBeCancelledAndHoldsUpARestart(t *testing.T) {
	f := newFake(t)
	release := make(chan struct{})
	f.handler = func(w http.ResponseWriter) {
		io.WriteString(w, ": OPENROUTER PROCESSING\n\n")
		w.(http.Flusher).Flush()
		<-release
	}
	defer close(release)
	c := f.open()
	id, updates, err := c.SendManual([]byte(`{"model":"test/model","messages":[{"role":"user","content":"Hi"}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RequestRestart(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Drained():
		t.Fatal("drained while a manual call was running")
	case <-time.After(50 * time.Millisecond):
	}
	if _, _, err := c.SendManual([]byte(`{"model":"m"}`), ""); !errors.Is(err, ErrRestarting) {
		t.Errorf("a new one during a restart: %v", err)
	}
	if list := c.ManualCalls(); len(list) != 1 || !list[0].Running {
		t.Fatalf("list = %+v", list)
	}
	if err := c.CancelManual(id); err != nil {
		t.Fatal(err)
	}
	finish(t, updates)
	select {
	case <-c.Drained():
	case <-time.After(5 * time.Second):
		t.Fatal("never drained")
	}
	d, err := c.ManualCall(id)
	if err != nil || d.Running || !strings.Contains(d.Error, "cancelled") {
		t.Errorf("detail %+v, %v", d.ManualSummary, err)
	}
	if err := c.CancelManual(id); !errors.Is(err, ErrNothingRunning) {
		t.Errorf("cancelling again: %v", err)
	}
}

func TestInterruptedManualCall(t *testing.T) {
	f := newFake(t)
	c := f.open()
	if err := f.store.Append(&store.Event{Type: store.ManualCallStarted, Call: "x", Model: "m", Request: mustPut(t, f.store, `{"model":"m"}`)}); err != nil {
		t.Fatal(err)
	}
	c = f.open()
	if list := c.ManualCalls(); len(list) != 1 || !list[0].Interrupted || list[0].Running {
		t.Errorf("list = %+v", list)
	}
}

func TestRecordedCallsIncludeConversations(t *testing.T) {
	f := newFake(t)
	c := f.open()
	conv, err := c.NewConversation("")
	if err != nil {
		t.Fatal(err)
	}
	updates, err := c.Send(conv, "Hi there")
	if err != nil {
		t.Fatal(err)
	}
	finish(t, updates)
	calls := c.RecordedCalls(10)
	if len(calls) != 1 || calls[0].Conversation != conv || calls[0].Preview != "Hi there" || calls[0].Host != "morph/fp8" {
		t.Fatalf("calls = %+v", calls)
	}
	body, err := c.RecordedRequest(calls[0].Call)
	if err != nil || string(body) != string(f.bodies[0]) {
		t.Errorf("request %q, %v", body, err)
	}
	if _, err := c.RecordedRequest("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
}
