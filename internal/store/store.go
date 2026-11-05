// Package store is tack's raw record: append-only JSONL events plus
// content-addressed blobs. Everything else is derived from these and can be
// rebuilt.
//
//	events/2026-11-05.jsonl   one fact per line, by UTC day
//	blobs/ab/cdef...          exact bytes, named by SHA-256
//	partial/                  blobs still being written
//	lock                      held by the one process using this directory
//
// A file left in partial/ after a crash is the part of a response that had
// arrived. It's kept on purpose.
package store

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/betawaffle/tack/internal/secret"
)

// Ref names a blob by its hash: "sha256:<hex>". The hash always covers the
// uncompressed bytes.
type Ref string

// Part is a piece of a context entry. For now it's always a whole block.
type Part struct {
	Block Ref `json:"block"`
}

// Entry is one message in a context.
type Entry struct {
	Role  string `json:"role"`
	Parts []Part `json:"parts"`

	// An assistant message that made tool calls. Its reasoning has to be sent back
	// exactly, or the host's cache misses from that point.
	ToolCalls []ToolCall `json:"tool_calls,omitzero"`
	Reasoning Ref        `json:"reasoning,omitzero"`

	// A tool message: the call it answers.
	ToolCall string `json:"tool_call,omitzero"`
}

// ToolCall is one call a model asked for. Arguments are a block holding the exact
// text the model sent, since they can carry whole files. Invalid says why the call
// can't run, as judged when it arrived.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments Ref    `json:"arguments"`
	Invalid   string `json:"invalid,omitzero"`
}

// Choice is what was decided about one tool call. Parts are the result the model
// will see, and they're absent only when a call was allowed to run.
type Choice struct {
	ToolCall string `json:"tool_call"`
	Allow    bool   `json:"allow"`
	Parts    []Part `json:"parts,omitzero"`
}

// Decision kinds.
const (
	DecideCalls       = "tool_calls"  // may these run?
	DecideOutput      = "tool_output" // may the model see what they produced?
	DecideInterrupted = "interrupted" // tack stopped before these finished
)

// Event types.
const (
	TackStarted       = "tack_started"
	ConversationAdded = "conversation_created"
	UserMessage       = "user_message"
	SystemMessage     = "system_message"
	EditProposed      = "system_edit_proposed"
	EditApplied       = "system_edit_applied"
	EditDiscarded     = "system_edit_discarded"
	CallStarted       = "model_call_started"
	CallFinished      = "model_call_finished"
	DecisionRequested = "decision_requested"
	DecisionMade      = "decision_made"
	ToolStarted       = "tool_started"
	ToolFinished      = "tool_finished"
)

// Known reports whether this build understands an event type. A record with a type
// it doesn't know was written by a newer build, and reading it anyway could mean
// getting history wrong, such as not knowing a tool call was already decided.
func Known(eventType string) bool {
	switch eventType {
	case TackStarted, ConversationAdded, UserMessage, SystemMessage, EditProposed, EditApplied, EditDiscarded,
		CallStarted, CallFinished, DecisionRequested, DecisionMade, ToolStarted, ToolFinished:
		return true
	}
	return false
}

// Event is one recorded fact. Fields beyond Time and Type depend on the type.
type Event struct {
	Time time.Time `json:"time"`
	Type string    `json:"type"`

	// tack_started
	Version string `json:"version,omitzero"`
	PID     int    `json:"pid,omitzero"`

	// conversation_created and everything that happens in a conversation
	Conversation string `json:"conversation,omitzero"`

	// conversation_created: the exact tool definitions its calls send.
	Tools Ref `json:"tools,omitzero"`

	// user_message, system_message, system_edit_proposed. An edit with no parts
	// removes the message.
	Parts []Part `json:"parts,omitzero"`

	// system_message, system_edit_proposed: the message concerned.
	Message string `json:"message,omitzero"`

	// system_edit_*
	Edit string `json:"edit,omitzero"`

	// model_call_started
	Call           string              `json:"call,omitzero"`
	Context        []Entry             `json:"context,omitzero"`
	Model          string              `json:"model,omitzero"`
	Routing        jsontext.Value      `json:"routing,omitzero"`
	URL            string              `json:"url,omitzero"`
	Request        Ref                 `json:"request,omitzero"`
	RequestHeaders map[string][]string `json:"request_headers,omitzero"`

	// model_call_finished
	Response        Ref                 `json:"response,omitzero"`
	Status          int                 `json:"status,omitzero"`
	ResponseHeaders map[string][]string `json:"response_headers,omitzero"`
	Host            string              `json:"host,omitzero"`
	GenerationID    string              `json:"generation_id,omitzero"`
	FinishReason    string              `json:"finish_reason,omitzero"`
	Usage           jsontext.Value      `json:"usage,omitzero"`
	TTFTMillis      int64               `json:"ttft_ms,omitzero"`
	DurationMillis  int64               `json:"duration_ms,omitzero"`
	Text            Ref                 `json:"text,omitzero"`
	Reasoning       Ref                 `json:"reasoning,omitzero"`
	ParseErrors     int                 `json:"parse_errors,omitzero"`
	Error           string              `json:"error,omitzero"` // also tool_finished
	ToolCalls       []ToolCall          `json:"tool_calls,omitzero"`

	// decision_*
	Decision      string   `json:"decision,omitzero"`
	Kind          string   `json:"kind,omitzero"`
	DecisionCalls []string `json:"decision_calls,omitzero"`
	Choices       []Choice `json:"choices,omitzero"`
	By            string   `json:"by,omitzero"`

	// tool_*
	ToolCall string `json:"tool_call,omitzero"`
	Output   Ref    `json:"output,omitzero"`
}

// Store is the single writer for a data directory.
type Store struct {
	dir      string
	tripwire *secret.Tripwire
	lock     *os.File

	mu   sync.Mutex
	day  string
	file *os.File
}

// Open prepares a data directory and locks it. Each process keeps its own view
// of the record in memory, so two of them sharing a directory would each act on a
// history missing the other's events. The kernel drops the lock however the process
// ends, so a crash can't leave it stuck. The lock is only trusted on a local disk.
func Open(dir string, tripwire *secret.Tripwire) (*Store, error) {
	for _, sub := range []string{"events", "blobs", "partial"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another tack is already using %s", dir)
		}
		return nil, fmt.Errorf("locking %s: %w", dir, err)
	}
	return &Store{dir: dir, tripwire: tripwire, lock: lock}, nil
}

// Close releases the directory.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeEvents()
	return s.lock.Close()
}

func (s *Store) closeEvents() {
	if s.file != nil {
		s.file.Close()
		s.file, s.day = nil, ""
	}
}

// NewID returns a random identifier.
func NewID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func refFor(h hash.Hash) Ref {
	return Ref("sha256:" + hex.EncodeToString(h.Sum(nil)))
}

func (s *Store) blobPath(ref Ref) (string, error) {
	sum, ok := strings.CutPrefix(string(ref), "sha256:")
	if !ok || len(sum) != sha256.Size*2 {
		return "", fmt.Errorf("bad blob ref %q", ref)
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return "", fmt.Errorf("bad blob ref %q", ref)
	}
	return filepath.Join(s.dir, "blobs", sum[:2], sum[2:]), nil
}

// Put stores bytes as a blob. Storing the same bytes twice is harmless.
func (s *Store) Put(data []byte) (Ref, error) {
	p, err := s.NewPartial("put-" + NewID())
	if err != nil {
		return "", err
	}
	if _, err := p.Write(data); err != nil {
		p.Abort()
		return "", err
	}
	return p.Commit()
}

// Get returns a blob's bytes, after checking they still match its hash.
func (s *Store) Get(ref Ref) ([]byte, error) {
	path, err := s.blobPath(ref)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write(data)
	if got := refFor(h); got != ref {
		return nil, fmt.Errorf("blob %s is corrupt: contents hash to %s", ref, got)
	}
	return data, nil
}

// Partial is a blob being written, usually a response as it streams in.
type Partial struct {
	store *Store
	file  *os.File
	hash  hash.Hash
	size  int64
	tail  []byte // the end of what's been written, for the tripwire
}

// NewPartial starts a blob. The name only has to be unique within partial/.
func (s *Store) NewPartial(name string) (*Partial, error) {
	f, err := os.OpenFile(filepath.Join(s.dir, "partial", name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &Partial{store: s, file: f, hash: sha256.New()}, nil
}

// Write checks for secrets before anything reaches the disk. When a response is
// streaming through here, that's also before any of it reaches the browser.
func (p *Partial) Write(b []byte) (int, error) {
	tripwire := p.store.tripwire
	window := append(p.tail, b...)
	tripwire.Check("a blob", window)
	p.tail = append(p.tail[:0], window[max(len(window)-tripwire.Overlap(), 0):]...)

	n, err := p.file.Write(b)
	p.hash.Write(b[:n])
	p.size += int64(n)
	return n, err
}

// Size returns the number of bytes written so far.
func (p *Partial) Size() int64 { return p.size }

// Abort discards the partial blob.
func (p *Partial) Abort() {
	p.file.Close()
	os.Remove(p.file.Name())
}

// Commit makes the bytes durable and moves them to their final name.
func (p *Partial) Commit() (Ref, error) {
	defer p.file.Close()
	tmp := p.file.Name()
	if err := p.file.Sync(); err != nil {
		return "", err
	}
	ref := refFor(p.hash)
	path, err := p.store.blobPath(ref)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		os.Remove(tmp)
		return ref, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	if err := errors.Join(syncDir(dir), syncDir(filepath.Dir(dir))); err != nil {
		return "", err
	}
	return ref, nil
}

// Append records an event durably. It sets the event's time if it's unset.
func (s *Store) Append(ev *Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	line, err := json.Marshal(ev, json.Deterministic(true))
	if err != nil {
		return err
	}
	s.tripwire.Check("an event", line)

	f, err := s.fileFor(ev.Time.UTC().Format(time.DateOnly))
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		// Part of the line may have landed. Reopening repairs it, so it can't
		// swallow the next event.
		s.closeEvents()
	}
	return err
}

func (s *Store) fileFor(day string) (*os.File, error) {
	if s.file != nil && s.day == day {
		return s.file, nil
	}
	dir := filepath.Join(s.dir, "events")
	path := filepath.Join(dir, day+".jsonl")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	// A crash can leave a torn last line. Finish it so it can't swallow the
	// next event.
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if size := info.Size(); size > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], size-1); err != nil {
			f.Close()
			return nil, err
		}
		if last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				f.Close()
				return nil, err
			}
		}
	}
	if err := syncDir(dir); err != nil {
		f.Close()
		return nil, err
	}
	if s.file != nil {
		s.file.Close()
	}
	s.file, s.day = f, day
	return f, nil
}

// Replay calls fn for every recorded event, oldest first. A line that can't be
// parsed is an error, except for a torn line left by a crash: those are
// reported through torn and skipped.
func (s *Store) Replay(fn func(*Event) error, torn func(file string, line int)) error {
	dir := filepath.Join(s.dir, "events")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)

	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		lines := bytes.Split(data, []byte{'\n'})
		for i, line := range lines {
			if len(line) == 0 {
				continue
			}
			var ev Event
			if err := json.Unmarshal(line, &ev); err != nil {
				if torn != nil && isTornWrite(line) {
					torn(name, i+1)
					continue
				}
				return fmt.Errorf("%s line %d: %w", name, i+1, err)
			}
			if err := fn(&ev); err != nil {
				return fmt.Errorf("%s line %d: %w", name, i+1, err)
			}
		}
	}
	return nil
}

// isTornWrite reports whether a bad line is the start of a JSON object that got
// cut off, as an interrupted append leaves it. Anything else that fails to parse
// is real damage. It doesn't parse the JSON, because a cut can fall in the middle
// of a character, and it only has to tell whether the object was ever closed.
func isTornWrite(line []byte) bool {
	if line[0] != '{' {
		return false
	}
	depth := 0
	inString, escaped := false, false
	for _, c := range line {
		switch {
		case escaped:
			escaped = false
		case inString:
			escaped = c == '\\'
			inString = c != '"'
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth--
		}
	}
	return inString || depth > 0
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
