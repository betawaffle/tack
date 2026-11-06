package core

import (
	"net/http"
	"slices"
	"strings"

	"github.com/betawaffle/tack/internal/store"
)

// What a model will accept is something only it can say, so tack learns it from what's
// in the record: each call says which unusual things its request had, and how it ended
// shows what the model made of them. A refusal only counts when there's one thing it can
// be blamed on, so nothing here tries to read an error message.
const (
	featureName         = "name"
	featureAssistant    = "written_assistant" // an assistant message a person wrote or changed
	featureReasoning    = "written_reasoning"
	featureTool         = "written_tool" // a tool result a person wrote or changed
	featureEndAssistant = "ends_with_assistant"
	featureRolePrefix   = "role:" // then the name of a role the API doesn't define
)

type capabilityKey struct{ model, host, feature string }

type capability struct {
	allowed bool
	why     string // what the host said, if it refused
}

// knownRefused is what's known without having to find out.
func knownRefused(model, feature string) bool {
	// Anthropic signs reasoning, so it can't be written by anyone else.
	return strings.HasPrefix(model, "anthropic/") && feature == featureReasoning
}

// RefusedFeature is something a model won't accept.
type RefusedFeature struct {
	Feature string `json:"feature"`
	Why     string `json:"why,omitzero"`
	// Learned says it came from a response, so it's worth being able to try again.
	Learned bool `json:"learned,omitzero"`
}

// features lists the unusual things the next request would have. The caller holds mu.
func (conv *conversation) features(entries []store.Entry) []string {
	var out []string
	add := func(feature string) {
		if !slices.Contains(out, feature) {
			out = append(out, feature)
		}
	}
	for _, it := range conv.items {
		if it.name != "" && (len(it.parts) > 0 || len(it.writtenCalls) > 0) {
			add(featureName)
		}
		switch {
		case it.role == "assistant":
			if len(it.parts) > 0 || len(it.writtenCalls) > 0 {
				add(featureAssistant)
				if it.reasoning != "" {
					add(featureReasoning)
				}
			}
		case it.role == "tool":
			add(featureTool)
		case it.written() && len(it.parts) > 0:
			add(featureRolePrefix + it.role)
		case it.role == "" && it.override != nil && usable(it.finished):
			add(featureAssistant)
			if it.override.reasoning != "" {
				add(featureReasoning)
			}
		}
		for _, call := range it.calls {
			if call.edited {
				add(featureTool)
			}
		}
	}
	if n := len(entries); n > 0 && entries[n-1].Role == "assistant" {
		add(featureEndAssistant)
	}
	slices.Sort(out)
	return out
}

// learn takes what a finished call shows. The caller holds mu, or is New.
func (c *Core) learn(it *item, ev *store.Event) {
	if len(it.features) == 0 {
		return
	}
	key := func(feature string) capabilityKey { return capabilityKey{it.model, it.host, feature} }
	switch {
	case ev.Status == http.StatusOK && ev.Error == "":
		for _, feature := range it.features {
			c.capabilities[key(feature)] = capability{allowed: true}
		}
	case ev.Status == http.StatusBadRequest || ev.Status == http.StatusUnprocessableEntity:
		var unknown []string
		for _, feature := range it.features {
			known, ok := c.capabilities[key(feature)]
			if ok && !known.allowed {
				return // That already explains it.
			}
			if !ok {
				unknown = append(unknown, feature)
			}
		}
		if len(unknown) == 1 {
			c.capabilities[key(unknown[0])] = capability{why: ev.Error}
		}
	}
}

// refused lists what a model and host won't accept, as far as is known. The caller holds mu.
func (c *Core) refused(model, host string) []RefusedFeature {
	var out []RefusedFeature
	for key, known := range c.capabilities {
		if key.model == model && key.host == host && !known.allowed {
			out = append(out, RefusedFeature{Feature: key.feature, Why: known.why, Learned: true})
		}
	}
	// Having seen it work beats what was expected.
	if knownRefused(model, featureReasoning) && !c.capabilities[capabilityKey{model, host, featureReasoning}].allowed &&
		!slices.ContainsFunc(out, func(r RefusedFeature) bool { return r.Feature == featureReasoning }) {
		out = append(out, RefusedFeature{Feature: featureReasoning})
	}
	slices.SortFunc(out, func(a, b RefusedFeature) int { return strings.Compare(a.Feature, b.Feature) })
	return out
}

// ForgetRefusal lets a model be asked again, since a refusal can be blamed on the wrong
// thing. For a conversation's current model and host.
func (c *Core) ForgetRefusal(convID, feature string) error {
	c.mu.Lock()
	conv := c.convs[convID]
	if conv == nil {
		c.mu.Unlock()
		return ErrNotFound
	}
	settings := conv.effectiveSettings(c.settings)
	c.mu.Unlock()
	return c.record(&store.Event{Type: store.CapabilityForgotten, Model: settings.Model, Host: settings.Host, Feature: feature})
}
