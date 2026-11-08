package core

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"

	"github.com/betawaffle/tack/internal/store"
)

// Offer is a tool to offer a conversation. A built-in tool's Definition can be left
// out, which means tack's own, or differ from tack's only in its descriptions. A custom
// tool is one tack doesn't have: its definition is whatever the person wrote, and they
// write the result of every call to it.
type Offer struct {
	Name       string         `json:"name"`
	Custom     bool           `json:"custom,omitzero"`
	Definition jsontext.Value `json:"definition,omitzero"`
}

// offered builds the definitions for a list of tools, in its order, and names the
// custom ones. Nothing about a custom definition is checked beyond its name, because
// trying out what a model does with an unusual one is the point of having them.
func (c *Core) offered(settings store.Settings, offers []Offer) (store.Ref, []string, error) {
	var definitions []jsontext.Value
	var custom, seen []string
	for _, offer := range offers {
		if offer.Name == "" {
			return "", nil, fmt.Errorf("%w: a tool has no name", ErrBadTools)
		}
		if slices.Contains(seen, offer.Name) {
			return "", nil, fmt.Errorf("%w: there are two tools called %q", ErrBadTools, offer.Name)
		}
		seen = append(seen, offer.Name)
		tool := c.findTool(offer.Name)
		switch {
		case offer.Custom && tool != nil:
			return "", nil, fmt.Errorf("%w: tack already has a tool called %q", ErrBadTools, offer.Name)
		case offer.Custom:
			var head struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(offer.Definition, &head); err != nil {
				return "", nil, fmt.Errorf("%w: %s's definition: %v", ErrBadTools, offer.Name, err)
			}
			if head.Function.Name != offer.Name {
				return "", nil, fmt.Errorf("%w: %s's definition has to have function.name %q", ErrBadTools, offer.Name, offer.Name)
			}
			definitions = append(definitions, offer.Definition)
			custom = append(custom, offer.Name)
		case tool == nil:
			return "", nil, fmt.Errorf("%w: there's no tool called %q", ErrBadTools, offer.Name)
		case offer.Definition == nil:
			definitions = append(definitions, tool.define(settings))
		case !sameExceptDescriptions(tool.define(settings), offer.Definition):
			return "", nil, fmt.Errorf("%w: only the descriptions and allowed values of %s can change", ErrBadTools, offer.Name)
		default:
			definitions = append(definitions, offer.Definition)
		}
	}
	ref, err := c.putDefinitions(definitions)
	return ref, custom, err
}

// ProposeTools holds a new set of tools for a conversation, applied like settings. The
// tools are part of the start of every request, so the next call misses the whole cache.
func (c *Core) ProposeTools(convID string, offers []Offer) (string, error) {
	c.mu.Lock()
	conv := c.convs[convID]
	var settings store.Settings
	if conv != nil {
		settings = conv.effectiveSettings(c.settings).WithDefaults()
	}
	c.mu.Unlock()
	if conv == nil {
		return "", ErrNotFound
	}
	tools, custom, err := c.offered(settings, offers)
	if err != nil {
		return "", err
	}
	id := store.NewID()
	return id, c.record(&store.Event{Type: store.SettingsProposed, Conversation: convID, Edit: id, Settings: &settings,
		Tools: tools, ToolsProposed: true, CustomTools: custom})
}

// ChangeSettingsNow changes settings that have nothing to do with what's waiting, such as
// whether to run code without asking, straight away, even while a reply is running. A change
// someone's holding is held again with the same change, so it isn't lost.
func (c *Core) ChangeSettingsNow(convID string, change func(*store.Settings)) error {
	c.mu.Lock()
	conv := c.convs[convID]
	if conv == nil {
		c.mu.Unlock()
		return ErrNotFound
	}
	before := conv.effectiveSettings(c.settings).WithDefaults()
	held := conv.pendingSettings
	c.mu.Unlock()
	after := before
	change(&after)
	if !nothingWaitingDiffers(before, after) {
		return fmt.Errorf("%w: only settings that don't affect what's waiting can change straight away", ErrBadSettings)
	}
	if err := after.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSettings, err)
	}
	id := store.NewID()
	if err := c.record(&store.Event{Type: store.SettingsProposed, Conversation: convID, Edit: id, Settings: &after}); err != nil {
		return err
	}
	if err := c.record(&store.Event{Type: store.SettingsApplied, Conversation: convID, Edit: id}); err != nil {
		return err
	}
	if held == nil {
		return nil
	}
	again := held.settings
	change(&again)
	return c.record(&store.Event{Type: store.SettingsProposed, Conversation: convID, Edit: store.NewID(), Settings: &again,
		Tools: held.tools, ToolsProposed: held.toolsProposed, CustomTools: held.custom})
}

// isCustom says a conversation offers a tool that a person answers. The caller holds mu.
func (conv *conversation) isCustom(name string) bool { return slices.Contains(conv.custom, name) }

// byHand says a person writes the call's result: it's to a custom tool, or tack can't run it.
func (conv *conversation) byHand(call *toolCall) bool {
	return conv.isCustom(call.Name) || call.Unsupported != ""
}

// Unsupported is what a tool's Inspect returns for a call it understands but can't run, like
// one asking for a language tack doesn't have yet. Rather than failing, the call goes to a
// person, whatever the settings, with Answer ready to send as the result.
type Unsupported struct {
	Answer string
}

func (u Unsupported) Error() string { return u.Answer }

func hasKey(m map[string]string, key string) bool {
	_, ok := m[key]
	return ok
}
