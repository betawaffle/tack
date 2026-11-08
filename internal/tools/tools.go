// Package tools has the tools models can ask for that reach anything outside the
// conversation's own record. Each call is approved by a person, unless a conversation
// opted in, and its output can wait for a second approval. The sandbox is what keeps
// code from reaching anything of the user's.
//
// Name and Definition are what the model sees, and Run is what happens, so a
// different model can later get a different interface to the same code.
package tools

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"time"

	"github.com/betawaffle/tack/internal/cgroup"
	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/docs"
	"github.com/betawaffle/tack/internal/store"
)

// FlagCode is on every run_code. It can run without asking, but only in a conversation
// someone has opted in, because its sandbox has nothing of theirs in it.
const FlagCode = "Runs code"

// Options configure the tools.
type Options struct {
	// Protected files are hidden from the sandbox wherever they are.
	Protected []string
	// Cgroups, if set, gives each run its own cgroup.
	Cgroups *cgroup.Manager
	// Docs is the documentation code can read, under /docs, with Variables filled in.
	Docs      docs.Library
	Variables func(store.Settings) map[string]string
	// PythonSite is a directory of packages code can import, built for Python PythonFor, as
	// major.minor. They're only offered while that's the Python there is.
	PythonSite string
	PythonFor  string
}

// All returns every tool.
func All(opts Options) []core.Tool {
	r := &reader{protected: opts.Protected, cgroups: opts.Cgroups, docs: opts.Docs, variables: opts.Variables, pythonSite: opts.PythonSite, pythonFor: opts.PythonFor}
	return []core.Tool{{
		Name:          "run_code",
		Definition:    definition("run_code", codeDescription(PackageList(opts.PythonSite)), codeProperties, `["language","code"]`),
		AutoSendBytes: codeAutoSendBytes,
		AutoRun:       func(settings store.Settings) bool { return settings.AutoRunCode },
		Inspect:       r.inspectCode,
		Run:           r.runCode,
		Timeout: func(settings store.Settings) time.Duration {
			return time.Duration(settings.WithDefaults().CommandTimeoutSeconds) * time.Second
		},
	}}
}

func definition(name, description, properties, required string) jsontext.Value {
	return jsontext.Value(fmt.Sprintf(`{"type":"function","function":{"name":%q,"description":%q,"parameters":{"type":"object","properties":%s,"required":%s,"additionalProperties":false}}}`,
		name, description, properties, required))
}

type reader struct {
	protected []string
	cgroups   *cgroup.Manager
	memory    budgets
	docs      docs.Library
	variables func(store.Settings) map[string]string

	pythonSite, pythonFor string
}

func (r *reader) site() string { return r.pythonSite }

// arguments reads a call's strings, refusing anything it doesn't expect.
func arguments(call core.Call, allowed ...string) (map[string]string, error) {
	out := map[string]string{}
	for name, value := range call.Args {
		if !slices.Contains(allowed, name) {
			return nil, fmt.Errorf("unexpected argument %q", name)
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return nil, fmt.Errorf("argument %q has to be a string", name)
		}
		out[name] = text
	}
	return out, nil
}
