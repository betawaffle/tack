package tokens

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// vectors.json has counts from Hugging Face's own tokenizers library for this file, which is
// the implementation hosts run.
func TestCountsMatchTheReferenceTokenizer(t *testing.T) {
	data, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Text  string `json:"text"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	tk, err := loadDeepSeekV41()
	if err != nil {
		t.Fatal(err)
	}
	wrong := 0
	for _, v := range vectors {
		if got := tk.Count(v.Text); got != v.Count {
			wrong++
			if wrong < 12 {
				t.Errorf("%.80q = %d, want %d", v.Text, got, v.Count)
			}
		}
	}
	if wrong > 0 {
		t.Errorf("%d of %d wrong", wrong, len(vectors))
	}
}

// DeepSeek's own test cases for how messages become a prompt. Its others use things tack doesn't
// send, like images and its internal task tokens, so those files aren't here.
func TestPromptsMatchDeepSeeksEncoder(t *testing.T) {
	for i := 1; i <= 2; i++ {
		input, err := os.ReadFile(fmt.Sprintf("testdata/test_input_%d.json", i))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(fmt.Sprintf("testdata/test_output_%d.txt", i))
		if err != nil {
			t.Fatal(err)
		}
		var body jsontext.Value = input
		if body.Kind() == '[' {
			body = jsontext.Value(`{"messages":` + string(input) + `}`)
		}
		var mode struct {
			ThinkingMode string `json:"thinking_mode"`
		}
		json.Unmarshal(body, &mode)
		got, err := dsRender(body, mode.ThinkingMode == "thinking", dsDefaultEffort)
		if err != nil {
			t.Fatal(err)
		}
		if got != strings.TrimRight(string(want), "\n") && got != string(want) {
			other, _ := dsRender(body, mode.ThinkingMode != "thinking", dsDefaultEffort)
			if other == string(want) || other == strings.TrimRight(string(want), "\n") {
				continue // That case is for the other mode, which the file doesn't say.
			}
			t.Errorf("case %d:\n got %q\nwant %q", i, got, want)
		}
	}
}

func TestLongRunsStayFast(t *testing.T) {
	tk, err := loadDeepSeekV41()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	n := tk.Count(strings.Repeat("z", 1<<20)) + tk.Count(strings.Repeat(" ", 1<<20)) + tk.Count(strings.Repeat("<", 1<<20))
	if took := time.Since(start); took > 20*time.Second || n == 0 {
		t.Errorf("took %v for %d tokens", took, n)
	}
}

func TestOnlyKnownModelsHaveACounter(t *testing.T) {
	if _, ok := For("deepseek/deepseek-v4.1-flash-1017"); !ok {
		t.Error("the model tack uses has no counter")
	}
	if _, ok := For("deepseek/deepseek-v4.1-flash-9999"); ok {
		t.Error("a model it knows nothing about has one")
	}
}

func TestRequestsCountTheWholePrompt(t *testing.T) {
	counter, _ := For("deepseek/deepseek-v4.1-flash-1017")
	// A real call: the count it got and the count the host reported were both 32.
	body := []byte(`{"model":"deepseek/deepseek-v4.1-flash-1017","messages":[{"role":"user","content":"Hello there"}]}`)
	got, err := counter.Request(body)
	if err != nil || got <= 0 {
		t.Fatalf("%d, %v", got, err)
	}
	longer, err := counter.Request([]byte(`{"messages":[{"role":"user","content":"Hello there, and some more words"}]}`))
	if err != nil || longer <= got {
		t.Errorf("%d then %d, %v", got, longer, err)
	}
	if n, err := counter.Text("a\x00b"); err != nil || n != 1 {
		t.Errorf("a NUL should count for nothing: %d, %v", n, err)
	}
	if _, err := counter.Request([]byte("not json")); err == nil {
		t.Error("bad JSON was counted")
	}
}
