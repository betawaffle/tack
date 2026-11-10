package web

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
)

// compress gzips what's sent back, for a browser that takes it, when it's text that's sent
// all at once. A prompt is a lot of JSON, which is slow to send over a slow link and shrinks
// to about a quarter. What streams, like a reply as it comes in, is left alone, so each piece
// goes when it's written.
//
// Nothing tack sends back holds a secret, so compressing it can't leak one through its size.
func compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) || r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// acceptsGzip says whether an Accept-Encoding header takes gzip, not turned off with q=0.
func acceptsGzip(header string) bool {
	for _, item := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(item), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		q := strings.ReplaceAll(params, " ", "")
		return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
	}
	return false
}

// compressible is what's worth gzipping: text, sent whole.
func compressible(contentType string) bool {
	media, _, _ := mime.ParseMediaType(contentType)
	switch media {
	case "application/json", "text/javascript", "text/css", "text/html", "text/plain", "text/markdown", "image/svg+xml":
		return true
	}
	return false
}

var gzipWriters = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression)
	return w
}}

// gzipWriter decides when the headers go whether what follows is gzipped.
type gzipWriter struct {
	http.ResponseWriter
	decided bool
	gz      *gzip.Writer
}

func (g *gzipWriter) WriteHeader(status int) {
	if !g.decided {
		g.decided = true
		h := g.Header()
		if status != http.StatusNoContent && status != http.StatusNotModified && status != http.StatusPartialContent &&
			h.Get("Content-Encoding") == "" && compressible(h.Get("Content-Type")) {
			h.Del("Content-Length")
			h.Set("Content-Encoding", "gzip")
			g.gz = gzipWriters.Get().(*gzip.Writer)
			g.gz.Reset(g.ResponseWriter)
		}
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipWriter) Write(data []byte) (int, error) {
	if !g.decided {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(data))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		return g.gz.Write(data)
	}
	return g.ResponseWriter.Write(data)
}

// Flush sends what's been written so far, compressed or not.
func (g *gzipWriter) Flush() {
	if !g.decided {
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(g.ResponseWriter).Hijack()
}

// Unwrap lets http.ResponseController reach the connection's own writer.
func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) close() {
	if g.gz == nil {
		return
	}
	g.gz.Close()
	g.gz.Reset(nil)
	gzipWriters.Put(g.gz)
	g.gz = nil
}

// withETags serves files with an ETag of their contents, so a browser that has one already
// is told it hasn't changed rather than sent it again. The files are embedded, so they're
// hashed once.
func withETags(files fs.FS, next http.Handler) http.Handler {
	tags := map[string]string{}
	fs.WalkDir(files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(files, path)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		tags["/"+path] = `"` + hex.EncodeToString(sum[:12]) + `"`
		return nil
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tag, ok := tags[r.URL.Path]; ok {
			w.Header().Set("ETag", tag)
		}
		next.ServeHTTP(w, r)
	})
}
