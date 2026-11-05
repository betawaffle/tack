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
	"github.com/betawaffle/tack/internal/prompt"
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
	s := httptest.NewServer(Handler(c, func() Defaults {
		return Defaults{Sections: []prompt.Section{{Name: "a", Text: "the default"}}}
	}))
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

	resp, err := http.Get(s.URL + "/api/defaults")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(data), `"the default"`) {
		t.Errorf("defaults = %s", data)
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

func TestPageAndPolicy(t *testing.T) {
	s := site(t)
	for _, path := range []string{"/", "/static/app.js", "/static/style.css", "/static/markdown.js"} {
		resp, err := http.Get(s.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || len(body) == 0 {
			t.Errorf("%s = %d, %d bytes", path, resp.StatusCode, len(body))
		}
		policy := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(policy, "default-src 'none'") || strings.Contains(policy, "unsafe-inline") {
			t.Errorf("%s policy = %q", path, policy)
		}
	}
	if resp, err := http.Get(s.URL + "/markdown.test.js"); err != nil || resp.StatusCode != 404 {
		t.Errorf("test file served: %v %v", resp, err)
	}
}

func TestCrossSitePostIsRefused(t *testing.T) {
	s := site(t)
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/api/conversations", strings.NewReader("{}"))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestNewConversationNeedsARealWorkdirForTools(t *testing.T) {
	s := toolSite(t, t.TempDir())
	conv := s.URL + "/api/conversations"
	for _, workdir := range []string{"", "relative", "/does/not/exist", "/etc/hostname"} {
		body, _ := json.Marshal(map[string]any{"tools": true, "workdir": workdir})
		post(t, conv, string(body), http.StatusBadRequest)
	}
	post(t, conv, `{"tools":false,"workdir":"relative"}`, 200)
}

func TestToolsAreOnlyOfferedWhenAvailable(t *testing.T) {
	s := site(t)
	workdir := t.TempDir()
	body, _ := json.Marshal(map[string]any{"tools": true, "workdir": workdir})
	id := field(t, post(t, s.URL+"/api/conversations", string(body), 200), "id")
	resp, err := http.Get(s.URL + "/api/conversations/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(data), `"tools"`) {
		t.Errorf("conversation = %s", data)
	}
}

func TestDefaultsHideTheToolsSectionWhenThereAreNone(t *testing.T) {
	s := toolSite(t, "")
	data := get(t, s.URL+"/api/defaults")
	if !strings.Contains(data, `"name":"tools"`) {
		t.Errorf("with tools: %s", data)
	}
	s = site(t)
	if data := get(t, s.URL+"/api/defaults"); strings.Contains(data, "with-tools") {
		t.Errorf("without tools: %s", data)
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return string(data)
}

func toolSite(t *testing.T, workdir string) *httptest.Server {
	t.Helper()
	key := secret.NewKey("sk-or-test-not-a-real-key")
	st, err := store.Open(t.TempDir(), secret.NewTestTripwire(func(where string) { t.Errorf("tripwire: %s", where) }, key))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c, err := core.New(core.Config{Store: st, Client: openrouter.NewClient("http://127.0.0.1:1", key), Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(Handler(c, func() Defaults {
		return Defaults{Sections: []prompt.Section{{Name: "tools", For: "tools", Text: "x"}}, Workdir: workdir, Tools: true}
	}))
	t.Cleanup(s.Close)
	return s
}
