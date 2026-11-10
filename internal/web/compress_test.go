package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompressGzipsWholeText(t *testing.T) {
	body := strings.Repeat(`{"text":"the same words again"}`, 200)
	h := compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	r := httptest.NewRequest("GET", "/api/x", nil)
	r.Header.Set("Accept-Encoding", "gzip, deflate, br")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("not gzipped: %v", rec.Header())
	}
	if rec.Body.Len() >= len(body)/4 {
		t.Errorf("gzipped to %d bytes of %d", rec.Body.Len(), len(body))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != body {
		t.Errorf("didn't come back the same")
	}
}

func TestCompressLeavesAlone(t *testing.T) {
	cases := []struct {
		name, accept, contentType string
	}{
		{"no gzip asked for", "", "application/json"},
		{"gzip turned off", "gzip;q=0", "application/json"},
		{"a stream", "gzip", "application/x-ndjson"},
		{"an image", "gzip", "image/png"},
	}
	for _, c := range cases {
		h := compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", c.contentType)
			io.WriteString(w, "line one\n")
			http.NewResponseController(w).Flush()
			io.WriteString(w, "line two\n")
		}))
		r := httptest.NewRequest("GET", "/api/x", nil)
		if c.accept != "" {
			r.Header.Set("Accept-Encoding", c.accept)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != "line one\nline two\n" {
			t.Errorf("%s: %v %q", c.name, rec.Header(), rec.Body.String())
		}
		if !rec.Flushed {
			t.Errorf("%s: flushing didn't reach the connection", c.name)
		}
	}
}

func TestStaticFilesHaveETags(t *testing.T) {
	h := compress(withETags(static, http.FileServerFS(static)))
	get := func(tag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/static/prompt.js", nil)
		r.Header.Set("Accept-Encoding", "gzip")
		if tag != "" {
			r.Header.Set("If-None-Match", tag)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	first := get("")
	tag := first.Header().Get("ETag")
	if first.Code != 200 || tag == "" || first.Header().Get("Content-Encoding") != "gzip" || first.Header().Get("Content-Length") != "" {
		t.Fatalf("first: %d %v", first.Code, first.Header())
	}
	if again := get(tag); again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Errorf("with its tag: %d, %d bytes", again.Code, again.Body.Len())
	}
	if other := get(`"something else"`); other.Code != 200 {
		t.Errorf("with another tag: %d", other.Code)
	}
}
