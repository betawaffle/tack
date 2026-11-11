package web

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"encoding/json/jsontext"
)

// What a send's answer has in it that the page already has: the reply, which came as it was
// written. The answer is the draft after the call, which has the reply in it several times
// over (in the request, in the prompt's parts, in the call before's text), so each copy of what
// streamed is taken out, and where it was is said instead, for the page to put it back.

// streamedAt is where a piece of what streamed goes back: into the string at At, a JSON
// Pointer into the answer, Offset UTF-16 code units in, once the ones before it are back.
type streamedAt struct {
	At     string `json:"at"`
	Offset int    `json:"offset"`
	Is     string `json:"is"`
}

// Text that streamed and is shorter than this isn't worth taking out.
const streamedAbove = 64

// stripStreamed takes every copy of known's texts out of the strings in data, which is JSON,
// and says where they were. Names in objects are left alone.
func stripStreamed(data []byte, known map[string]string) ([]byte, []streamedAt, error) {
	var kinds []string
	for kind, text := range known {
		if len(text) >= streamedAbove {
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) == 0 {
		return data, nil, nil
	}
	d := jsontext.NewDecoder(bytes.NewReader(data))
	var out bytes.Buffer
	e := jsontext.NewEncoder(&out)
	var at []streamedAt
	for {
		t, err := d.ReadToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		kind, n := d.StackIndex(d.StackDepth())
		if t.Kind() != '"' || (kind == '{' && n%2 == 1) {
			if err := e.WriteToken(t); err != nil {
				return nil, nil, err
			}
			continue
		}
		s := t.String()
		pointer := string(d.StackPointer())
		var kept strings.Builder
		full := 0 // UTF-16 code units of the whole string so far, with what's taken out
		for {
			first, which := -1, ""
			for _, k := range kinds {
				if i := strings.Index(s, known[k]); i >= 0 && (first < 0 || i < first || (i == first && len(known[k]) > len(known[which]))) {
					first, which = i, k
				}
			}
			if first < 0 {
				kept.WriteString(s)
				break
			}
			kept.WriteString(s[:first])
			full += utf16Len(s[:first])
			at = append(at, streamedAt{At: pointer, Offset: full, Is: which})
			full += utf16Len(known[which])
			s = s[first+len(known[which]):]
		}
		if err := e.WriteToken(jsontext.String(kept.String())); err != nil {
			return nil, nil, err
		}
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), at, nil
}

// withStreamed is a reply with what streamed taken out of it, and where it goes back. It also
// says how long each text that streamed was, in UTF-16 code units, so that a page that didn't
// get just that can tell, and open the draft whole instead.
func withStreamed(reply []byte, known map[string]string) ([]byte, error) {
	stripped, at, err := stripStreamed(reply, known)
	if err != nil || len(at) == 0 {
		return stripped, err
	}
	list := []byte("[")
	for i, a := range at {
		if i > 0 {
			list = append(list, ',')
		}
		list, _ = jsontext.AppendQuote(append(list, `{"at":`...), a.At)
		list = fmt.Appendf(list, `,"offset":%d,"is":`, a.Offset)
		list, _ = jsontext.AppendQuote(list, a.Is)
		list = append(list, '}')
	}
	list = append(list, ']')
	lengths := []byte("{")
	for kind, text := range known {
		if len(lengths) > 1 {
			lengths = append(lengths, ',')
		}
		lengths, _ = jsontext.AppendQuote(lengths, kind)
		lengths = fmt.Appendf(lengths, ":%d", utf16Len(text))
	}
	lengths = append(lengths, '}')
	// The reply is an object; these go last in it.
	stripped = bytes.TrimSuffix(stripped, []byte("}"))
	stripped = append(append(stripped, `,"streamed":`...), list...)
	return append(append(append(stripped, `,"lengths":`...), lengths...), '}'), nil
}
