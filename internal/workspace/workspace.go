// Package workspace keeps the files code leaves in /work as one blob per file and a manifest,
// rather than one archive per run, so a file that didn't change costs nothing to keep again.
// The sandbox still hands tack an archive and gets one back: what's in it is hostile, so tack
// only reads it as data, and never puts any of it on this machine's filesystem.
package workspace

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"time"
)

// Entry is one thing in /work. Path is relative to it.
type Entry struct {
	Path   string `json:"path"`
	Type   string `json:"type"` // file, dir or link
	Mode   int64  `json:"mode"`
	MTime  int64  `json:"mtime,omitzero"`
	Size   int64  `json:"size,omitzero"`
	Blob   string `json:"blob,omitzero"`   // a file's content
	Target string `json:"target,omitzero"` // a link's
}

// Split reads an archive from the sandbox into a manifest, putting each file's content with
// put. limit is the most it unpacks to.
func Split(archive []byte, limit int64, put func([]byte) (string, error)) ([]byte, error) {
	packed, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	r := tar.NewReader(packed)
	entries := []Entry{}
	total := int64(0)
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		e := Entry{Path: h.Name, Mode: h.Mode, MTime: h.ModTime.Unix()}
		switch h.Typeflag {
		case tar.TypeReg:
			if total += h.Size; total > limit {
				return nil, errors.New("it's too big")
			}
			data, err := io.ReadAll(io.LimitReader(r, h.Size+1))
			if err != nil {
				return nil, err
			}
			if int64(len(data)) != h.Size {
				return nil, fmt.Errorf("%s isn't the size it says", h.Name)
			}
			e.Type, e.Size = "file", h.Size
			if e.Blob, err = put(data); err != nil {
				return nil, err
			}
		case tar.TypeDir:
			e.Type = "dir"
		case tar.TypeSymlink:
			e.Type, e.Target, e.MTime = "link", h.Linkname, 0
		default:
			continue // The sandbox only keeps these three.
		}
		entries = append(entries, e)
	}
	// Reading to the end checks the checksum.
	if _, err := io.Copy(io.Discard, io.LimitReader(packed, 1<<20)); err != nil {
		return nil, err
	}
	return json.Marshal(entries, json.Deterministic(true))
}

// Join makes the archive the sandbox unpacks from a manifest, getting each file's content
// with get. The same manifest always makes the same archive.
func Join(manifest []byte, get func(string) ([]byte, error)) ([]byte, error) {
	var entries []Entry
	if err := json.Unmarshal(manifest, &entries); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	packed, err := gzip.NewWriterLevel(&out, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	w := tar.NewWriter(packed)
	for _, e := range entries {
		h := &tar.Header{Name: e.Path, Mode: e.Mode, ModTime: time.Unix(e.MTime, 0), Format: tar.FormatGNU}
		var data []byte
		switch e.Type {
		case "file":
			if data, err = get(e.Blob); err != nil {
				return nil, err
			}
			h.Typeflag, h.Size = tar.TypeReg, int64(len(data))
		case "dir":
			h.Typeflag = tar.TypeDir
		case "link":
			h.Typeflag, h.Linkname = tar.TypeSymlink, e.Target
		default:
			return nil, fmt.Errorf("%s has an unknown type %q", e.Path, e.Type)
		}
		if err := w.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := packed.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
