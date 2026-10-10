package raceconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "race-configs")
	fo := NewFolder(dir)
	if list, err := fo.List(); err != nil || len(list) != 0 {
		t.Fatalf("empty folder = %v, %v", list, err)
	}
	if err := fo.Save("ridge-50k.json", []byte(sample), false); err != nil {
		t.Fatal(err)
	}
	list, err := fo.List()
	if err != nil || len(list) != 1 || list[0].Name != "ridge-50k.json" || list[0].RaceName != "Ridge 50K" || list[0].Error != "" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := fo.Save("ridge-50k.json", []byte(sample), false); !errors.Is(err, ErrExists) {
		t.Fatalf("save over an existing file err = %v, want ErrExists", err)
	}
	if err := fo.Save("ridge-50k.json", []byte(sample), true); err != nil {
		t.Fatalf("replace = %v", err)
	}
	f, err := fo.Open("ridge-50k.json")
	if err != nil || f.Race.Name != "Ridge 50K" {
		t.Fatalf("open = %+v, %v", f, err)
	}
	// A broken file copied in by hand is listed with its problem.
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, _ = fo.List()
	if len(list) != 2 || list[0].Name != "broken.json" || list[0].Error == "" {
		t.Fatalf("list with a broken file = %+v", list)
	}
	if err := fo.Delete("broken.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := fo.Open("broken.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file open err = %v", err)
	}
}

func TestFolderRefuses(t *testing.T) {
	fo := NewFolder(t.TempDir())
	for _, name := range []string{"../x.json", "a/b.json", "x.txt", ".json", "", strings.Repeat("a", 90) + ".json", "a b.json"} {
		if err := fo.Save(name, []byte(sample), false); !errors.Is(err, ErrInvalid) {
			t.Errorf("Save(%q) err = %v, want refused", name, err)
		}
		if _, err := fo.Open(name); !errors.Is(err, ErrInvalid) {
			t.Errorf("Open(%q) err = %v, want refused", name, err)
		}
		if err := fo.Delete(name); !errors.Is(err, ErrInvalid) {
			t.Errorf("Delete(%q) err = %v, want refused", name, err)
		}
	}
	if err := fo.Save("bad.json", []byte(`{"format":"nope"}`), false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid content saved: %v", err)
	}
	if list, _ := fo.List(); len(list) != 0 {
		t.Fatalf("an invalid upload was kept: %+v", list)
	}
	for i := range MaxFiles {
		if err := fo.Save(strings.Repeat("f", 1)+itoa(i)+".json", []byte(sample), false); err != nil {
			t.Fatal(err)
		}
	}
	if err := fo.Save("one-too-many.json", []byte(sample), false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over the file cap err = %v", err)
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"Ridge 50K.json": "Ridge-50K.json", `C:\Users\x\race.json`: "race.json", "../../etc/passwd": "passwd.json",
		"...json": "", "été 2026.json": "t-2026.json", "a.b.txt": "a.b.json",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
		if got := SafeName(in); got != "" && !nameRe.MatchString(got) {
			t.Errorf("SafeName(%q) = %q isn't a valid folder name", in, got)
		}
	}
}

// Symlinks and FIFOs in the folder are never followed or opened.
func TestFolderRegularFilesOnly(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	fo := NewFolder(dir)
	if list, _ := fo.List(); len(list) != 0 {
		t.Fatalf("list shows a symlink: %+v", list)
	}
	if _, err := fo.Open("link.json"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("open symlink err = %v", err)
	}
}
