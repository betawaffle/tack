package core

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/betawaffle/tack/internal/openrouter"
	"github.com/betawaffle/tack/internal/secret"
	"github.com/betawaffle/tack/internal/store"
)

const testKey = "sk-or-test-not-a-real-key"

const replyStream = ": OPENROUTER PROCESSING\n\n" +
	`data: {"id":"gen-1","provider":"Morph","choices":[{"delta":{"reasoning":"Hmm."}}]}` + "\n\n" +
	`data: {"id":"gen-1","provider":"Morph","choices":[{"delta":{"content":"Hello"}}]}` + "\n\n" +
	`data: {"id":"gen-1","provider":"Morph","choices":[{"delta":{"content":" there"},"finish_reason":"stop"}]}` + "\n\n" +
	`data: {"id":"gen-1","provider":"Morph","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3,"cost":0.00001,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":1}}}` + "\n\n" +
	"data: [DONE]\n\n"

type fake struct {
	t        *testing.T
	mu       sync.Mutex
	bodies   [][]byte
	auth     []string
	handler  func(w http.ResponseWriter)
	server   *httptest.Server
	dataDir  string
	tripwire *secret.Tripwire
	store    *store.Store
	tools    []Tool
	settings store.Settings
	model    string
	prices   []openrouter.Endpoint
	// generation answers what a call cost, if it's set.
	generation func(id string) (float64, bool)
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, dataDir: t.TempDir()}
	f.handler = func(w http.ResponseWriter) { io.WriteString(w, replyStream) }
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		f.handler(w)
	}))
	t.Cleanup(f.server.Close)
	t.Cleanup(func() {
		if f.store != nil {
			f.store.Close()
		}
	})
	f.tripwire = secret.NewTestTripwire(func(where string) {
		t.Errorf("tripwire fired in %s", where)
	}, secret.NewKey(testKey))
	return f
}

func (f *fake) open() *Core {
	f.t.Helper()
	if f.store != nil {
		f.store.Close() // as a restart would
	}
	st, err := store.Open(f.dataDir, f.tripwire)
	if err != nil {
		f.t.Fatal(err)
	}
	f.store = st
	c, err := New(Config{
		Store:  st,
		Client: openrouter.NewClient(f.server.URL, secret.NewKey(testKey)),
		Model:  cmp.Or(f.model, "test/model"),
		Generation: func(_ context.Context, id string) (float64, bool, []byte, error) {
			if f.generation == nil {
				return 0, false, nil, errors.New("no generations")
			}
			cost, ok := f.generation(id)
			return cost, ok, []byte(`{"data":{}}`), nil
		},
		Prices: func(context.Context, string) ([]openrouter.Endpoint, error) {
			if f.prices == nil {
				return nil, errors.New("no prices")
			}
			return f.prices, nil
		},
		Routing:  openrouter.Routing{Order: []string{"morph/fp8"}},
		Tools:    f.tools,
		Settings: f.settings,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := c.Recover(); err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fake) events() []*store.Event {
	f.t.Helper()
	var out []*store.Event
	if err := f.store.Replay(func(ev *store.Event) error {
		out = append(out, ev)
		return nil
	}, nil); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// finish reads updates to the end and returns the last one.
func finish(t *testing.T, updates <-chan Update) Update {
	t.Helper()
	var last Update
	for u := range updates {
		last = u
	}
	return last
}

func TestRecordsExactBytes(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, err := c.NewConversation("")
	if err != nil {
		t.Fatal(err)
	}
	updates, err := c.Send(id, "Hi")
	if err != nil {
		t.Fatal(err)
	}
	if last := finish(t, updates); last.Kind != "done" {
		t.Fatalf("last update = %+v", last)
	}

	var started, finished *store.Event
	for _, ev := range f.events() {
		switch ev.Type {
		case store.CallStarted:
			started = ev
		case store.CallFinished:
			finished = ev
		}
	}
	if started == nil || finished == nil {
		t.Fatal("missing call events")
	}
	request, err := f.store.Get(started.Request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(request, f.bodies[0]) {
		t.Errorf("recorded request differs from what was sent:\n%s\n%s", request, f.bodies[0])
	}
	response, err := f.store.Get(finished.Response)
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != replyStream {
		t.Errorf("recorded response differs from what arrived:\n%s", response)
	}
	if finished.Host != "Morph" || finished.GenerationID != "gen-1" || finished.FinishReason != "stop" {
		t.Errorf("finished event = %+v", finished)
	}
	text, err := f.store.Get(finished.Text)
	if err != nil || string(text) != "Hello there" {
		t.Errorf("text = %q, %v", text, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.dataDir, "partial")); len(entries) != 0 {
		t.Errorf("partial/ still has %d files", len(entries))
	}
}

func TestKeyIsSentButNeverRecorded(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "Hi")
	if err != nil {
		t.Fatal(err)
	}
	finish(t, updates)

	if f.auth[0] != "Bearer "+testKey {
		t.Errorf("Authorization header not sent as expected")
	}
	err = filepath.WalkDir(f.dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(testKey)) {
			t.Errorf("key found in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestContextGrowsAndPrefixIsStable(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("")
	for _, text := range []string{"one", "two"} {
		updates, err := c.Send(id, text)
		if err != nil {
			t.Fatal(err)
		}
		if last := finish(t, updates); last.Kind != "done" {
			t.Fatalf("last update = %+v", last)
		}
	}
	type body struct {
		Messages []openrouter.Message `json:"messages"`
	}
	var second body
	if err := json.Unmarshal(f.bodies[1], &second); err != nil {
		t.Fatal(err)
	}
	want := []openrouter.Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "Hello there"},
		{Role: "user", Content: "two"},
	}
	if len(second.Messages) != len(want) {
		t.Fatalf("messages = %+v", second.Messages)
	}
	for i := range want {
		if !reflect.DeepEqual(second.Messages[i], want[i]) {
			t.Errorf("message %d = %+v, want %+v", i, second.Messages[i], want[i])
		}
	}
	// The cacheable part: the first request's messages must appear byte for
	// byte at the start of the second's.
	first := string(f.bodies[0])
	prefix := first[:strings.LastIndex(first, "}]")+1]
	if !strings.HasPrefix(string(f.bodies[1]), prefix) {
		t.Errorf("second request doesn't start with the first's prefix:\n%s\n%s", f.bodies[0], f.bodies[1])
	}
}

func TestStateRebuildsFromRecord(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "Hi")
	if err != nil {
		t.Fatal(err)
	}
	finish(t, updates)
	before, err := c.Conversation(id)
	if err != nil {
		t.Fatal(err)
	}

	after, err := f.open().Conversation(id)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(before, json.Deterministic(true))
	b, _ := json.Marshal(after, json.Deterministic(true))
	if !bytes.Equal(a, b) {
		t.Errorf("state differs after rebuild:\n%s\n%s", a, b)
	}
	if len(after.Items) != 2 || after.Items[1].Text != "Hello there" || after.Items[1].Reasoning != "Hmm." {
		t.Errorf("items = %+v", after.Items)
	}
	if s := after.Items[1].Stats; s == nil || s.CachedTokens != 4 || s.Host != "Morph" {
		t.Errorf("stats = %+v", s)
	}
}

func TestCrashDuringCall(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("")

	// Write what a crash mid-call would leave: a started event and no finish,
	// then a torn line.
	if err := f.store.Append(&store.Event{Type: store.UserMessage, Conversation: id, Parts: []store.Part{{Block: mustPut(t, f.store, "Hi")}}}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Append(&store.Event{Type: store.CallStarted, Conversation: id, Call: "dead"}); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(f.dataDir, "events", "*.jsonl"))
	file, err := os.OpenFile(matches[0], os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString(`{"time":"2026-11-05T00:00:00Z","type":"model_call_fin`)
	file.Close()

	c = f.open()
	conv, err := c.Conversation(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Items) != 2 || conv.Items[1].Role != "notice" || !strings.Contains(conv.Items[1].Text, "interrupted") {
		t.Fatalf("items = %+v", conv.Items)
	}

	// Carrying on works, the earlier message is kept, and the new event doesn't
	// land on the end of the torn line.
	updates, err := c.Send(id, "Still there?")
	if err != nil {
		t.Fatal(err)
	}
	if last := finish(t, updates); last.Kind != "done" {
		t.Fatalf("last update = %+v", last)
	}
	conv, _ = f.open().Conversation(id)
	if got := conv.Items[len(conv.Items)-1].Text; got != "Hello there" {
		t.Errorf("last item = %q", got)
	}
}

func mustPut(t *testing.T, s *store.Store, text string) store.Ref {
	t.Helper()
	ref, err := s.Put([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestProviderErrorsAreRecorded(t *testing.T) {
	cases := []struct {
		name    string
		handler func(w http.ResponseWriter)
		want    string
	}{
		{"http error", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"slow down"}}`)
		}, "slow down"},
		{"mid-stream error", func(w http.ResponseWriter) {
			io.WriteString(w, `data: {"choices":[{"delta":{"content":"par"}}]}`+"\n\n")
			io.WriteString(w, `data: {"error":{"code":502,"message":"upstream died"},"choices":[{"delta":{"content":""},"finish_reason":"error"}]}`+"\n\n")
		}, "upstream died"},
		{"truncated stream", func(w http.ResponseWriter) {
			io.WriteString(w, `data: {"choices":[{"delta":{"content":"par"}}]}`+"\n\n")
		}, "without a finish reason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.handler = tc.handler
			c := f.open()
			id, _ := c.NewConversation("")
			updates, err := c.Send(id, "Hi")
			if err != nil {
				t.Fatal(err)
			}
			last := finish(t, updates)
			if last.Kind != "error" || !strings.Contains(last.Text, tc.want) {
				t.Errorf("last update = %+v", last)
			}
			conv, _ := c.Conversation(id)
			if n := len(conv.Items); n != 2 || conv.Items[1].Role != "notice" {
				t.Errorf("items = %+v", conv.Items)
			}

			// A failed reply stays out of the next context.
			f.handler = func(w http.ResponseWriter) { io.WriteString(w, replyStream) }
			updates, err = c.Send(id, "Again")
			if err != nil {
				t.Fatal(err)
			}
			finish(t, updates)
			if strings.Contains(string(f.bodies[1]), `"assistant"`) {
				t.Errorf("failed reply was sent back: %s", f.bodies[1])
			}
		})
	}
}

func TestNoTextIsNotAReply(t *testing.T) {
	f := newFake(t)
	f.handler = func(w http.ResponseWriter) {
		io.WriteString(w, `data: {"choices":[{"delta":{"reasoning":"long thought"},"finish_reason":"length"}]}`+"\n\n")
	}
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "Hi")
	if err != nil {
		t.Fatal(err)
	}
	if last := finish(t, updates); last.Kind != "error" {
		t.Errorf("last update = %+v", last)
	}
}

func TestOneCallAtATime(t *testing.T) {
	f := newFake(t)
	release := make(chan struct{})
	f.handler = func(w http.ResponseWriter) {
		<-release
		io.WriteString(w, replyStream)
	}
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "Hi")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(id, "Hi again"); err != ErrBusy {
		t.Errorf("second send: %v", err)
	}
	close(release)
	finish(t, updates)
	if _, err := c.Send("nope", "Hi"); err != ErrNotFound {
		t.Errorf("unknown conversation: %v", err)
	}
}

// messagesIn returns the messages of the nth request the provider received.
func (f *fake) messagesIn(n int) []openrouter.Message {
	f.t.Helper()
	var body struct {
		Messages []openrouter.Message `json:"messages"`
	}
	if err := json.Unmarshal(f.bodies[n], &body); err != nil {
		f.t.Fatal(err)
	}
	return body.Messages
}

func (f *fake) wantMessages(n int, want ...openrouter.Message) {
	f.t.Helper()
	got := f.messagesIn(n)
	if len(got) != len(want) {
		f.t.Fatalf("request %d messages = %+v, want %+v", n, got, want)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			f.t.Errorf("request %d message %d = %+v, want %+v", n, i, got[i], want[i])
		}
	}
}

func mustFinish(t *testing.T, updates <-chan Update, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if last := finish(t, updates); last.Kind != "done" {
		t.Fatalf("last update = %+v", last)
	}
}

func sys(text string) openrouter.Message  { return openrouter.Message{Role: "system", Content: text} }
func user(text string) openrouter.Message { return openrouter.Message{Role: "user", Content: text} }
func bot(text string) openrouter.Message {
	return openrouter.Message{Role: "assistant", Content: text}
}

func TestSystemPromptComesFirst(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, err := c.NewConversation("Be brief.")
	if err != nil {
		t.Fatal(err)
	}
	updates, err := c.Send(id, "Hi")
	mustFinish(t, updates, err)
	f.wantMessages(0, sys("Be brief."), user("Hi"))
}

func TestEmptySystemPromptSendsNone(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("  \n")
	updates, err := c.Send(id, "Hi")
	mustFinish(t, updates, err)
	f.wantMessages(0, user("Hi"))
}

func TestSystemMessageWaitsForNextCall(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "one")
	mustFinish(t, updates, err)

	if _, err := c.AddSystemMessage(id, "New rule."); err != nil {
		t.Fatal(err)
	}
	if len(f.bodies) != 1 {
		t.Fatalf("adding a system message made a call")
	}
	conv, _ := c.Conversation(id)
	last := conv.Items[len(conv.Items)-1]
	if last.Role != "system" || !last.Queued {
		t.Errorf("last item = %+v", last)
	}

	updates, err = c.Send(id, "two")
	mustFinish(t, updates, err)
	f.wantMessages(1, user("one"), bot("Hello there"), sys("New rule."), user("two"))
	conv, _ = c.Conversation(id)
	for _, it := range conv.Items {
		if it.Role == "system" && it.Queued {
			t.Errorf("still queued after being sent: %+v", it)
		}
	}
}

func TestReplyToSystemMessageSpeaksOutOfTurn(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "one")
	mustFinish(t, updates, err)

	if _, err := c.Reply(id); err != ErrNothingToSend {
		t.Errorf("reply with nothing new: %v", err)
	}
	if _, err := c.AddSystemMessage(id, "Speak up."); err != nil {
		t.Fatal(err)
	}
	updates, err = c.Reply(id)
	mustFinish(t, updates, err)
	f.wantMessages(1, user("one"), bot("Hello there"), sys("Speak up."))
}

func TestEditsAreHeldUntilApplied(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("Old prompt.")
	updates, err := c.Send(id, "one")
	mustFinish(t, updates, err)

	conv, _ := c.Conversation(id)
	message := conv.Items[0].Message

	editID, err := c.ProposeEdit(id, message, "New prompt.")
	if err != nil {
		t.Fatal(err)
	}
	conv, _ = c.Conversation(id)
	if it := conv.Items[0]; it.Text != "Old prompt." || it.Pending == nil || it.Pending.Text != "New prompt." {
		t.Fatalf("held edit shown as %+v", it)
	}

	// A held edit isn't used.
	updates, err = c.Send(id, "two")
	mustFinish(t, updates, err)
	f.wantMessages(1, sys("Old prompt."), user("one"), bot("Hello there"), user("two"))

	if err := c.ApplyEdit(id, editID); err != nil {
		t.Fatal(err)
	}
	conv, _ = c.Conversation(id)
	if it := conv.Items[0]; it.Text != "New prompt." || it.Pending != nil || !it.Edited || !it.Queued {
		t.Errorf("applied edit shown as %+v", it)
	}
	if err := c.ApplyEdit(id, editID); err != ErrNoEdit {
		t.Errorf("applying twice: %v", err)
	}

	updates, err = c.Send(id, "three")
	mustFinish(t, updates, err)
	f.wantMessages(2, sys("New prompt."), user("one"), bot("Hello there"), user("two"), bot("Hello there"), user("three"))

	// All of it survives a rebuild.
	rebuilt, err := f.open().Conversation(id)
	if err != nil {
		t.Fatal(err)
	}
	if it := rebuilt.Items[0]; it.Text != "New prompt." || !it.Edited || it.Queued {
		t.Errorf("after rebuild: %+v", it)
	}
}

func TestDiscardAndSupersedeEdits(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("Prompt.")
	conv, _ := c.Conversation(id)
	message := conv.Items[0].Message

	first, _ := c.ProposeEdit(id, message, "First.")
	second, _ := c.ProposeEdit(id, message, "Second.")
	if err := c.ApplyEdit(id, first); err != ErrNoEdit {
		t.Errorf("applying a replaced edit: %v", err)
	}
	if err := c.DiscardEdit(id, second); err != nil {
		t.Fatal(err)
	}
	conv, _ = c.Conversation(id)
	if it := conv.Items[0]; it.Text != "Prompt." || it.Pending != nil || it.Edited {
		t.Errorf("after discard: %+v", it)
	}
	if _, err := c.ProposeEdit(id, "nope", "x"); err != ErrNoMessage {
		t.Errorf("unknown message: %v", err)
	}
}

func TestEditCanRemoveAMessage(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("Prompt.")
	conv, _ := c.Conversation(id)
	editID, err := c.ProposeEdit(id, conv.Items[0].Message, "")
	if err != nil {
		t.Fatal(err)
	}
	conv, _ = c.Conversation(id)
	if it := conv.Items[0]; it.Pending == nil || it.Pending.Text != "" {
		t.Errorf("proposed removal shown as %+v", it)
	}
	if err := c.ApplyEdit(id, editID); err != nil {
		t.Fatal(err)
	}
	conv, _ = c.Conversation(id)
	if !conv.Items[0].Removed {
		t.Errorf("removed message shown as %+v", conv.Items[0])
	}
	updates, err := c.Send(id, "Hi")
	mustFinish(t, updates, err)
	f.wantMessages(0, user("Hi"))
}

func TestRetrySendsTheSameBytes(t *testing.T) {
	f := newFake(t)
	f.handler = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "nope")
	}
	c := f.open()
	id, _ := c.NewConversation("Prompt.")
	updates, err := c.Send(id, "Hi")
	if err != nil {
		t.Fatal(err)
	}
	if last := finish(t, updates); last.Kind != "error" {
		t.Fatalf("last update = %+v", last)
	}
	conv, _ := c.Conversation(id)
	if !conv.CanReply {
		t.Error("can't retry a failed call")
	}

	f.handler = func(w http.ResponseWriter) { io.WriteString(w, replyStream) }
	updates, err = c.Reply(id)
	mustFinish(t, updates, err)
	if !bytes.Equal(f.bodies[0], f.bodies[1]) {
		t.Errorf("retry differs:\n%s\n%s", f.bodies[0], f.bodies[1])
	}
	conv, _ = c.Conversation(id)
	if conv.CanReply {
		t.Error("can retry after a good reply")
	}
	// The failure stays visible and the message appears once.
	var users, notices int
	for _, it := range conv.Items {
		switch it.Role {
		case "user":
			users++
		case "notice":
			notices++
		}
	}
	if users != 1 || notices != 1 {
		t.Errorf("items = %+v", conv.Items)
	}
}

func TestRetryAfterCrash(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("")
	f.store.Append(&store.Event{Type: store.UserMessage, Conversation: id, Parts: []store.Part{{Block: mustPut(t, f.store, "Hi")}}})
	f.store.Append(&store.Event{Type: store.CallStarted, Conversation: id, Call: "dead"})

	c = f.open()
	conv, _ := c.Conversation(id)
	if !conv.CanReply {
		t.Fatal("can't retry an interrupted call")
	}
	updates, err := c.Reply(id)
	mustFinish(t, updates, err)
	f.wantMessages(0, user("Hi"))
}

func TestSystemMessageDuringCallDoesNotHideTheRunningCall(t *testing.T) {
	f := newFake(t)
	release := make(chan struct{})
	f.handler = func(w http.ResponseWriter) {
		<-release
		io.WriteString(w, replyStream)
	}
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "Hi")
	if err != nil {
		t.Fatal(err)
	}
	for {
		f.mu.Lock()
		n := len(f.bodies)
		f.mu.Unlock()
		if n == 1 {
			break
		}
	}
	if _, err := c.AddSystemMessage(id, "Later."); err != nil {
		t.Fatal(err)
	}
	conv, _ := c.Conversation(id)
	for _, it := range conv.Items {
		if it.Role == "notice" {
			t.Errorf("running call shown as %+v", it)
		}
	}
	close(release)
	mustFinish(t, updates, nil)
}
