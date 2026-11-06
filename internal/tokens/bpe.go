package tokens

import (
	"bytes"
	"compress/gzip"
	"container/heap"
	"encoding/json/v2"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// tokenizer is a byte-level BPE tokenizer, loaded from a Hugging Face tokenizer.json. It covers
// what DeepSeek's file uses, and refuses a file that asks for anything else rather than
// producing a count that's wrong.
type tokenizer struct {
	vocab   map[string]int32
	ranks   map[[2]string]int32
	added   map[string]int32
	starts  [256]bool // bytes an added token can start with
	longest int
	split   func(string) []string

	mu    sync.Mutex
	cache map[string]int
}

// byteLevel maps each byte to the character that stands for it in the vocabulary.
var byteLevel = func() [256]string {
	var out [256]string
	next := rune(256)
	for b := range 256 {
		switch {
		case b >= '!' && b <= '~', b >= 0xa1 && b <= 0xac, b >= 0xae && b <= 0xff:
			out[b] = string(rune(b))
		default:
			out[b] = string(next)
			next++
		}
	}
	return out
}()

func loadTokenizer(packed []byte, split func(string) []string) (*tokenizer, error) {
	reader, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	var file struct {
		AddedTokens []struct {
			ID      int32  `json:"id"`
			Content string `json:"content"`
		} `json:"added_tokens"`
		Model struct {
			Type   string           `json:"type"`
			Vocab  map[string]int32 `json:"vocab"`
			Merges []string         `json:"merges"`
		} `json:"model"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	if file.Model.Type != "BPE" {
		return nil, fmt.Errorf("a %s tokenizer isn't supported", file.Model.Type)
	}
	t := &tokenizer{
		vocab: file.Model.Vocab, split: split,
		ranks: make(map[[2]string]int32, len(file.Model.Merges)),
		added: make(map[string]int32, len(file.AddedTokens)),
		cache: map[string]int{},
	}
	for i, merge := range file.Model.Merges {
		left, right, ok := strings.Cut(merge, " ")
		if !ok {
			return nil, fmt.Errorf("a merge with no space: %q", merge)
		}
		t.ranks[[2]string{left, right}] = int32(i)
	}
	for _, token := range file.AddedTokens {
		t.added[token.Content] = token.ID
		t.starts[token.Content[0]] = true
		t.longest = max(t.longest, len(token.Content))
	}
	return t, nil
}

// Count returns how many tokens text is. An added token anywhere in the text counts as that
// token, because that's what happens to it: the model can't tell it from the real thing.
func (t *tokenizer) Count(text string) int {
	total, at := 0, 0
	for i := 0; i < len(text); i++ {
		if !t.starts[text[i]] {
			continue
		}
		// The longest one here, like the matcher Hugging Face uses.
		for end := min(len(text), i+t.longest); end > i; end-- {
			if _, ok := t.added[text[i:end]]; ok {
				total += t.countPlain(text[at:i]) + 1
				at, i = end, end-1
				break
			}
		}
	}
	return total + t.countPlain(text[at:])
}

func (t *tokenizer) countPlain(text string) int {
	total := 0
	for _, piece := range t.split(text) {
		total += t.countPiece(piece)
	}
	return total
}

// cacheBelow is the longest piece worth remembering, which is nearly all of them. The cap on
// the cache's size is so that what a conversation holds can't grow without limit.
const (
	cacheBelow = 64
	cacheLimit = 1 << 18
)

func (t *tokenizer) countPiece(piece string) int {
	if piece == "" {
		return 0
	}
	if len(piece) < cacheBelow {
		t.mu.Lock()
		n, ok := t.cache[piece]
		t.mu.Unlock()
		if ok {
			return n
		}
	}
	n := len(t.merge(piece))
	if len(piece) < cacheBelow {
		t.mu.Lock()
		if len(t.cache) >= cacheLimit {
			clear(t.cache)
		}
		t.cache[piece] = n
		t.mu.Unlock()
	}
	return n
}

// merge applies merges lowest rank first. It keeps the symbols in a linked list with a heap of
// candidate merges, so that one very long word, like a run of a single letter in some output,
// takes about n log n and not n squared.
type mergeCandidate struct {
	rank, left, right   int32
	leftText, rightText string
}

type mergeHeap []mergeCandidate

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	if h[i].rank != h[j].rank {
		return h[i].rank < h[j].rank
	}
	return h[i].left < h[j].left
}
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any)   { *h = append(*h, x.(mergeCandidate)) }
func (h *mergeHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func (t *tokenizer) merge(piece string) []string {
	symbols := make([]string, 0, len(piece))
	for i := range len(piece) {
		symbols = append(symbols, byteLevel[piece[i]])
	}
	next := make([]int32, len(symbols))
	prev := make([]int32, len(symbols))
	for i := range symbols {
		next[i], prev[i] = int32(i+1), int32(i-1)
	}
	if len(next) > 0 {
		next[len(next)-1] = -1
	}
	candidates := &mergeHeap{}
	consider := func(left, right int32) {
		if left < 0 || right < 0 {
			return
		}
		if rank, ok := t.ranks[[2]string{symbols[left], symbols[right]}]; ok {
			heap.Push(candidates, mergeCandidate{rank, left, right, symbols[left], symbols[right]})
		}
	}
	for i := range int32(len(symbols)) - 1 {
		consider(i, i+1)
	}
	for candidates.Len() > 0 {
		c := heap.Pop(candidates).(mergeCandidate)
		// It's out of date if either side has since merged into something else.
		if next[c.left] != c.right || symbols[c.left] != c.leftText || symbols[c.right] != c.rightText {
			continue
		}
		symbols[c.left] += symbols[c.right]
		symbols[c.right] = ""
		after := next[c.right]
		next[c.left] = after
		if after >= 0 {
			prev[after] = c.left
		}
		consider(prev[c.left], c.left)
		consider(c.left, after)
	}
	out := make([]string, 0, 4)
	for i := int32(0); i >= 0 && len(symbols) > 0; i = next[i] {
		// With no unknown token, something not in the vocabulary is left out.
		if _, ok := t.vocab[symbols[i]]; ok {
			out = append(out, symbols[i])
		}
	}
	return out
}

// isolate splits text around every match of a pattern, keeping the matches.
func isolate(pieces []string, pattern *regexp.Regexp) []string {
	var out []string
	for _, piece := range pieces {
		at := 0
		for _, match := range pattern.FindAllStringIndex(piece, -1) {
			if match[0] > at {
				out = append(out, piece[at:match[0]])
			}
			out = append(out, piece[match[0]:match[1]])
			at = match[1]
		}
		if at < len(piece) {
			out = append(out, piece[at:])
		}
	}
	return out
}

// isSpace is what \s means to the regular expression library that Hugging Face's uses, which is
// wider than \s in Go.
func isSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func runeLen(s string) int {
	_, n := utf8.DecodeRuneInString(s)
	return n
}
