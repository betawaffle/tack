package web

import (
	"bytes"
	"strings"
	"testing"

	"encoding/json/jsontext"
	"encoding/json/v2"
)

func TestPatchRoundTrips(t *testing.T) {
	cases := []struct{ name, a, b string }{
		{"same", `{"a":1}`, `{"a":1}`},
		{"a value", `{"a":1,"b":"x"}`, `{"a":1,"b":"y"}`},
		{"a new key goes last", `{"a":1}`, `{"a":1,"c":[1]}`},
		{"a key gone", `{"a":1,"b":2,"c":3}`, `{"a":1,"c":3}`},
		{"keys moved", `{"a":1,"b":2}`, `{"b":2,"a":1}`},
		{"a new key before others", `{"b":2}`, `{"a":1,"b":2}`},
		{"gone and back at the end", `{"a":1,"b":2,"c":3}`, `{"a":1,"c":3,"b":2}`},
		{"an element changed", `{"m":[{"c":"one"},{"c":"two"},{"c":"three"}]}`, `{"m":[{"c":"one"},{"c":"2"},{"c":"three"}]}`},
		{"an element added", `[1,2,3]`, `[1,2,9,3]`},
		{"elements gone", `[1,2,3,4]`, `[1,4]`},
		{"at the start", `[1,2,3]`, `[0,1,2,3]`},
		{"emptied", `[1,2,3]`, `[]`},
		{"from empty", `[]`, `[1]`},
		{"kind changed", `{"a":[1]}`, `{"a":{"b":1}}`},
		{"nested", `{"a":{"b":{"c":[1,{"d":"x"}]}}}`, `{"a":{"b":{"c":[1,{"d":"y","e":null}]}}}`},
		{"strings with escapes", `{"a\"b":" "}`, `{"a\"b":"<\n>"}`},
		{"from nothing", ``, `{"a":1}`},
	}
	for _, c := range cases {
		p, err := diffJSON(jsontext.Value(c.a), jsontext.Value(c.b))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.a == c.b && p != nil {
			t.Errorf("%s: a patch for no change: %s", c.name, p)
		}
		got, err := applyJSON(jsontext.Value(c.a), p)
		if err != nil {
			t.Fatalf("%s: applying %s: %v", c.name, p, err)
		}
		want := jsontext.Value(c.b)
		if c.b != "" {
			want.Compact()
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: patch %s gave %s, not %s", c.name, p, got, want)
		}
	}
}

func TestPatchIsSmall(t *testing.T) {
	big := `{"messages":[`
	for i := range 500 {
		if i > 0 {
			big += ","
		}
		big += `{"role":"user","content":"a message that is some way into the conversation"}`
	}
	big += `]}`
	changed := bytes.Replace([]byte(big), []byte(`way into`), []byte(`way in to`), 1)
	p, err := diffJSON(jsontext.Value(big), jsontext.Value(changed))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"o":{"messages":{"i":[[0,{"o":{"content":{"=":"a message that is some way in to the conversation"}}}]]}}}`; string(p) != want {
		t.Errorf("patch %s", p)
	}
}

func TestPatchRefusesNonsense(t *testing.T) {
	for _, p := range []string{`{}`, `{"x":1}`, `{"=":1,"x":2}`, `{"s":[5,1,[]]}`, `{"s":[0,-1,[]]}`, `{"i":[[9,{"=":1}]]}`, `{"o":{},"d":"a"}`, `{"o":{"a":{"o":{}}}}`} {
		if _, err := applyJSON(jsontext.Value(`[1,2]`), jsontext.Value(p)); err == nil {
			if _, err := applyJSON(jsontext.Value(`{"b":1}`), jsontext.Value(p)); err == nil {
				t.Errorf("%s was applied", p)
			}
		}
	}
}

func TestPatchSplicesLongStrings(t *testing.T) {
	long := strings.Repeat("words, words and more words. ", 40)
	cases := []struct{ a, b string }{
		{long + "end", long + "the end"},
		{"start " + long, "a start " + long},
		{long + "é and 𝄞 and é" + long, long + "é but 𝄞 or é" + long},
		{long + "𝄞" + long, long + "𝄟" + long}, // differs only in a surrogate pair's second half
		{long, "short now"},
	}
	for _, c := range cases {
		a, _ := json.Marshal(c.a)
		b, _ := json.Marshal(c.b)
		p, err := diffJSON(a, b)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.b) > 300 && (!bytes.HasPrefix(p, []byte(`{"t":`)) || len(p) > 100) {
			t.Errorf("not a small splice: %s", p)
		}
		got, err := applyJSON(a, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		var s string
		json.Unmarshal(got, &s)
		if s != c.b {
			t.Errorf("%s made %q", p, s)
		}
	}
	// UTF-16 offsets that split a character are refused.
	a, _ := json.Marshal("a𝄞b")
	if _, err := applyJSON(a, jsontext.Value(`{"t":[2,0,"x"]}`)); err == nil {
		t.Error("a splice in the middle of a character was applied")
	}
	if got, err := applyJSON(a, jsontext.Value(`{"t":[3,1,"c"]}`)); err != nil || string(got) != `"a𝄞c"` {
		t.Errorf("after a surrogate pair: %s %v", got, err)
	}
}
