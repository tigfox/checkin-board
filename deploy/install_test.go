// Package deploy holds the node installer; these tests run install.sh's
// option handling, which finishes before it touches the system.
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runInstall(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"install.sh"}, args...)...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, string(out)
}

func TestHelpMentionsGraywolfDeb(t *testing.T) {
	code, out := runInstall(t, "--help")
	if code != 0 || !strings.Contains(out, "--graywolf-deb") {
		t.Fatalf("--help = %d %q", code, out)
	}
}

func TestGraywolfDebIsValidatedFirst(t *testing.T) {
	dir := t.TempDir()
	notDeb := filepath.Join(dir, "graywolf.txt")
	deb := filepath.Join(dir, "graywolf_0.14.14+4978244d.armv6buf_armhf.deb")
	for _, f := range []string{notDeb, deb} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--graywolf-deb"}, "needs a .deb file"},
		{[]string{"--graywolf-deb", filepath.Join(dir, "missing.deb")}, "not found"},
		{[]string{"--graywolf-deb=" + notDeb}, "isn't a .deb"},
		{[]string{"--graywolf-deb", deb, "--no-graywolf"}, "can't be combined"},
		{[]string{"--graywolf-deb", deb, "--graywolf-version", "v0.14.14"}, "can't be combined"},
	}
	for _, c := range cases {
		code, out := runInstall(t, c.args...)
		if code != 2 || !strings.Contains(out, c.want) {
			t.Errorf("install.sh %v = %d %q, want exit 2 with %q", c.args, code, out, c.want)
		}
	}
}

func TestRaceConfigOptionValidatedFirst(t *testing.T) {
	dir := t.TempDir()
	txt := filepath.Join(dir, "race.txt")
	spaced := filepath.Join(dir, "my race.json")
	for _, f := range []string{txt, spaced} {
		if err := os.WriteFile(f, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--race-config"}, "needs a .json file"},
		{[]string{"--race-config", filepath.Join(dir, "missing.json")}, "not found"},
		{[]string{"--race-config=" + txt}, "isn't a .json"},
		{[]string{"--race-config", spaced}, "letters, digits"},
	}
	for _, c := range cases {
		code, out := runInstall(t, c.args...)
		if code != 2 || !strings.Contains(out, c.want) {
			t.Errorf("install.sh %v = %d %q, want exit 2 with %q", c.args, code, out, c.want)
		}
	}
}
