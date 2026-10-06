// Package config loads application settings from the environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"time"
)

const (
	DefaultBaseURL = "http://localhost:8080"
	DefaultTimeout = 10 * time.Second

	EnvBaseURL = "API_BASE_URL"
	EnvTimeout = "API_TIMEOUT"
)

// Config holds runtime settings for talking to the local API.
type Config struct {
	BaseURL string
	Timeout time.Duration
}

// Load reads configuration using os.Getenv.
func Load() (Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom reads configuration using the supplied lookup function,
// which makes it easy to test without touching the real environment.
func LoadFrom(getenv func(string) string) (Config, error) {
	baseURL := valueOr(getenv(EnvBaseURL), DefaultBaseURL)
	if err := validateBaseURL(baseURL); err != nil {
		return Config{}, err
	}

	timeout := DefaultTimeout
	if raw := getenv(EnvTimeout); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid %s %q: %w", EnvTimeout, raw, err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("invalid %s %q: must be positive", EnvTimeout, raw)
		}
		timeout = d
	}

	return Config{BaseURL: baseURL, Timeout: timeout}, nil
}

func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", EnvBaseURL, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid %s %q: scheme must be http or https", EnvBaseURL, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid %s %q: missing host", EnvBaseURL, raw)
	}
	return nil
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
