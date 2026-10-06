package config

import (
	"testing"
	"time"
)

func TestLoadFrom(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr bool
	}{
		{
			name: "defaults",
			env:  map[string]string{},
			want: Config{BaseURL: DefaultBaseURL, Timeout: DefaultTimeout},
		},
		{
			name: "overrides",
			env:  map[string]string{EnvBaseURL: "http://127.0.0.1:3000", EnvTimeout: "2s"},
			want: Config{BaseURL: "http://127.0.0.1:3000", Timeout: 2 * time.Second},
		},
		{name: "bad scheme", env: map[string]string{EnvBaseURL: "ftp://localhost"}, wantErr: true},
		{name: "missing host", env: map[string]string{EnvBaseURL: "http://"}, wantErr: true},
		{name: "bad timeout", env: map[string]string{EnvTimeout: "soon"}, wantErr: true},
		{name: "non-positive timeout", env: map[string]string{EnvTimeout: "0s"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LoadFrom(func(k string) string { return tt.env[k] })
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
