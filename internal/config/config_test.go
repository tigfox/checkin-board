package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func envFrom(vars map[string]string) Env {
	env := OSEnv()
	env.Getenv = func(k string) string { return vars[k] }
	return env
}

func writeSecret(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gw-pass")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFrom(t *testing.T) {
	base := map[string]string{EnvGraywolfUser: "race", EnvGraywolfPassword: "pw"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	tests := []struct {
		name    string
		vars    map[string]string
		want    Config
		wantErr string
	}{
		{
			name: "defaults",
			vars: base,
			want: Config{GraywolfURL: DefaultGraywolfURL, GraywolfUser: "race", GraywolfPassword: "pw", Timeout: DefaultTimeout},
		},
		{
			name: "overrides",
			vars: with(map[string]string{EnvGraywolfURL: "http://10.0.0.2:8080", EnvTimeout: "3s"}),
			want: Config{GraywolfURL: "http://10.0.0.2:8080", GraywolfUser: "race", GraywolfPassword: "pw", Timeout: 3 * time.Second},
		},
		{name: "bad scheme", vars: with(map[string]string{EnvGraywolfURL: "ftp://x"}), wantErr: "scheme"},
		{name: "missing host", vars: with(map[string]string{EnvGraywolfURL: "http://"}), wantErr: "host"},
		{name: "credentials in url", vars: with(map[string]string{EnvGraywolfURL: "http://u:leak@x"}), wantErr: "credentials"},
		{name: "bad timeout", vars: with(map[string]string{EnvTimeout: "soon"}), wantErr: EnvTimeout},
		{name: "zero timeout", vars: with(map[string]string{EnvTimeout: "0s"}), wantErr: "positive"},
		{name: "missing user", vars: map[string]string{EnvGraywolfPassword: "pw"}, wantErr: EnvGraywolfUser},
		{name: "missing password", vars: map[string]string{EnvGraywolfUser: "race"}, wantErr: EnvGraywolfPassword},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LoadFrom(envFrom(tt.vars))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPasswordFile(t *testing.T) {
	good := writeSecret(t, "from-file\n", 0o600)
	cfg, err := LoadFrom(envFrom(map[string]string{
		EnvGraywolfUser: "race", EnvGraywolfPassword: "ignored", EnvGraywolfPassFile: good,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GraywolfPassword != "from-file" {
		t.Errorf("password = %q, want from-file (file wins, newline trimmed)", cfg.GraywolfPassword)
	}
}

func TestPasswordFileRejected(t *testing.T) {
	tests := []struct {
		name    string
		path    func(t *testing.T) string
		wantErr string
	}{
		{"group readable", func(t *testing.T) string { return writeSecret(t, "pw", 0o640) }, "chmod 600"},
		{"empty", func(t *testing.T) string { return writeSecret(t, "\n", 0o600) }, "empty"},
		{"too large", func(t *testing.T) string { return writeSecret(t, strings.Repeat("x", maxPasswordFileSize+1), 0o600) }, "too large"},
		{"missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope") }, EnvGraywolfPassFile},
		{"directory", func(t *testing.T) string {
			d := filepath.Join(t.TempDir(), "dir")
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			return d
		}, "regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadFrom(envFrom(map[string]string{EnvGraywolfUser: "race", EnvGraywolfPassFile: tt.path(t)}))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestPasswordFileOpenError(t *testing.T) {
	env := envFrom(map[string]string{EnvGraywolfUser: "race", EnvGraywolfPassFile: "/secret"})
	env.Open = func(string) (fs.File, error) { return nil, fs.ErrPermission }
	if _, err := LoadFrom(env); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v", err)
	}
}

func TestPasswordFileFromFS(t *testing.T) {
	mem := fstest.MapFS{"secret": {Data: []byte("mem-pass\r\n"), Mode: 0o600}}
	env := envFrom(map[string]string{EnvGraywolfUser: "race", EnvGraywolfPassFile: "secret"})
	env.Open = mem.Open
	cfg, err := LoadFrom(env)
	if err != nil || cfg.GraywolfPassword != "mem-pass" {
		t.Fatalf("cfg = %v, err = %v", cfg, err)
	}
}

func TestURLErrorsDoNotEchoCredentials(t *testing.T) {
	_, err := LoadFrom(envFrom(map[string]string{
		EnvGraywolfURL: "ftp://u:leak@x", EnvGraywolfUser: "race", EnvGraywolfPassword: "pw",
	}))
	if err == nil || strings.Contains(err.Error(), "leak") {
		t.Fatalf("err = %v", err)
	}
}

func TestStringHidesPassword(t *testing.T) {
	cfg := Config{GraywolfURL: "http://x", GraywolfUser: "race", GraywolfPassword: "hunter2", Timeout: time.Second}
	if strings.Contains(cfg.String(), "hunter2") {
		t.Errorf("String() leaks password: %s", cfg)
	}
}
