package web

import (
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/openrouter"
	"github.com/betawaffle/tack/internal/prompt"
	"github.com/betawaffle/tack/internal/secret"
	"github.com/betawaffle/tack/internal/store"
)

const reply = `data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":"stop"}]}` + "\n\n"

// saved is what the last site's page saved as the defaults.
var saved *prompt.Defaults

func site(t *testing.T) *httptest.Server {
	t.Helper()
	saved = nil
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
	s := httptest.NewServer(Handler(c, Options{Defaults: func() Defaults {
		return Defaults{Prompt: prompt.Defaults{Sections: []prompt.Section{{Name: "a", Paragraphs: prompt.Split("the default")}}}}
	}, Save: func(d prompt.Defaults) error {
		saved = &d
		return nil
	}, Reload: noReload, Hosts: []string{"framework"}}))
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

	id := field(t, post(t, conv, `{"sections":[{"name":"a","text":"Be brief."}]}`, 200), "id")
	base := conv + "/" + id

	if out := post(t, base+"/messages", `{"text":"Hi"}`, 200); !strings.Contains(out, `"kind":"done"`) {
		t.Errorf("stream = %s", out)
	}
	post(t, base+"/reply", `{}`, http.StatusConflict)

	post(t, base+"/held", `{"role":"system","text":"Rule."}`, 200)
	if out := post(t, base+"/reply", `{}`, 200); !strings.Contains(out, `"kind":"done"`) {
		t.Errorf("stream = %s", out)
	}
	var view core.Conversation
	if err := json.Unmarshal([]byte(get(t, base)), &view); err != nil {
		t.Fatal(err)
	}
	message := view.Items[len(view.Items)-2].Message

	editID := field(t, post(t, base+"/system/"+message+"/edits", `{"text":"Better rule."}`, 200), "edit")
	post(t, base+"/edits/"+editID+"/discard", ``, 200)
	post(t, base+"/edits/"+editID+"/apply", ``, http.StatusNotFound)

	post(t, base+"/messages", `{"text":"  "}`, http.StatusBadRequest)
	post(t, base+"/held", `{"role":"system","text":""}`, http.StatusBadRequest)
	post(t, base+"/held", `{"role":"tool","text":"x","tool_call_id":"nope"}`, http.StatusBadRequest)
	post(t, conv+"/nope/reply", ``, http.StatusNotFound)
}

func TestPageAndPolicy(t *testing.T) {
	s := site(t)
	for _, path := range []string{"/", "/static/app.js", "/static/start.js", "/static/style.css", "/static/markdown.js", "/requests", "/static/requests.js", "/static/requests.css"} {
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

func TestStartingFromSectionsAndTools(t *testing.T) {
	s := site(t)
	conv := s.URL + "/api/conversations"
	post(t, conv, `{"system":"the old way"}`, http.StatusBadRequest)
	post(t, conv, `{"tools":[{"name":"nope"}]}`, http.StatusBadRequest)
	body := `{"sections":[{"name":"a","text":"On {{date}}."},{"name":"b","text":"Not sent.","off":true},{"name":"c","text":"C"}],` +
		`"tools":[{"name":"look","custom":true,"definition":{"type":"function","function":{"name":"look"}}}]}`
	id := field(t, post(t, conv, body, 200), "id")
	var view core.Conversation
	if err := json.Unmarshal([]byte(get(t, conv+"/"+id)), &view); err != nil {
		t.Fatal(err)
	}
	want := "On " + prompt.Date(time.Now()) + ".\n\nC"
	if view.Items[0].Text != want || !view.Tools || len(view.CustomTools) != 1 {
		t.Errorf("view = %q, %+v", view.Items[0].Text, view)
	}
	if tools := get(t, conv+"/"+id+"/tools"); tools != `[{"type":"function","function":{"name":"look"}}]` {
		t.Errorf("tools = %s", tools)
	}
	// The tools change like settings, and some settings change straight away.
	proposal := field(t, post(t, conv+"/"+id+"/tools", `{"tools":[]}`, 200), "proposal")
	post(t, conv+"/"+id+"/settings/"+proposal+"/apply", "", 200)
	post(t, conv+"/"+id+"/code-calls/nope", `{"text":"x"}`, http.StatusNotFound)
	post(t, conv+"/"+id+"/code-calls/nope", `{"txet":"x"}`, http.StatusBadRequest)
	post(t, conv+"/"+id+"/settings/now", `{"model":"x"}`, http.StatusBadRequest)
	post(t, conv+"/"+id+"/settings/now", `{"reasoning_effort":"lots"}`, http.StatusBadRequest)
	post(t, conv+"/"+id+"/settings/now", `{"auto_run_code":true,"host":"relace/fp4","reasoning_effort":"high"}`, 200)
	if got := get(t, conv+"/"+id); !strings.Contains(got, `"auto_run_code":true`) || !strings.Contains(got, `"reasoning_effort":"high"`) || strings.Contains(got, `"custom_tools"`) {
		t.Errorf("after: %s", got)
	}
}

func TestDefaultsCanBeSaved(t *testing.T) {
	s := site(t)
	post(t, s.URL+"/api/defaults", `{"sections":[],"oops":1}`, http.StatusBadRequest)
	post(t, s.URL+"/api/defaults", `{"sections":[{"name":"x","text":"X"}]}`, 200)
	if saved == nil || len(saved.Sections) != 1 || saved.Tools == nil {
		t.Errorf("saved = %+v", saved)
	}
}

func TestSettingsAreHeldAndValidated(t *testing.T) {
	s := site(t)
	id := field(t, post(t, s.URL+"/api/conversations", `{}`, 200), "id")
	base := s.URL + "/api/conversations/" + id + "/settings"
	post(t, base, `{"modle":"x"}`, http.StatusBadRequest)
	post(t, base, `{"host":"no-model"}`, http.StatusBadRequest)
	proposal := field(t, post(t, base, `{"model":"other/model"}`, 200), "proposal")
	if got := get(t, s.URL+"/api/conversations/"+id); !strings.Contains(got, `"pending_settings"`) {
		t.Errorf("after proposing: %s", got)
	}
	post(t, base+"/nope/apply", "", http.StatusNotFound)
	post(t, base+"/"+proposal+"/apply", "", 200)
	post(t, base+"/"+proposal+"/apply", "", http.StatusNotFound)
	if got := get(t, s.URL+"/api/conversations/"+id); !strings.Contains(got, `"model":"other/model"`) || strings.Contains(got, `"pending_settings"`) {
		t.Errorf("after applying: %s", got)
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

func noReload() (Reload, error) { return Reload{}, errors.New("nothing to reload") }

func TestReloadReportsFailure(t *testing.T) {
	s := site(t)
	if got := post(t, s.URL+"/api/reload", "", http.StatusBadRequest); !strings.Contains(got, "nothing to reload") {
		t.Errorf("reload = %s", got)
	}
}

func TestOnlyKnownHostnamesGetIn(t *testing.T) {
	s := site(t)
	for host, want := range map[string]int{
		"evil.example":       http.StatusForbidden,
		"evil.example:8484":  http.StatusForbidden,
		"framework.evil.com": http.StatusForbidden,
		"framework":          http.StatusOK,
		"FRAMEWORK:8484":     http.StatusOK,
		"localhost:8484":     http.StatusOK,
		"127.0.0.1:8484":     http.StatusOK,
		"100.64.0.1":         http.StatusOK,
		"[::1]:8484":         http.StatusOK,
	} {
		req, err := http.NewRequest(http.MethodGet, s.URL+"/api/conversations", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", host, resp.StatusCode, want)
		}
	}
}

func TestHoldingAMessageMakesNoCall(t *testing.T) {
	s := site(t)
	id := field(t, post(t, s.URL+"/api/conversations", `{}`, 200), "id")
	base := s.URL + "/api/conversations/" + id
	post(t, base+"/held", `{"text":"  "}`, http.StatusBadRequest)
	post(t, s.URL+"/api/conversations/nope/held", `{"text":"x"}`, http.StatusNotFound)
	post(t, base+"/held", `{"text":"Wait for it."}`, 200)
	got := get(t, base)
	if !strings.Contains(got, `"queued":true`) || !strings.Contains(got, `"can_reply":true`) || strings.Contains(got, `"assistant"`) {
		t.Errorf("conversation = %s", got)
	}
}

func TestAPageThatsOutOfDateIsRefused(t *testing.T) {
	s := site(t)
	send := func(required string) int {
		req, _ := http.NewRequest(http.MethodPost, s.URL+"/api/conversations", strings.NewReader("{}"))
		if required != "" {
			req.Header.Set(pageRequiredHeader, required)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := send(strconv.Itoa(pageRequired - 1)); got != http.StatusConflict {
		t.Errorf("an old page got %d", got)
	}
	if got := send(strconv.Itoa(pageRequired)); got != http.StatusOK {
		t.Errorf("the current page got %d", got)
	}
	if got := send(""); got != http.StatusOK {
		t.Errorf("something that isn't a page got %d", got)
	}
}

func TestThePageKnowsItsVersion(t *testing.T) {
	s := site(t)
	resp, err := http.Get(s.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page, _ := io.ReadAll(resp.Body)
	want := `content="` + pageVersion() + `" data-required="` + strconv.Itoa(pageRequired) + `"`
	if !strings.Contains(string(page), want) || strings.Contains(string(page), "{{") {
		t.Errorf("no %s in the page", want)
	}
	status, err := http.Get(s.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer status.Body.Close()
	var got struct {
		PageVersion  string `json:"page_version"`
		PageRequired int    `json:"page_required"`
	}
	if err := json.UnmarshalRead(status.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.PageVersion != pageVersion() || got.PageRequired != pageRequired {
		t.Errorf("status = %+v", got)
	}
}

func TestAnActionBasedOnAnOldRevisionIsRefused(t *testing.T) {
	s := site(t)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(post(t, s.URL+"/api/conversations", "{}", http.StatusOK)), &created); err != nil {
		t.Fatal(err)
	}
	hold := func(revision string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, s.URL+"/api/conversations/"+created.ID+"/held", strings.NewReader(`{"text":"hi"}`))
		if revision != "" {
			req.Header.Set("X-Tack-Revision", revision)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if got := hold("0"); got.StatusCode != http.StatusOK {
		t.Errorf("current: %d", got.StatusCode)
	}
	got := hold("0")
	if got.StatusCode != http.StatusConflict || got.Header.Get("X-Tack-Stale") == "" {
		t.Errorf("stale: %d, %v", got.StatusCode, got.Header)
	}
	if got := hold(""); got.StatusCode != http.StatusOK {
		t.Errorf("not saying: %d", got.StatusCode)
	}
	if got := hold("nonsense"); got.StatusCode != http.StatusBadRequest {
		t.Errorf("nonsense: %d", got.StatusCode)
	}
}

func TestManualRequests(t *testing.T) {
	s := site(t)
	if page := get(t, s.URL+"/requests"); !strings.Contains(page, "/static/requests.js") || strings.Contains(page, "{{page_version}}") {
		t.Errorf("page = %q", page)
	}
	post(t, s.URL+"/api/manual-calls", `{"messages":[]}`, http.StatusBadRequest)
	body := `{"model":"m","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	lines := post(t, s.URL+"/api/manual-calls", body, http.StatusOK)
	var first struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(lines, "\n", 2)[0]), &first); err != nil || first.Kind != "call" || first.ID == "" {
		t.Fatalf("first update %q: %v", lines, err)
	}
	if list := get(t, s.URL+"/api/manual-calls"); !strings.Contains(list, first.ID) {
		t.Errorf("list = %s", list)
	}
	if detail := get(t, s.URL+"/api/manual-calls/"+first.ID); !strings.Contains(detail, `"request":`) {
		t.Errorf("detail = %s", detail)
	}
	if got := get(t, s.URL+"/api/recorded-calls/"+first.ID); got != body {
		t.Errorf("recorded request %q", got)
	}
	if calls := get(t, s.URL+"/api/recorded-calls"); !strings.Contains(calls, first.ID) {
		t.Errorf("recorded calls = %s", calls)
	}
	post(t, s.URL+"/api/manual-calls/"+first.ID+"/cancel", "", http.StatusConflict)
	post(t, s.URL+"/api/manual-calls/nope/cancel", "", http.StatusNotFound)
}

func TestAnyRequestCanBePreviewed(t *testing.T) {
	s := site(t)
	out := post(t, s.URL+"/api/prompt-preview", `{"model":"deepseek/deepseek-v4.1-flash-1017","request":{"messages":[{"role":"user","content":"Hello there"}]}}`, 200)
	if !strings.Contains(out, `Hello there`) || !strings.Contains(out, `"tokens":[`) {
		t.Errorf("preview = %s", out)
	}
	post(t, s.URL+"/api/prompt-preview", `{"model":"other/model","request":{}}`, http.StatusNotFound)
	post(t, s.URL+"/api/prompt-preview", `{"model":"deepseek/deepseek-v4.1-flash-1017","request":"nope"}`, http.StatusBadRequest)
}
