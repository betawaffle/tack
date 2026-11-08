// Package pyenv makes the Python packages code can import, beyond the standard library. They're
// listed in python.lock, every dependency included, each a wheel with its SHA-256. Building an
// environment downloads each wheel, checks it, and unpacks it: nothing in a package runs on this
// machine, only in the sandbox. An environment is named by the lock's hash and never changes
// once it's built, so what a run can import is exactly what the lock says.
package pyenv

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

//go:embed python.lock
var defaultLock []byte

// Limits on what a build will download and unpack.
const (
	maxWheelBytes    = 512 << 20
	maxUnpackedBytes = 2 << 30
)

// Package is one pinned wheel.
type Package struct {
	Name    string
	Version string
	URL     string
	SHA256  string
}

// Lock is what an environment is built from.
type Lock struct {
	// Python is the version the wheels are for, as major.minor.
	Python   string
	Packages []Package
	// Hash identifies the lock, and so the environment built from it.
	Hash string
}

// Default is tack's own lock.
func Default() Lock {
	lock, err := Parse(defaultLock)
	if err != nil {
		panic(err)
	}
	return lock
}

// Parse reads a lock: a "python X.Y" line, then a line per package of its name, version, URL and
// sha256=hex. Blank lines and lines starting with # are left out.
func Parse(data []byte) (Lock, error) {
	sum := sha256.Sum256(data)
	lock := Lock{Hash: hex.EncodeToString(sum[:])}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) == 2 && fields[0] == "python":
			lock.Python = fields[1]
		case len(fields) == 4 && strings.HasPrefix(fields[3], "sha256="):
			p := Package{Name: fields[0], Version: fields[1], URL: fields[2], SHA256: strings.TrimPrefix(fields[3], "sha256=")}
			if !strings.HasPrefix(p.URL, "https://") || len(p.SHA256) != 64 {
				return Lock{}, fmt.Errorf("line %d: a package needs an https URL and a SHA-256", n)
			}
			lock.Packages = append(lock.Packages, p)
		default:
			return Lock{}, fmt.Errorf("line %d isn't a python or package line", n)
		}
	}
	if lock.Python == "" {
		return Lock{}, errors.New("the lock doesn't say which Python it's for")
	}
	return lock, scanner.Err()
}

// Dir is where the environment for the lock is, under root. Its packages are in Site.
func (l Lock) Dir(root string) string { return filepath.Join(root, l.Hash[:16]) }

// Site is the directory to put on Python's path.
func Site(dir string) string { return filepath.Join(dir, "site") }

// Built says whether the environment exists.
func (l Lock) Built(root string) bool {
	info, err := os.Stat(Site(l.Dir(root)))
	return err == nil && info.IsDir()
}

// Build makes the environment if it isn't there yet. It's built beside where it goes and
// renamed into place when it's whole, then made read-only.
func Build(ctx context.Context, client *http.Client, l Lock, root string) error {
	if l.Built(root) {
		return nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(root, ".building-")
	if err != nil {
		return err
	}
	defer func() {
		// It's read-only only once it's done, so anything left here can still be removed.
		os.RemoveAll(staging)
	}()
	site := Site(staging)
	if err := os.Mkdir(site, 0o755); err != nil {
		return err
	}
	unpacked := int64(0)
	for _, p := range l.Packages {
		wheel, err := download(ctx, client, p, staging)
		if err != nil {
			return fmt.Errorf("%s %s: %w", p.Name, p.Version, err)
		}
		n, err := unpack(wheel, site, maxUnpackedBytes-unpacked)
		os.Remove(wheel)
		if err != nil {
			return fmt.Errorf("%s %s: %w", p.Name, p.Version, err)
		}
		unpacked += n
	}
	if err := readOnly(staging); err != nil {
		return err
	}
	return os.Rename(staging, l.Dir(root))
}

func download(ctx context.Context, client *http.Client, p Package, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tack")
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading it: %s", res.Status)
	}
	f, err := os.CreateTemp(dir, "wheel-*.whl")
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(res.Body, maxWheelBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxWheelBytes {
		err = fmt.Errorf("it's over %d bytes", maxWheelBytes)
	}
	if err == nil && hex.EncodeToString(hash.Sum(nil)) != p.SHA256 {
		err = errors.New("its SHA-256 isn't the one in the lock")
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// dataDir is a wheel's .data directory, whose purelib and platlib go in site like the rest.
var dataDir = regexp.MustCompile(`^[^/]+\.data/(purelib|platlib|scripts|headers|data)/(.*)$`)

// unpack puts a wheel's files in site, refusing anything that isn't a plain file at a plain
// path inside it. Scripts, headers and data aren't needed to import anything, so they're left out.
func unpack(wheel, site string, room int64) (int64, error) {
	r, err := zip.OpenReader(wheel)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	total := int64(0)
	for _, f := range r.File {
		name := f.Name
		if strings.HasSuffix(name, "/") {
			continue
		}
		if !fs.ValidPath(name) || strings.Contains(name, "\\") || !f.Mode().IsRegular() {
			return total, fmt.Errorf("it has %q, which isn't a plain file", name)
		}
		if m := dataDir.FindStringSubmatch(name); m != nil {
			if m[1] != "purelib" && m[1] != "platlib" {
				continue
			}
			name = m[2]
		}
		if total += int64(f.UncompressedSize64); total > room {
			return total, errors.New("it unpacks to too much")
		}
		full := filepath.Join(site, filepath.FromSlash(path.Clean(name)))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return total, err
		}
		in, err := f.Open()
		if err != nil {
			return total, err
		}
		out, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			in.Close()
			return total, err
		}
		_, err = io.Copy(out, io.LimitReader(in, int64(f.UncompressedSize64)+1))
		in.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func readOnly(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		return os.Chmod(p, 0o444)
	})
	if err != nil {
		return err
	}
	// Directories last, deepest first, since a read-only directory can't be changed.
	for i := len(dirs) - 1; i >= 0; i-- {
		if dirs[i] == root {
			continue
		}
		if err := os.Chmod(dirs[i], 0o555); err != nil {
			return err
		}
	}
	return nil
}

// Normalize is a distribution's name as Python compares them: lower case, with every run of
// -, _ and . as one -.
func Normalize(name string) string {
	return strings.Trim(separators.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

var separators = regexp.MustCompile(`[-_.]+`)

// Installed lists what's in a site directory, from its .dist-info directories: version by
// normalized name.
func Installed(site string) (map[string]string, error) {
	entries, err := os.ReadDir(site)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		base, ok := strings.CutSuffix(e.Name(), ".dist-info")
		if !ok || !e.IsDir() {
			continue
		}
		// A wheel's dist-info is name-version, with any - in the name already made _.
		i := strings.LastIndex(base, "-")
		if i <= 0 {
			continue
		}
		out[Normalize(base[:i])] = base[i+1:]
	}
	return out, nil
}
