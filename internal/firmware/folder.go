// Package firmware keeps UV-K5/UV-K1 firmware images on the node
// (/var/lib/checkin-board/firmware), for flashing the radio plugged into
// the Pi: copied in by hand, by install.sh --firmware, or uploaded on the
// Station page.
package firmware

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"checkin-board/internal/uvk5"
)

// MaxFiles caps the folder, so uploads can't fill the SD card.
const MaxFiles = 20

var (
	// ErrExists: a file of that name is already there.
	ErrExists = errors.New("firmware: a file of that name is already on this node")
	// ErrInvalid wraps a bad name or image.
	ErrInvalid = errors.New("firmware")
)

// nameRe: a file name only (no path), ending .bin.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}\.bin$`)

// Folder is the node's firmware files.
type Folder struct {
	dir string

	mu   sync.Mutex
	sums map[string]cachedSum // by name: the page polls the list during a flash
}

type cachedSum struct {
	size          int64
	mod           time.Time
	sum           string
	name, version string
}

// NewFolder returns the folder at dir (created on first save).
func NewFolder(dir string) *Folder { return &Folder{dir: dir, sums: map[string]cachedSum{}} }

// Entry is one firmware file.
type Entry struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	SHA256   string    `json:"sha256"`
	// Name and Version suggest what to call it in a slot (from the file
	// name, as UV Studio does).
	SlotName    string `json:"slot_name,omitempty"`
	SlotVersion string `json:"slot_version,omitempty"`
	Error       string `json:"error,omitempty"` // why it can't go in a slot
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func (fo *Folder) path(name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", invalid("file name %q: use letters, digits, '.', '-' or '_', ending in .bin", name)
	}
	return filepath.Join(fo.dir, name), nil
}

// List returns the folder's files by name.
func (fo *Folder) List() ([]Entry, error) {
	des, err := os.ReadDir(fo.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, de := range des {
		if !de.Type().IsRegular() || !nameRe.MatchString(de.Name()) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		e := Entry{Name: de.Name(), Size: info.Size(), Modified: info.ModTime().UTC()}
		if c, ok := fo.cached(e); ok {
			e.SHA256, e.SlotName, e.SlotVersion = c.sum, c.name, c.version
		} else if data, err := fo.Read(de.Name()); err != nil {
			e.Error = err.Error()
		} else {
			raw := sha256.Sum256(data)
			e.SHA256 = hex.EncodeToString(raw[:])
			e.SlotName, e.SlotVersion = uvk5.SlotMeta(de.Name(), data)
			fo.remember(e)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Read returns one image, checked.
func (fo *Folder) Read(name string) ([]byte, error) {
	p, err := fo.path(name)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(p) // a regular file only: no symlinks or FIFOs
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, invalid("%s isn't a regular file", name)
	}
	fh, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, uvk5.MaxSlotImage+1))
	if err != nil {
		return nil, err
	}
	if err := uvk5.ValidSlotImage(data); err != nil {
		return nil, invalid("%s: %v", name, err)
	}
	return data, nil
}

// Save writes data as name after checking it; an existing file is
// replaced only with replace.
func (fo *Folder) Save(name string, data []byte, replace bool) error {
	p, err := fo.path(name)
	if err != nil {
		return err
	}
	if err := uvk5.ValidSlotImage(data); err != nil {
		return invalid("%v", err)
	}
	if err := os.MkdirAll(fo.dir, 0o750); err != nil {
		return err
	}
	switch _, err := os.Lstat(p); {
	case err == nil && !replace:
		return ErrExists
	case errors.Is(err, os.ErrNotExist):
		if list, _ := fo.List(); len(list) >= MaxFiles {
			return invalid("the folder already has %d files; delete some first", MaxFiles)
		}
	}
	tmp, err := os.CreateTemp(fo.dir, ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Delete removes one file.
func (fo *Folder) Delete(name string) error {
	p, err := fo.path(name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// SafeName turns an uploaded file's name into a folder name ("" when
// nothing usable is left).
func SafeName(upload string) string {
	base := filepath.Base(strings.ReplaceAll(upload, "\\", "/"))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	name := strings.TrimLeft(b.String(), ".-_")
	if len(name) > 75 {
		name = name[:75]
	}
	if name == "" {
		return ""
	}
	return name + ".bin"
}

// cached is what was worked out about the file, if it hasn't changed.
func (fo *Folder) cached(e Entry) (cachedSum, bool) {
	fo.mu.Lock()
	defer fo.mu.Unlock()
	c, ok := fo.sums[e.Name]
	if !ok || c.size != e.Size || !c.mod.Equal(e.Modified) {
		return cachedSum{}, false
	}
	return c, true
}

func (fo *Folder) remember(e Entry) {
	fo.mu.Lock()
	defer fo.mu.Unlock()
	fo.sums[e.Name] = cachedSum{size: e.Size, mod: e.Modified, sum: e.SHA256, name: e.SlotName, version: e.SlotVersion}
}
