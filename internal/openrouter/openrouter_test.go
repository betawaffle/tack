package openrouter

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/betawaffle/tack/internal/secret"
)

const testKey = "sk-or-test-not-a-real-key"

// serve returns a client for a server that answers with handler.
func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return NewClient(s.URL, secret.NewKey(testKey))
}

func stream(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, text) }
}

func TestRenderIsStableAndOrdered(t *testing.T) {
	r := Request{
		Model:     "m",
		Routing:   Routing{Order: []string{"h"}},
		Messages:  []Message{{Role: "user", Content: "hi <&> é"}},
		SessionID: "s",
	}
	first, err := Render(r)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"model":"m","messages":[{"role":"user","content":"hi <&> é"}],"provider":{"order":["h"],"allow_fallbacks":false},"session_id":"s","stream":true}`
	if string(first) != want {
		t.Errorf("got  %s\nwant %s", first, want)
	}
	again, _ := Render(r)
	if !bytes.Equal(first, again) {
		t.Error("rendering twice differs")
	}
}

func TestRenderRefusesInvalidUTF8(t *testing.T) {
	_, err := Render(Request{Messages: []Message{{Role: "user", Content: "bad \xff"}}})
	if err == nil {
		t.Error("invalid UTF-8 was silently accepted")
	}
}

func TestStreamParsing(t *testing.T) {
	const text = ": OPENROUTER PROCESSING\r\n\r\n" +
		`data: {"id":"g1","provider":"Morph","choices":[{"delta":{"reasoning":"think "}}]}` + "\r\n\r\n" +
		`data: {"id":"g1","choices":[{"delta":{"reasoning_content":"more "}}]}` + "\n\n" +
		"data: {not json}\n\n" +
		"event: something\n" +
		`data:{"id":"g1","usage":null,"error":null,"choices":[{"delta":{"content":"Hel"}}]}` + "\n\n" +
		`data: {"id":"g2","provider":"Other","choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":5}}` + "\n\n" +
		"data: [DONE]\n\n"
	c := serve(t, stream(text))
	var raw bytes.Buffer
	var deltas []Delta
	res, err := c.Do(context.Background(), []byte("{}"), &raw, func(d Delta) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if raw.String() != text {
		t.Errorf("raw bytes changed:\n%q", raw.String())
	}
	if res.Text != "Hello" || res.Reasoning != "think more " {
		t.Errorf("text %q, reasoning %q", res.Text, res.Reasoning)
	}
	if res.Host != "Morph" || res.GenerationID != "g1" || res.FinishReason != "stop" {
		t.Errorf("res = %+v", res)
	}
	if res.ParseErrors != 1 {
		t.Errorf("parse errors = %d", res.ParseErrors)
	}
	if res.Error != "" {
		t.Errorf("error = %q", res.Error)
	}
	if !bytes.Equal(res.Usage, jsontext.Value(`{"prompt_tokens":5}`)) {
		t.Errorf("usage = %s", res.Usage)
	}
	if len(deltas) != 4 || res.TTFT <= 0 || res.Duration < res.TTFT {
		t.Errorf("deltas %d, ttft %v, duration %v", len(deltas), res.TTFT, res.Duration)
	}
}

func TestUsageIsKeptExactly(t *testing.T) {
	const usage = `{"cost":0.000016414,"z":1,"a":{"nested":[1,2]}}`
	c := serve(t, stream(`data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}],"usage":`+usage+"}\n\n"))
	res, err := c.Do(context.Background(), []byte("{}"), io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Usage) != usage {
		t.Errorf("usage = %s", res.Usage)
	}
}

func TestLastLineWithoutNewline(t *testing.T) {
	c := serve(t, stream(`data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`))
	res, err := c.Do(context.Background(), []byte("{}"), io.Discard, nil)
	if err != nil || res.Text != "x" || res.Error != "" {
		t.Errorf("res = %+v, err = %v", res, err)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"http status", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusPaymentRequired)
			io.WriteString(w, "no credits")
		}, "HTTP 402: no credits"},
		{"error in stream", stream(`data: {"error":{"message":"boom"}}` + "\n\n"), "boom"},
		{"error in choice", stream(`data: {"choices":[{"error":{"message":"bang"},"finish_reason":"error"}]}` + "\n\n"), "bang"},
		{"cut off", stream(`data: {"choices":[{"delta":{"content":"x"}}]}` + "\n\n"), "without a finish reason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw bytes.Buffer
			res, err := serve(t, tc.handler).Do(context.Background(), []byte("{}"), &raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(res.Error, tc.want) {
				t.Errorf("error = %q", res.Error)
			}
			if raw.Len() == 0 {
				t.Error("raw response wasn't kept")
			}
		})
	}
}

func TestIdleTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"par"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	c.IdleTimeout = 100 * time.Millisecond
	var raw bytes.Buffer
	res, err := c.Do(context.Background(), []byte("{}"), &raw, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(res.Error, "nothing received") {
		t.Errorf("error = %q", res.Error)
	}
	if res.Text != "par" || raw.Len() == 0 {
		t.Errorf("partial reply lost: text %q, raw %d bytes", res.Text, raw.Len())
	}
}

func TestKeepAlivesPreventTheIdleTimeout(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		for range 4 {
			io.WriteString(w, ": OPENROUTER PROCESSING\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(60 * time.Millisecond)
		}
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`+"\n\n")
	})
	c.IdleTimeout = 150 * time.Millisecond
	res, err := c.Do(context.Background(), []byte("{}"), io.Discard, nil)
	if err != nil || res.Text != "x" {
		t.Errorf("res = %+v, err = %v", res, err)
	}
}

func TestKeyStaysOutOfTheResult(t *testing.T) {
	var got string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Set-Cookie", "a=b")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`+"\n\n")
	})
	res, err := c.Do(context.Background(), []byte("{}"), io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Bearer "+testKey {
		t.Error("key wasn't sent")
	}
	if _, ok := res.RequestHeaders["Authorization"]; ok {
		t.Error("recorded headers include Authorization")
	}
	if _, ok := res.ResponseHeaders["Set-Cookie"]; ok {
		t.Error("recorded headers include cookies")
	}
	if strings.Contains(fmt.Sprintf("%+v", res), testKey) {
		t.Error("key found in the result")
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var elsewhere bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere = true }))
	defer target.Close()
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	res, err := c.Do(context.Background(), []byte("{}"), io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if elsewhere {
		t.Error("followed a redirect, and could have sent the key")
	}
	if res.Status != http.StatusTemporaryRedirect || !strings.Contains(res.Error, "HTTP 307") {
		t.Errorf("res = %+v", res)
	}
}

func TestWholeReply(t *testing.T) {
	const text = `{"id":"g1","provider":"Morph","usage":{"prompt_tokens":5},"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"Hi","reasoning":"hmm","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}},{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]}}]}`
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		io.WriteString(w, text)
	})
	var raw bytes.Buffer
	var deltas []Delta
	res, err := c.Do(context.Background(), []byte(`{"stream":false}`), &raw, func(d Delta) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if raw.String() != text {
		t.Errorf("raw bytes changed:\n%q", raw.String())
	}
	if res.Text != "Hi" || res.Reasoning != "hmm" || res.Host != "Morph" || res.GenerationID != "g1" || res.FinishReason != "tool_calls" || res.Error != "" {
		t.Errorf("res = %+v", res)
	}
	if len(res.ToolCalls) != 2 || res.ToolCalls[0] != (FunctionCallResult{"c1", "f", `{"a":1}`}) || res.ToolCalls[1].Name != "g" {
		t.Errorf("tool calls = %+v", res.ToolCalls)
	}
	if !bytes.Equal(res.Usage, jsontext.Value(`{"prompt_tokens":5}`)) || len(deltas) != 1 {
		t.Errorf("usage %s, deltas %d", res.Usage, len(deltas))
	}

	bad := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{nope")
	})
	res, err = bad.Do(context.Background(), []byte("{}"), io.Discard, nil)
	if err != nil || res.ParseErrors != 1 || res.Error == "" {
		t.Errorf("bad JSON: err %v, res %+v", err, res)
	}
}
