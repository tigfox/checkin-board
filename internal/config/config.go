// Package config loads bootstrap settings from the environment. Race
// settings (role, codes, tuning) live in the app database instead.
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	DefaultGraywolfURL = "http://localhost:8080"
	DefaultTimeout     = 10 * time.Second
	DefaultDBPath      = "checkin-board.db"
	DefaultListen      = ":8090"

	EnvGraywolfURL      = "GW_BASE_URL"
	EnvGraywolfUser     = "GW_USER"
	EnvGraywolfPassword = "GW_PASSWORD"
	EnvGraywolfPassFile = "GW_PASSWORD_FILE"
	EnvTimeout          = "GW_TIMEOUT"
	EnvDBPath           = "CB_DB_PATH"
	EnvListen           = "CB_LISTEN"
	// EnvHookTokenFile enables the local automation hook (the graywolf
	// Action that runs a link check); unset leaves it off.
	EnvHookTokenFile = "CB_HOOK_TOKEN_FILE"

	// MinHookTokenLen is the shortest accepted hook token.
	MinHookTokenLen = 24

	maxPasswordFileSize = 4 << 10
)

// Config holds bootstrap settings for reaching graywolf.
type Config struct {
	GraywolfURL      string
	GraywolfUser     string
	GraywolfPassword string
	Timeout          time.Duration
	DBPath           string
	Listen           string
	HookToken        string // "" = automation hook off
}

// String never includes the password.
func (c Config) String() string {
	hook := "off"
	if c.HookToken != "" {
		hook = "on"
	}
	return fmt.Sprintf("graywolf=%s user=%s timeout=%s db=%s listen=%s hook=%s", c.GraywolfURL, c.GraywolfUser, c.Timeout, c.DBPath, c.Listen, hook)
}

// Env abstracts the environment and filesystem so loading is testable.
type Env struct {
	Getenv func(string) string
	Open   func(string) (fs.File, error)
}

// OSEnv reads the real process environment and filesystem.
func OSEnv() Env {
	return Env{
		Getenv: os.Getenv,
		Open:   func(name string) (fs.File, error) { return os.Open(name) },
	}
}

// Load reads configuration from the real environment.
func Load() (Config, error) {
	return LoadFrom(OSEnv())
}

// LoadFrom reads configuration from env.
func LoadFrom(env Env) (Config, error) {
	gwURL := valueOr(env.Getenv(EnvGraywolfURL), DefaultGraywolfURL)
	if err := validateURL(gwURL); err != nil {
		return Config{}, err
	}
	timeout, err := parseTimeout(env.Getenv(EnvTimeout))
	if err != nil {
		return Config{}, err
	}
	user := strings.TrimSpace(env.Getenv(EnvGraywolfUser))
	if user == "" {
		return Config{}, fmt.Errorf("%s is required", EnvGraywolfUser)
	}
	pass, err := loadPassword(env)
	if err != nil {
		return Config{}, err
	}
	hook := ""
	if path := env.Getenv(EnvHookTokenFile); path != "" {
		if hook, err = readSecretFile(env, EnvHookTokenFile, path); err != nil {
			return Config{}, err
		}
		if len(hook) < MinHookTokenLen {
			return Config{}, fmt.Errorf("%s: the token must be at least %d characters", EnvHookTokenFile, MinHookTokenLen)
		}
	}
	return Config{
		GraywolfURL: gwURL, GraywolfUser: user, GraywolfPassword: pass, Timeout: timeout,
		DBPath:    DBPathFrom(env.Getenv),
		Listen:    valueOr(strings.TrimSpace(env.Getenv(EnvListen)), DefaultListen),
		HookToken: hook,
	}, nil
}

// DBPathFrom reads only the database path (for host commands that don't
// talk to graywolf, such as reset-admin-password).
func DBPathFrom(getenv func(string) string) string {
	return valueOr(strings.TrimSpace(getenv(EnvDBPath)), DefaultDBPath)
}

// loadPassword prefers GW_PASSWORD_FILE (which must not be readable by
// group or others) over GW_PASSWORD.
func loadPassword(env Env) (string, error) {
	path := env.Getenv(EnvGraywolfPassFile)
	if path == "" {
		if pass := env.Getenv(EnvGraywolfPassword); pass != "" {
			return pass, nil
		}
		return "", fmt.Errorf("%s or %s is required", EnvGraywolfPassword, EnvGraywolfPassFile)
	}
	return readSecretFile(env, EnvGraywolfPassFile, path)
}

// readSecretFile reads a one-line secret from a file that must not be
// readable by group or others.
func readSecretFile(env Env, name, path string) (string, error) {
	// Stat the open handle, not the path, so the checked file is the
	// one that gets read.
	f, err := env.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s %s is not a regular file", name, path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s %s has mode %o; it must not be readable by group or others (chmod 600)", name, path, info.Mode().Perm())
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxPasswordFileSize+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if len(raw) > maxPasswordFileSize {
		return "", fmt.Errorf("%s %s is too large", name, path)
	}
	secret := strings.TrimRight(string(raw), "\r\n")
	if secret == "" {
		return "", fmt.Errorf("%s %s is empty", name, path)
	}
	return secret, nil
}

func parseTimeout(raw string) (time.Duration, error) {
	if raw == "" {
		return DefaultTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", EnvTimeout, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid %s %q: must be positive", EnvTimeout, raw)
	}
	return d, nil
}

// validateURL never echoes raw: it might embed credentials.
func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid %s: not a valid URL", EnvGraywolfURL)
	}
	if u.User != nil {
		return errors.New("invalid " + EnvGraywolfURL + ": must not contain credentials; use " + EnvGraywolfUser + "/" + EnvGraywolfPassword)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid %s %s: scheme must be http or https", EnvGraywolfURL, u.Redacted())
	}
	if u.Host == "" {
		return fmt.Errorf("invalid %s %s: missing host", EnvGraywolfURL, u.Redacted())
	}
	return nil
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// Panel is what the node panel process needs: where the app is, and the
// local hook token (spec 8.4).
type Panel struct {
	AppURL string
	Token  string
}

// PanelFrom reads the panel's settings from the app's own environment
// file: CB_LISTEN (the app's address) and CB_HOOK_TOKEN_FILE.
func PanelFrom(env Env) (Panel, error) {
	path := env.Getenv(EnvHookTokenFile)
	if path == "" {
		return Panel{}, fmt.Errorf("%s is required for the panel (the app's local hook token)", EnvHookTokenFile)
	}
	tok, err := readSecretFile(env, EnvHookTokenFile, path)
	if err != nil {
		return Panel{}, err
	}
	listen := valueOr(strings.TrimSpace(env.Getenv(EnvListen)), DefaultListen)
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return Panel{}, fmt.Errorf("%s %q: %w", EnvListen, listen, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1" // the hook answers loopback only
	}
	return Panel{AppURL: "http://" + net.JoinHostPort(host, port), Token: tok}, nil
}
