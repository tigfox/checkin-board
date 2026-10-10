package raceconfig

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// MaxFiles caps the folder, so uploads can't fill the SD card.
const MaxFiles = 50

// ErrExists: a file of that name is already in the folder (save with
// replace to overwrite it).
var ErrExists = errors.New("race config: a file of that name is already on this node")

// nameRe: a file name only (no path), so a name can't reach outside the
// folder.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}\.json$`)

// Folder is the node's race config files (/var/lib/checkin-board/
// race-configs): copied in by hand (scp, USB), by install.sh
// --race-config, or uploaded from the browser.
type Folder struct{ dir string }

// NewFolder returns the folder at dir (created on first save).
func NewFolder(dir string) *Folder { return &Folder{dir: dir} }

// Entry is one file in the folder.
type Entry struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	RaceName string    `json:"race_name,omitempty"`
	Stations int       `json:"stations"`
	// Error is why the file can't be used ("" when it can).
	Error string `json:"error,omitempty"`
}

func (fo *Folder) path(name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", invalid("file name %q: use letters, digits, '.', '-' or '_', ending in .json", name)
	}
	return filepath.Join(fo.dir, name), nil
}

// List returns the folder's files by name, each checked.
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
			continue // directories, symlinks, FIFOs: never opened
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		e := Entry{Name: de.Name(), Size: info.Size(), Modified: info.ModTime().UTC()}
		if f, err := fo.Open(de.Name()); err != nil {
			e.Error = err.Error()
		} else {
			e.RaceName, e.Stations = f.Race.Name, len(f.Stations())
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Open reads and parses one file.
func (fo *Folder) Open(name string) (*File, error) {
	p, err := fo.path(name)
	if err != nil {
		return nil, err
	}
	// A regular file only: a symlink or FIFO planted in the folder is
	// never followed (a FIFO would block the read).
	info, err := os.Lstat(p)
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
	return Parse(fh)
}

// Save writes data as name after checking it parses; an invalid upload
// is never kept. An existing file of that name is replaced only with
// replace (ErrExists otherwise).
func (fo *Folder) Save(name string, data []byte, replace bool) error {
	p, err := fo.path(name)
	if err != nil {
		return err
	}
	if _, err := Parse(bytes.NewReader(data)); err != nil {
		return err
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
	if err := os.Rename(tmp.Name(), p); err != nil {
		return fmt.Errorf("race config: save %s: %w", name, err)
	}
	return nil
}

// Delete removes one file.
func (fo *Folder) Delete(name string) error {
	p, err := fo.path(name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// SafeName turns an uploaded file's name into a folder name, or "" when
// nothing usable is left.
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
	return name + ".json"
}
