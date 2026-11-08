package workspace

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	packed := gzip.NewWriter(&b)
	w := tar.NewWriter(packed)
	w.WriteHeader(&tar.Header{Name: "dir", Typeflag: tar.TypeDir, Mode: 0o755})
	w.WriteHeader(&tar.Header{Name: "dir/link", Typeflag: tar.TypeSymlink, Linkname: "../a.txt"})
	for _, name := range []string{"a.txt", "dir/b.txt"} {
		text := files[name]
		w.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(text)), ModTime: time.Unix(1000, 0)})
		w.Write([]byte(text))
	}
	w.Close()
	packed.Close()
	return b.Bytes()
}

func TestSplitAndJoinKeepEverythingAndShareUnchangedFiles(t *testing.T) {
	blobs := map[string][]byte{}
	put := func(data []byte) (string, error) {
		sum := sha256.Sum256(data)
		ref := hex.EncodeToString(sum[:])
		blobs[ref] = data
		return ref, nil
	}
	get := func(ref string) ([]byte, error) { return blobs[ref], nil }
	first, err := Split(archive(t, map[string]string{"a.txt": "same", "dir/b.txt": "one"}), 1<<20, put)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Split(archive(t, map[string]string{"a.txt": "same", "dir/b.txt": "two"}), 1<<20, put); err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 3 {
		t.Errorf("%d blobs, but an unchanged file should be kept once", len(blobs))
	}
	// Joining and splitting again gives the same manifest, so nothing's lost on the way.
	joined, err := Join(first, get)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Split(joined, 1<<20, put)
	if err != nil || !bytes.Equal(again, first) {
		t.Errorf("again = %s, first = %s, %v", again, first, err)
	}
	if again2, _ := Join(again, get); !bytes.Equal(again2, joined) {
		t.Error("the same manifest made a different archive")
	}
	if _, err := Split(archive(t, map[string]string{"a.txt": "too big"}), 3, put); err == nil {
		t.Error("it went past the limit")
	}
}
