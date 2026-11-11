package web

import (
	"bytes"
	"errors"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"

	"encoding/json/jsontext"
	"encoding/json/v2"
)

// A patch says how one JSON value became another, so a big value that changes a little can be
// sent as the change. The page and the server both make and apply them (prompt-patch.js is
// the page's side), in this form:
//
//	{"=": v}                      the value is now v
//	{"o": {key: patch}, "d": [k]} an object: keys with patches, then keys gone (new keys go last)
//	{"i": [[n, patch], ...]}      an array of the same length, with patches for some elements
//	{"s": [at, n, [v, ...]]}      an array with n elements at at replaced by these
//	{"t": [at, n, "text"]}        a string with n characters at at replaced by text, counted
//	                              in UTF-16 code units, as JavaScript counts them
//
// No change at all is null. Where the order of an object's keys matters, an object whose keys
// moved is sent whole, so it's kept: a request's does, where a tool's schema is written into
// the prompt, but what's drawn doesn't.

// member is one of an object's keys and its value, in order.
type member struct {
	name  string
	value jsontext.Value
}

// members reads an object's keys and values, in order.
func members(v jsontext.Value) ([]member, error) {
	d := jsontext.NewDecoder(bytes.NewReader(v))
	if t, err := d.ReadToken(); err != nil || t.Kind() != '{' {
		return nil, errors.New("not an object")
	}
	var out []member
	for d.PeekKind() != '}' {
		t, err := d.ReadToken()
		if err != nil {
			return nil, err
		}
		// A token is only good until the next read.
		name := t.String()
		value, err := d.ReadValue()
		if err != nil {
			return nil, err
		}
		out = append(out, member{name, bytes.Clone(value)})
	}
	return out, nil
}

// elements reads an array's elements.
func elements(v jsontext.Value) ([]jsontext.Value, error) {
	d := jsontext.NewDecoder(bytes.NewReader(v))
	if t, err := d.ReadToken(); err != nil || t.Kind() != '[' {
		return nil, errors.New("not an array")
	}
	var out []jsontext.Value
	for d.PeekKind() != ']' {
		value, err := d.ReadValue()
		if err != nil {
			return nil, err
		}
		out = append(out, bytes.Clone(value))
	}
	return out, nil
}

func objectOf(ms []member) jsontext.Value {
	out := []byte{'{'}
	for i, m := range ms {
		if i > 0 {
			out = append(out, ',')
		}
		out, _ = jsontext.AppendQuote(out, m.name)
		out = append(out, ':')
		out = append(out, m.value...)
	}
	return append(out, '}')
}

func arrayOf(vs []jsontext.Value) jsontext.Value {
	out := []byte{'['}
	for i, v := range vs {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, v...)
	}
	return append(out, ']')
}

// replace is the patch that makes anything v.
func replace(v jsontext.Value) jsontext.Value {
	return jsontext.Value(`{"=":` + string(v) + `}`)
}

// diffJSON is the patch that makes a into b, or nil if they're the same, keeping the order of
// objects' keys. Both are compact JSON, as Marshal makes it, so the same value is the same
// bytes.
func diffJSON(a, b jsontext.Value) (jsontext.Value, error) {
	return diffValues(a, b, true)
}

// diffUnordered is diffJSON for values whose objects' keys can come in any order.
func diffUnordered(a, b jsontext.Value) (jsontext.Value, error) {
	return diffValues(a, b, false)
}

func diffValues(a, b jsontext.Value, ordered bool) (jsontext.Value, error) {
	if bytes.Equal(a, b) {
		return nil, nil
	}
	if len(a) == 0 || a.Kind() != b.Kind() {
		return replace(b), nil
	}
	switch a.Kind() {
	case '{':
		am, err := members(a)
		if err != nil {
			return nil, err
		}
		bm, err := members(b)
		if err != nil {
			return nil, err
		}
		// The keys both have must be in the same order, or it's sent whole.
		in := map[string]int{}
		for i, m := range bm {
			in[m.name] = i
		}
		last := -1
		var gone []string
		old := map[string]jsontext.Value{}
		for _, m := range am {
			i, ok := in[m.name]
			if !ok {
				gone = append(gone, m.name)
				continue
			}
			if ordered && i < last {
				return replace(b), nil
			}
			last = i
			old[m.name] = m.value
		}
		// New keys go last, after the ones both have.
		for i, m := range bm {
			if _, ok := old[m.name]; !ok && ordered && i < last {
				return replace(b), nil
			}
		}
		var changed []member
		for _, m := range bm {
			if prev, ok := old[m.name]; ok {
				p, err := diffValues(prev, m.value, ordered)
				if err != nil {
					return nil, err
				}
				if p != nil {
					changed = append(changed, member{m.name, p})
				}
			} else {
				changed = append(changed, member{m.name, replace(m.value)})
			}
		}
		out := []byte(`{"o":`)
		out = append(out, objectOf(changed)...)
		if len(gone) > 0 {
			names, err := json.Marshal(gone)
			if err != nil {
				return nil, err
			}
			out = append(out, `,"d":`...)
			out = append(out, names...)
		}
		return append(out, '}'), nil
	case '[':
		ae, err := elements(a)
		if err != nil {
			return nil, err
		}
		be, err := elements(b)
		if err != nil {
			return nil, err
		}
		start := 0
		for start < len(ae) && start < len(be) && bytes.Equal(ae[start], be[start]) {
			start++
		}
		end := 0
		for end < len(ae)-start && end < len(be)-start && bytes.Equal(ae[len(ae)-1-end], be[len(be)-1-end]) {
			end++
		}
		if len(ae) == len(be) {
			out := []byte(`{"i":[`)
			for i := start; i < len(ae)-end; i++ {
				p, err := diffValues(ae[i], be[i], ordered)
				if err != nil {
					return nil, err
				}
				if p == nil {
					continue
				}
				if out[len(out)-1] != '[' {
					out = append(out, ',')
				}
				out = fmt.Appendf(out, "[%d,%s]", i, p)
			}
			return append(out, "]}"...), nil
		}
		return jsontext.Value(fmt.Sprintf(`{"s":[%d,%d,%s]}`, start, len(ae)-start-end, arrayOf(be[start:len(be)-end]))), nil
	case '"':
		if len(b) > stringSpliceAbove {
			return diffString(a, b)
		}
	}
	return replace(b), nil
}

// A string shorter than this is sent whole when it changes.
const stringSpliceAbove = 256

// diffString is the patch that makes string a into string b, as a splice if that's smaller.
func diffString(a, b jsontext.Value) (jsontext.Value, error) {
	var as, bs string
	if err := json.Unmarshal(a, &as); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &bs); err != nil {
		return nil, err
	}
	start := 0
	for start < len(as) && start < len(bs) && as[start] == bs[start] {
		start++
	}
	for start > 0 && start < len(as) && !utf8.RuneStart(as[start]) {
		start--
	}
	end := 0
	for end < len(as)-start && end < len(bs)-start && as[len(as)-1-end] == bs[len(bs)-1-end] {
		end++
	}
	for end > 0 && !utf8.RuneStart(as[len(as)-end]) {
		end--
	}
	insert, err := json.Marshal(bs[start : len(bs)-end])
	if err != nil {
		return nil, err
	}
	if len(insert) >= len(b) {
		return replace(b), nil
	}
	return jsontext.Value(fmt.Sprintf(`{"t":[%d,%d,%s]}`, utf16Len(as[:start]), utf16Len(as[start:len(as)-end]), insert)), nil
}

// utf16Len is how many UTF-16 code units s is.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// utf16At is the byte offset of UTF-16 code unit n of s, or -1 if it's past the end or in the
// middle of a character.
func utf16At(s string, n int) int {
	units := 0
	for i, r := range s {
		if units == n {
			return i
		}
		if units > n {
			return -1
		}
		units += utf16.RuneLen(r)
	}
	if units == n {
		return len(s)
	}
	return -1
}

// applyJSON makes v into what patch says, keeping the order of an object's keys.
func applyJSON(v jsontext.Value, patch jsontext.Value) (jsontext.Value, error) {
	if len(patch) == 0 || bytes.Equal(patch, []byte("null")) {
		return v, nil
	}
	ps, err := members(patch)
	if err != nil || len(ps) == 0 {
		return nil, errors.New("bad patch")
	}
	switch ps[0].name {
	case "=":
		if len(ps) != 1 {
			return nil, errors.New("bad patch")
		}
		return ps[0].value, nil
	case "o":
		ms, err := members(v)
		if err != nil {
			return nil, err
		}
		changes, err := members(ps[0].value)
		if err != nil {
			return nil, err
		}
		if len(ps) > 1 {
			var gone []string
			if ps[1].name != "d" || len(ps) > 2 || json.Unmarshal(ps[1].value, &gone) != nil {
				return nil, errors.New("bad patch")
			}
			for _, name := range gone {
				for i, m := range ms {
					if m.name == name {
						ms = append(ms[:i], ms[i+1:]...)
						break
					}
				}
			}
		}
	next:
		for _, c := range changes {
			for i, m := range ms {
				if m.name == c.name {
					if ms[i].value, err = applyJSON(m.value, c.value); err != nil {
						return nil, err
					}
					continue next
				}
			}
			value, err := applyJSON(nil, c.value)
			if err != nil {
				return nil, err
			}
			ms = append(ms, member{c.name, value})
		}
		return objectOf(ms), nil
	case "t":
		var t [3]jsontext.Value
		var str, insert string
		var at, n int
		if len(ps) != 1 || json.Unmarshal(ps[0].value, &t) != nil || json.Unmarshal(v, &str) != nil || json.Unmarshal(t[0], &at) != nil ||
			json.Unmarshal(t[1], &n) != nil || json.Unmarshal(t[2], &insert) != nil || at < 0 || n < 0 {
			return nil, errors.New("bad patch")
		}
		from, to := utf16At(str, at), utf16At(str, at+n)
		if from < 0 || to < 0 {
			return nil, errors.New("bad patch")
		}
		return json.Marshal(str[:from] + insert + str[to:])
	case "i", "s":
		if len(ps) != 1 {
			return nil, errors.New("bad patch")
		}
		es, err := elements(v)
		if err != nil {
			return nil, err
		}
		if ps[0].name == "i" {
			var changes [][2]jsontext.Value
			if err := json.Unmarshal(ps[0].value, &changes); err != nil {
				return nil, errors.New("bad patch")
			}
			for _, c := range changes {
				var n int
				if json.Unmarshal(c[0], &n) != nil || n < 0 || n >= len(es) {
					return nil, errors.New("bad patch")
				}
				if es[n], err = applyJSON(es[n], c[1]); err != nil {
					return nil, err
				}
			}
			return arrayOf(es), nil
		}
		var s [3]jsontext.Value
		var at, n int
		if json.Unmarshal(ps[0].value, &s) != nil || json.Unmarshal(s[0], &at) != nil || json.Unmarshal(s[1], &n) != nil ||
			at < 0 || n < 0 || at+n > len(es) {
			return nil, errors.New("bad patch")
		}
		insert, err := elements(s[2])
		if err != nil {
			return nil, errors.New("bad patch")
		}
		out := append(append(append([]jsontext.Value{}, es[:at]...), insert...), es[at+n:]...)
		return arrayOf(out), nil
	}
	return nil, errors.New("bad patch")
}
