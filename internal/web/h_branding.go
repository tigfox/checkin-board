package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"checkin-board/internal/branding"
	"checkin-board/internal/store"
)

type brandingView struct {
	HeaderText string          `json:"header_text"`
	FooterText string          `json:"footer_text"`
	Colors     branding.Colors `json:"colors"`
	HasLogo    bool            `json:"has_logo"`
	LogoSHA256 string          `json:"logo_sha256,omitempty"`
	LogoWidth  int             `json:"logo_width,omitempty"`
	LogoHeight int             `json:"logo_height,omitempty"`
}

// effectiveBranding is what the board renders: defaults filled in and
// the race name as the header when none is set.
func (s *server) effectiveBranding(r *http.Request) (brandingView, error) {
	row, err := s.Store.Branding(r.Context())
	if err != nil {
		return brandingView{}, err
	}
	b := branding.FromRow(row)
	v := brandingView{HeaderText: b.HeaderText, FooterText: b.FooterText, Colors: b.Colors.Effective()}
	if v.HeaderText == "" {
		cfg, err := s.Store.GetSettings(r.Context())
		if err != nil {
			return brandingView{}, err
		}
		v.HeaderText = cfg.RaceName
	}
	logo, err := s.Store.Logo(r.Context())
	switch {
	case err == nil:
		v.HasLogo, v.LogoSHA256, v.LogoWidth, v.LogoHeight = true, logo.SHA256, logo.Width, logo.Height
	case !errors.Is(err, store.ErrNotFound):
		return brandingView{}, err
	}
	return v, nil
}

func (s *server) getBranding(w http.ResponseWriter, r *http.Request) {
	v, err := s.effectiveBranding(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// getLogo serves the stored (re-encoded) PNG, with its stored type and
// an ETag, never anything the uploader sent.
func (s *server) getLogo(w http.ResponseWriter, r *http.Request) {
	logo, err := s.Store.Logo(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	etag := `"` + logo.SHA256 + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=300")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", logo.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(logo.Image)))
	_, _ = w.Write(logo.Image)
}

// getAdminBranding returns the stored (unfilled) settings for the editor
// plus their contrast readout.
func (s *server) getAdminBranding(w http.ResponseWriter, r *http.Request) {
	row, err := s.Store.Branding(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	settings := branding.FromRow(row)
	_, contrasts, _ := branding.Check(settings)
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings, "defaults": branding.Default, "contrasts": contrasts})
}

// postBrandingCheck validates without saving, for the editor's live
// contrast readout.
func (s *server) postBrandingCheck(w http.ResponseWriter, r *http.Request) {
	var b branding.Settings
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	settings, contrasts, err := branding.Check(b)
	resp := map[string]any{"ok": err == nil, "contrasts": contrasts}
	var ve *branding.ValidationError
	if errors.As(err, &ve) {
		resp["problems"] = ve.Problems
	} else if err != nil {
		writeError(w, r, s.log, err)
		return
	} else {
		resp["settings"] = settings
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) putBranding(w http.ResponseWriter, r *http.Request) {
	var b branding.Settings
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	settings, contrasts, err := branding.Check(b)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if _, err := s.Store.SaveBranding(r.Context(), settings.ToRow()); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings, "contrasts": contrasts})
}

func (s *server) putLogo(w http.ResponseWriter, r *http.Request) {
	f, err := uploadedFile(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	defer f.Close()
	logo, err := branding.ProcessLogo(f)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if err := s.Store.SaveLogo(r.Context(), store.LogoRow{Image: logo.PNG, ContentType: "image/png",
		Width: logo.Width, Height: logo.Height, SHA256: logo.SHA256}); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"width": logo.Width, "height": logo.Height, "sha256": logo.SHA256,
		"updated_at": s.now().UTC().Truncate(time.Second)})
}

func (s *server) deleteLogo(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteLogo(r.Context()); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
