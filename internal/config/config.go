// Package config loads bootstrap settings from the environment. Race
// settings (role, codes, tuning) live in the app database instead.
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	DefaultGraywolfURL = "http://localhost:8080"
	DefaultTimeout     = 10 * time.Second

	EnvGraywolfURL      = "GW_BASE_URL"
	EnvGraywolfUser     = "GW_USER"
	EnvGraywolfPassword = "GW_PASSWORD"
	EnvGraywolfPassFile = "GW_PASSWORD_FILE"
	EnvTimeout          = "GW_TIMEOUT"

	maxPasswordFileSize = 4 << 10
)

// Config holds bootstrap settings for reaching graywolf.
type Config struct {
	GraywolfURL      string
	GraywolfUser     string
	GraywolfPassword string
	Timeout          time.Duration
}

// String never includes the password.
func (c Config) String() string {
	return fmt.Sprintf("graywolf=%s user=%s timeout=%s", c.GraywolfURL, c.GraywolfUser, c.Timeout)
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
	return Config{GraywolfURL: gwURL, GraywolfUser: user, GraywolfPassword: pass, Timeout: timeout}, nil
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
	// Stat the open handle, not the path, so the checked file is the
	// one that gets read.
	f, err := env.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", EnvGraywolfPassFile, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("%s: %w", EnvGraywolfPassFile, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s %s is not a regular file", EnvGraywolfPassFile, path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s %s has mode %o; it must not be readable by group or others (chmod 600)", EnvGraywolfPassFile, path, info.Mode().Perm())
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxPasswordFileSize+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", EnvGraywolfPassFile, err)
	}
	if len(raw) > maxPasswordFileSize {
		return "", fmt.Errorf("%s %s is too large", EnvGraywolfPassFile, path)
	}
	pass := strings.TrimRight(string(raw), "\r\n")
	if pass == "" {
		return "", fmt.Errorf("%s %s is empty", EnvGraywolfPassFile, path)
	}
	return pass, nil
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
