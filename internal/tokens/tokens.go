// Package tokens counts tokens the way a model's host does, as far as that can be known,
// without asking anyone. It's keyed on the exact model, because both the tokenizer and how a
// request becomes a prompt belong to a model, and a guess for some other model would look
// just as precise while being wrong.
package tokens

// Counter counts tokens for one model.
type Counter struct {
	// Request counts the input tokens of a request, given the JSON body sent to the API.
	Request func(body []byte) (int, error)
	// Text counts a piece of text on its own, without what surrounds it in a prompt.
	Text func(text string) (int, error)

	render func(body []byte, out *Output) (*Prompt, error)
	tokens func(text string) ([]int, error)
}

var deepSeekV41 = Counter{Request: countDeepSeekV41, Text: countDeepSeekV41Text, render: renderDeepSeekV41, tokens: tokensDeepSeekV41}

// counters has what's known. Adding a model means adding it here.
var counters = map[string]Counter{
	"deepseek/deepseek-v4.1-flash":      deepSeekV41,
	"deepseek/deepseek-v4.1-flash-1017": deepSeekV41,
}

// For returns the counter for a model, if there is one.
func For(model string) (Counter, bool) {
	counter, ok := counters[model]
	return counter, ok
}
