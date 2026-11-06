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
	// Flags are what the approval card pointed out, as judged when the call arrived.
	Flags []string `json:"flags,omitzero"`
}

// Choice is what was decided about one tool call. Parts are the result the model
// will see, and they're absent only when a call was allowed to run.
type Choice struct {
	ToolCall string `json:"tool_call"`
	Allow    bool   `json:"allow"`
	// Network lets an allowed call use the network.
	Network bool   `json:"network,omitzero"`
	Parts   []Part `json:"parts,omitzero"`
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
	RestartRequested  = "restart_requested"
	ConversationAdded = "conversation_created"
	// ConversationForked starts a conversation from the state of another one at that
	// moment. It's its own type so that a build that can't work out the history
	// refuses the record, rather than showing an empty conversation.
	ConversationForked = "conversation_forked"
	UserMessage        = "user_message"
	SystemMessage      = "system_message"
	EditProposed       = "system_edit_proposed"
	EditApplied        = "system_edit_applied"
	EditDiscarded      = "system_edit_discarded"
	CallStarted        = "model_call_started"
	CallFinished       = "model_call_finished"
	DecisionRequested  = "decision_requested"
	DecisionMade       = "decision_made"
	ToolStarted        = "tool_started"
	ToolFinished       = "tool_finished"
	ConfigLoaded       = "config_loaded"
	SettingsProposed   = "settings_proposed"
	SettingsApplied    = "settings_applied"
	SettingsDiscarded  = "settings_discarded"
	BudgetAdded        = "budget_added"
	AllowanceStarted   = "allowance_started"
	BudgetStopped      = "budget_stopped"
	PauseRequested     = "pause_requested"
	PauseCleared       = "pause_cleared"
	NoteAdded          = "note_added"
)

// Note is an item on a conversation's page. The text is a block, like other content.
type Note struct {
	Kind   string `json:"kind"`
	Key    string `json:"key"`
	Status string `json:"status,omitzero"`
	Text   Ref    `json:"text"`
}

// Settings is everything a conversation keeps as its own. The config only provides
// the defaults new conversations start with. A setting that's absent from an older
// record means the default, through WithDefaults.
type Settings struct {
	Model string `json:"model,omitzero"`
	Host  string `json:"host,omitzero"`

	BudgetUSD      float64 `json:"budget_usd,omitzero"`
	BudgetTopUpUSD float64 `json:"budget_top_up_usd,omitzero"`

	SearchSkip            []string          `json:"search_skip,omitzero"`
	CommandEnv            map[string]string `json:"command_env,omitzero"`
	CommandTimeoutSeconds int               `json:"command_timeout_seconds,omitzero"`
	SandboxReadOnly       []string          `json:"sandbox_read_only,omitzero"`
	SandboxReadWrite      []string          `json:"sandbox_read_write,omitzero"`
	// SandboxProtected are paths inside the working directory that stay read-only.
	SandboxProtected []string `json:"sandbox_protected,omitzero"`

	ToolTimeoutSeconds    int `json:"tool_timeout_seconds,omitzero"`
	InspectTimeoutSeconds int `json:"inspect_timeout_seconds,omitzero"`
	OutputWaitSeconds     int `json:"output_wait_seconds,omitzero"`
	StopGraceSeconds      int `json:"stop_grace_seconds,omitzero"`
	MaxReadBytes          int `json:"max_read_bytes,omitzero"`
	MaxListed             int `json:"max_listed,omitzero"`
	MaxMatches            int `json:"max_matches,omitzero"`
	MaxSearchFileBytes    int `json:"max_search_file_bytes,omitzero"`
	MaxLineShown          int `json:"max_line_shown,omitzero"`
}

// DefaultSandboxReadOnly is what a sandboxed command can read, unless the config says
// otherwise: the system, and the parts of /etc that programs commonly need.
var DefaultSandboxReadOnly = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib64",
	"/etc/alternatives", "/etc/ca-certificates", "/etc/crypto-policies", "/etc/gitconfig",
	"/etc/group", "/etc/hosts", "/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
	"/etc/localtime", "/etc/nsswitch.conf", "/etc/passwd", "/etc/pki", "/etc/resolv.conf", "/etc/ssl",
}

// WithDefaults fills in the limits. Model, host and the lists stay as they are, since
// empty means something there.
func (s Settings) WithDefaults() Settings {
	fill := func(value *int, fallback int) {
		if *value == 0 {
			*value = fallback
		}
	}
	fill(&s.CommandTimeoutSeconds, 120)
	fill(&s.ToolTimeoutSeconds, 60)
	fill(&s.InspectTimeoutSeconds, 2)
	fill(&s.OutputWaitSeconds, 5)
	fill(&s.StopGraceSeconds, 5)
	if s.BudgetUSD == 0 {
		s.BudgetUSD = 0.001
	}
	if s.BudgetTopUpUSD == 0 {
		s.BudgetTopUpUSD = 0.001
	}
	fill(&s.MaxReadBytes, 16<<20)
	fill(&s.MaxListed, 10000)
	fill(&s.MaxMatches, 1000)
	fill(&s.MaxSearchFileBytes, 2<<20)
	fill(&s.MaxLineShown, 500)
	if s.SandboxReadOnly == nil {
		s.SandboxReadOnly = DefaultSandboxReadOnly
	}
	if s.SandboxProtected == nil {
		// Hooks and config in .git run unsandboxed the next time someone uses git.
		s.SandboxProtected = []string{".git"}
	}
	return s
}

// Validate refuses settings that can't work. Paths have to be absolute, because
// nothing in tack may depend on its own working directory.
func (s Settings) Validate() error {
	if s.Model == "" {
		return errors.New("model can't be empty")
	}
	for name, value := range map[string]int{
		"command_timeout_seconds": s.CommandTimeoutSeconds,
		"tool_timeout_seconds":    s.ToolTimeoutSeconds,
		"inspect_timeout_seconds": s.InspectTimeoutSeconds,
		"output_wait_seconds":     s.OutputWaitSeconds,
		"stop_grace_seconds":      s.StopGraceSeconds,
		"max_read_bytes":          s.MaxReadBytes,
		"max_listed":              s.MaxListed,
		"max_matches":             s.MaxMatches,
		"max_search_file_bytes":   s.MaxSearchFileBytes,
		"max_line_shown":          s.MaxLineShown,
	} {
		if value <= 0 {
			return fmt.Errorf("%s has to be positive", name)
		}
	}
	if s.BudgetUSD <= 0 || s.BudgetTopUpUSD <= 0 {
		return errors.New("budget_usd and budget_top_up_usd have to be positive")
	}
	for _, path := range append(slices.Clone(s.SandboxReadOnly), s.SandboxReadWrite...) {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("sandbox path %q has to be absolute", path)
		}
	}
	for _, path := range s.SandboxProtected {
		if path == "" || !filepath.IsLocal(path) {
			return fmt.Errorf("sandbox_protected path %q has to be inside the working directory", path)
		}
	}
	for _, name := range s.SearchSkip {
		if name == "" || strings.ContainsRune(name, filepath.Separator) {
			return errors.New("search_skip has directory names, not paths")
		}
	}
	return nil
}

// Artifact is a named block a tool wants kept beside its output, like the contents
// of a file before an edit.
type Artifact struct {
	Name  string `json:"name"`
	Block Ref    `json:"block"`
}

// Known reports whether this build understands an event type. A record with a type
// it doesn't know was written by a newer build, and reading it anyway could mean
// getting history wrong, such as not knowing a tool call was already decided.
func Known(eventType string) bool {
	switch eventType {
	case TackStarted, RestartRequested, ConversationAdded, ConversationForked, UserMessage, SystemMessage, EditProposed, EditApplied, EditDiscarded,
		CallStarted, CallFinished, DecisionRequested, DecisionMade, ToolStarted, ToolFinished,
		ConfigLoaded, SettingsProposed, SettingsApplied, SettingsDiscarded,
		BudgetAdded, AllowanceStarted, BudgetStopped, PauseRequested, PauseCleared, NoteAdded:
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

	// restart_requested: what the build it's restarting into changes, if that's known.
	Changes []string `json:"changes,omitzero"`

	// conversation_created and everything that happens in a conversation
	Conversation string `json:"conversation,omitzero"`

	// conversation_created: the exact tool definitions its calls send, and where
	// their relative paths start.
	// settings_proposed has the settings, and settings_applied the new definitions.
	Tools    Ref       `json:"tools,omitzero"`
	Workdir  string    `json:"workdir,omitzero"`
	Settings *Settings `json:"settings,omitzero"`

	// conversation_forked: where it came from, and how many of that conversation's
	// items it keeps, counting from the start. Absent means none. It also has settings.
	ForkedFrom string `json:"forked_from,omitzero"`
	ForkItems  int    `json:"fork_items,omitzero"`

	// budget_added, budget_stopped, allowance_started. allowance_started means someone
	// let the model spend up to amount_usd more, from when spent_usd had been spent.
	// It replaces the budget so far, and budget_added no longer gets written.
	AmountUSD   float64 `json:"amount_usd,omitzero"`
	SpentUSD    float64 `json:"spent_usd,omitzero"`
	EstimateUSD float64 `json:"estimate_usd,omitzero"`
	LimitUSD    float64 `json:"limit_usd,omitzero"`

	// note_added
	Note *Note `json:"note,omitzero"`

	// config_loaded: the settings in force, at startup and on each reload.
	Config Ref `json:"config,omitzero"`

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
	RequestBytes   int                 `json:"request_bytes,omitzero"`
	RequestHeaders map[string][]string `json:"request_headers,omitzero"`

	// model_call_finished
	Response        Ref                 `json:"response,omitzero"`
	Cancelled       bool                `json:"cancelled,omitzero"`
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
	Execution string     `json:"execution,omitzero"` // one run of a call. A re-run gets a new one
	Network   bool       `json:"network,omitzero"`
	ToolCall  string     `json:"tool_call,omitzero"`
	Output    Ref        `json:"output,omitzero"`
	Artifacts []Artifact `json:"artifacts,omitzero"`
}

var errReadOnly = errors.New("this store was opened read-only")

// Store is the single writer for a data directory.
type Store struct {
	dir      string
	tripwire *secret.Tripwire
	lock     *os.File
	readOnly bool

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

// OpenReadOnly reads a data directory that a running tack may be using. It takes no
// lock and creates nothing, so it can't get in the way, and anything that writes fails.
func OpenReadOnly(dir string) (*Store, error) {
	if _, err := os.Stat(filepath.Join(dir, "events")); err != nil {
		return nil, err
	}
	return &Store{dir: dir, readOnly: true}, nil
}

// Close releases the directory.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeEvents()
	if s.lock == nil {
		return nil
	}
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
	if s.readOnly {
		return nil, errReadOnly
	}
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
	if s.readOnly {
		return errReadOnly
	}
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
