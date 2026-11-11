package core

import (
	"strings"
	"testing"

	"encoding/json/jsontext"

	"github.com/betawaffle/tack/internal/tokens"
)

// A call that got nothing back leaves the same request to send again; one that got a reply
// has it, and an empty message from the user after it.
func TestTheRequestAfterACall(t *testing.T) {
	sent := jsontext.Value(`{"messages":[{"content":"Hi.","role":"user"}],"model":"m"}`)
	for _, out := range []*tokens.Output{nil, {}} {
		next, err := nextRequest(CallRequest{Model: "m", Request: sent, Output: out})
		if err != nil || string(next) != string(sent) {
			t.Errorf("after nothing (%v): %s, %v", out, next, err)
		}
	}
	next, err := nextRequest(CallRequest{Model: "m", Request: sent, Output: &tokens.Output{Reasoning: "Thinking."}})
	if err != nil || !strings.Contains(string(next), `"reasoning":"Thinking."`) || !strings.HasSuffix(string(next), `{"role":"user","content":""}],"model":"m"}`) {
		t.Errorf("after a reply: %s, %v", next, err)
	}
}
