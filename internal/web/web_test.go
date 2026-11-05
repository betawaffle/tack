package web

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/openrouter"
	"github.com/betawaffle/tack/internal/secret"
	"github.com/betawaffle/tack/internal/store"
)

const reply = `data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":"stop"}]}` + "\n\n"

func site(t *testing.T) *httptest.Server {
	t.Helper()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, reply)
	}))
	t.Cleanup(model.Close)
	key := secret.NewKey("sk-or-test-not-a-real-key")
	st, err := store.Open(t.TempDir(), secret.NewTestTripwire(func(where string) { t.Errorf("tripwire: %s", where) }, key))
	if err != nil {
		t.Fatal(err)
	}
	c, err := core.New(core.Config{Store: st, Client: openrouter.NewClient(model.URL, key), Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(Handler(c, func() string { return "the default" }))
	t.Cleanup(s.Close)
	return s
}

func post(t *testing.T, url, body string, wantStatus int) string {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("POST %s = %d: %s", url, resp.StatusCode, data)
	}
	return string(data)
}

func field(t *testing.T, data, name string) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		t.Fatal(err)
	}
	return m[name]
}

func TestSystemPromptFlow(t *testing.T) {
	s := site(t)
	conv := s.URL + "/api/conversations"

	resp, err := http.Get(s.URL + "/api/system-default")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if field(t, string(data), "text") != "the default" {
		t.Errorf("default = %s", data)
	}

	id := field(t, post(t, conv, `{"system":"Be brief."}`, 200), "id")
	base := conv + "/" + id

	if out := post(t, base+"/messages", `{"text":"Hi"}`, 200); !strings.Contains(out, `"kind":"done"`) {
		t.Errorf("stream = %s", out)
	}
	post(t, base+"/reply", `{}`, http.StatusConflict)

	message := field(t, post(t, base+"/system", `{"text":"Rule."}`, 200), "message")
	if out := post(t, base+"/reply", `{}`, 200); !strings.Contains(out, `"kind":"done"`) {
		t.Errorf("stream = %s", out)
	}

	editID := field(t, post(t, base+"/system/"+message+"/edits", `{"text":"Better rule."}`, 200), "edit")
	post(t, base+"/edits/"+editID+"/discard", ``, 200)
	post(t, base+"/edits/"+editID+"/apply", ``, http.StatusNotFound)

	post(t, base+"/messages", `{"text":"  "}`, http.StatusBadRequest)
	post(t, base+"/system", `{"text":""}`, http.StatusBadRequest)
	post(t, conv+"/nope/reply", ``, http.StatusNotFound)
}
