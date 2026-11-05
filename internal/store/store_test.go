package store

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/betawaffle/tack/internal/secret"
)

const testKey = "sk-or-test-not-a-real-key"

func open(t *testing.T, fired *string) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, secret.NewTestTripwire(func(where string) { *fired = where }, secret.NewKey(testKey)))
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestBlobsKeepExactBytes(t *testing.T) {
	var fired string
	s, _ := open(t, &fired)
	// Includes invalid UTF-8: blobs are bytes, not text.
	want := []byte("hello\xff\x00 world\r\n")
	ref, err := s.Put(want)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Put(want)
	if err != nil || again != ref {
		t.Fatalf("second put = %q, %v", again, err)
	}
	got, err := s.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q", got)
	}
}

func TestKnownHash(t *testing.T) {
	var fired string
	s, _ := open(t, &fired)
	ref, err := s.Put([]byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	const want = "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if ref != want {
		t.Errorf("ref = %s", ref)
	}
}

func TestCorruptBlobIsAnError(t *testing.T) {
	var fired string
	s, dir := open(t, &fired)
	ref, err := s.Put([]byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.TrimPrefix(string(ref), "sha256:")
	if err := os.WriteFile(filepath.Join(dir, "blobs", sum[:2], sum[2:]), []byte("abd"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ref); err == nil {
		t.Error("expected an error")
	}
}

func TestBadRefs(t *testing.T) {
	var fired string
	s, _ := open(t, &fired)
	for _, ref := range []Ref{"", "abc", "sha256:abc", "sha256:../../../../etc/passwd", Ref("sha256:" + strings.Repeat("zz", 32))} {
		if _, err := s.Get(ref); err == nil {
			t.Errorf("Get(%q) succeeded", ref)
		}
	}
}

func TestPartialStaysUntilCommitted(t *testing.T) {
	var fired string
	s, dir := open(t, &fired)
	p, err := s.NewPartial("call-1")
	if err != nil {
		t.Fatal(err)
	}
	p.Write([]byte("data: partly"))
	if data, err := os.ReadFile(filepath.Join(dir, "partial", "call-1")); err != nil || string(data) != "data: partly" {
		t.Fatalf("partial = %q, %v", data, err)
	}
	ref, err := p.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "partial", "call-1")); !os.IsNotExist(err) {
		t.Errorf("partial file still there: %v", err)
	}
	if got, err := s.Get(ref); err != nil || string(got) != "data: partly" {
		t.Errorf("blob = %q, %v", got, err)
	}
}

func TestEventsReplayInOrder(t *testing.T) {
	var fired string
	s, _ := open(t, &fired)
	for _, id := range []string{"a", "b", "c"} {
		if err := s.Append(&Event{Type: UserMessage, Conversation: id}); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	err := s.Replay(func(ev *Event) error {
		got = append(got, ev.Conversation)
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "") != "abc" {
		t.Errorf("got %v", got)
	}
}

func TestTornLineIsSkippedAndFinished(t *testing.T) {
	var fired string
	s, dir := open(t, &fired)
	if err := s.Append(&Event{Type: UserMessage, Conversation: "a"}); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "events", "*.jsonl"))
	f, _ := os.OpenFile(matches[0], os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"type":"user_mess`)
	f.Close()

	s2, err := Open(dir, secret.NewTestTripwire(func(string) {}, secret.NewKey(testKey)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Append(&Event{Type: UserMessage, Conversation: "b"}); err != nil {
		t.Fatal(err)
	}
	var got []string
	var torn int
	err = s2.Replay(func(ev *Event) error {
		got = append(got, ev.Conversation)
		return nil
	}, func(string, int) { torn++ })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "") != "ab" || torn != 1 {
		t.Errorf("got %v, torn %d", got, torn)
	}
}

func TestDamagedLineIsAnError(t *testing.T) {
	var fired string
	s, dir := open(t, &fired)
	if err := s.Append(&Event{Type: UserMessage}); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "events", "*.jsonl"))
	f, _ := os.OpenFile(matches[0], os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString("not json at all\n")
	f.Close()
	err := s.Replay(func(*Event) error { return nil }, func(string, int) {})
	if err == nil {
		t.Error("expected an error")
	}
}

func TestTripwire(t *testing.T) {
	var fired string
	s, _ := open(t, &fired)

	if _, err := s.Put([]byte("key is " + testKey)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fired, "blob") {
		t.Errorf("blob: fired = %q", fired)
	}
	fired = ""

	if err := s.Append(&Event{Type: UserMessage, Error: "oops " + testKey}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fired, "event") {
		t.Errorf("event: fired = %q", fired)
	}
}

func TestEventOmitsUnusedFields(t *testing.T) {
	line, err := json.Marshal(&Event{Type: TackStarted, Version: "v"})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"conversation", "context", "routing", "usage", "status"} {
		if strings.Contains(string(line), `"`+field+`"`) {
			t.Errorf("%s present in %s", field, line)
		}
	}
}

func TestTornWrites(t *testing.T) {
	event := `{"time":"2026-11-05T00:00:00Z","type":"x","usage":{"a":{"b":1}},"error":"say \"hi\" é"}`
	if !isValidEvent(event) {
		t.Fatal("test event is invalid")
	}
	// Wherever an append is cut off, the fragment reads as torn, not damaged.
	for i := 1; i < len(event); i++ {
		if !isTornWrite([]byte(event[:i])) {
			t.Errorf("not recognised as torn: %s", event[:i])
		}
	}
	for _, damaged := range []string{"not json", "[1,", `{"a":1}}x`, `{bad}`} {
		if isTornWrite([]byte(damaged)) {
			t.Errorf("recognised as torn: %s", damaged)
		}
	}
}

func isValidEvent(line string) bool {
	var ev Event
	return json.Unmarshal([]byte(line), &ev) == nil
}

func TestTripwireCatchesASecretAcrossWrites(t *testing.T) {
	var fired string
	s, _ := open(t, &fired)
	p, err := s.NewPartial("call-1")
	if err != nil {
		t.Fatal(err)
	}
	half := len(testKey) / 2
	p.Write([]byte("data: " + testKey[:half]))
	if fired != "" {
		t.Fatalf("fired early in %s", fired)
	}
	p.Write([]byte(testKey[half:] + " more"))
	if fired == "" {
		t.Error("didn't fire")
	}
}

func TestTripwireFiresBeforeTheDisk(t *testing.T) {
	dir := t.TempDir()
	stopped := false
	s, err := Open(dir, secret.NewTestTripwire(func(string) {
		stopped = true
		entries, _ := os.ReadDir(filepath.Join(dir, "partial"))
		for _, e := range entries {
			data, _ := os.ReadFile(filepath.Join(dir, "partial", e.Name()))
			if strings.Contains(string(data), testKey) {
				t.Error("secret reached the disk before the tripwire fired")
			}
		}
	}, secret.NewKey(testKey)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put([]byte("key is " + testKey)); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Error("didn't fire")
	}
}
