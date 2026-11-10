package store

import (
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Draft is a call being written. It changes as it's edited, so it isn't in the record, which
// only says what's true: each draft is a file of its own under drafts/, and the record gets the
// call it becomes when it's sent.
type Draft struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// From is the call it follows on from, as conversation/call or manual/call, if any.
	From  string `json:"from,omitzero"`
	Agent string `json:"agent,omitzero"`
	Model string `json:"model"`
	// Request is the body it would send, with whatever call options were changed in it, and
	// Off the messages in it that are turned off, by index: they're kept, but not sent.
	Request jsontext.Value `json:"request"`
	Off     []int          `json:"off,omitzero"`
	// OffTools are the tools in the request that are turned off, by index, kept the same way.
	OffTools []int `json:"off_tools,omitzero"`
	// Sent is the manual call it became, once it's sent.
	Sent string `json:"sent,omitzero"`
}

// ErrNoDraft is a draft that doesn't exist.
var ErrNoDraft = errors.New("there's no such draft")

func (s *Store) draftPath(id string) (string, error) {
	// IDs are NewID's, which are hex, so one can't name a file anywhere else.
	if _, err := hex.DecodeString(id); err != nil || id == "" {
		return "", ErrNoDraft
	}
	return filepath.Join(s.dir, "drafts", id+".json"), nil
}

// SaveDraft writes a draft, replacing what was saved before. It sets Updated, and Created if
// it's unset.
func (s *Store) SaveDraft(d *Draft) error {
	if s.readOnly {
		return errReadOnly
	}
	path, err := s.draftPath(d.ID)
	if err != nil {
		return err
	}
	d.Updated = time.Now().UTC()
	if d.Created.IsZero() {
		d.Created = d.Updated
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	s.tripwire.Check("a draft", data)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".draft-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

// LoadDraft reads a draft.
func (s *Store) LoadDraft(id string) (*Draft, error) {
	path, err := s.draftPath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoDraft
	}
	if err != nil {
		return nil, err
	}
	var d Draft
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("draft %s: %w", id, err)
	}
	return &d, nil
}

// Drafts lists the drafts, the one changed last first.
func (s *Store) Drafts() ([]*Draft, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "drafts"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Draft
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || strings.HasPrefix(id, ".") {
			continue
		}
		d, err := s.LoadDraft(id)
		if err != nil {
			continue
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b *Draft) int { return b.Updated.Compare(a.Updated) })
	return out, nil
}
