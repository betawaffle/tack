// Package secret keeps credentials away from models and logs.
//
// Prevention is the goal: a Key can't be formatted or serialized, and the only
// way to use it is to authorize a request. The Tripwire is the backstop. It
// exists to catch our own bugs loudly, not to scrub anything.
package secret

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
)

const redacted = "[redacted]"

// Key is a credential. Its value is deliberately hard to get at.
type Key struct {
	value []byte
}

// Load reads a key from a file that only its owner can access.
func Load(path string) (Key, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Key{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Key{}, fmt.Errorf("%s is accessible by other users (mode %o)", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Key{}, err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return Key{}, fmt.Errorf("%s is empty", path)
	}
	return Key{value: data}, nil
}

// NewKey makes a Key from a value. Tests use it.
func NewKey(value string) Key {
	return Key{value: []byte(value)}
}

// Authorize adds the key to a request as a bearer token.
func (k Key) Authorize(r *http.Request) {
	r.Header.Set("Authorization", "Bearer "+string(k.value))
}

func (k Key) String() string   { return redacted }
func (k Key) GoString() string { return redacted }

// Format covers every fmt verb, including %x and %#v.
func (k Key) Format(f fmt.State, _ rune) { io.WriteString(f, redacted) }

func (k Key) LogValue() slog.Value { return slog.StringValue(redacted) }

func (k Key) MarshalText() ([]byte, error) {
	return nil, errors.New("secret: refusing to serialize a key")
}

// Tripwire ends the process if it sees a known secret where none should be.
// If it fires, the key has to be rotated: nothing here tries to clean up.
type Tripwire struct {
	keys  []Key
	fatal func(where string)
}

// NewTripwire returns a Tripwire that exits the process when it fires.
func NewTripwire(keys ...Key) *Tripwire {
	return &Tripwire{keys: keys, fatal: func(where string) {
		fmt.Fprintf(os.Stderr, "\n*** TRIPWIRE: a known secret appeared in %s. Exiting. Rotate the key. ***\n\n", where)
		os.Exit(70)
	}}
}

// NewTestTripwire returns a Tripwire that reports to fatal instead of exiting.
func NewTestTripwire(fatal func(where string), keys ...Key) *Tripwire {
	return &Tripwire{keys: keys, fatal: fatal}
}

// Check fires the tripwire if data contains a known secret.
func (t *Tripwire) Check(where string, data []byte) {
	for _, k := range t.keys {
		if bytes.Contains(data, k.value) {
			t.fatal(where)
			return
		}
	}
}
