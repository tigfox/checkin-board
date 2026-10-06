package web

import (
	"net/http"

	"checkin-board/internal/store"
)

type passwordBody struct {
	Password string `json:"password"`
}

type loginBody struct {
	Role     string `json:"role"`
	Password string `json:"password"`
}

type roleBody struct {
	Role string `json:"role"`
}

func (s *server) getSetup(w http.ResponseWriter, r *http.Request) {
	st, err := s.Auth.Status(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"needs_setup": st.NeedsSetup, "volunteer_set": st.VolunteerSet})
}

// postSetup sets the first admin password and logs the admin in. It
// only works while no admin password exists.
func (s *server) postSetup(w http.ResponseWriter, r *http.Request) {
	var b passwordBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	token, err := s.Auth.Setup(r.Context(), b.Password)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	setSessionCookie(w, r, token, store.RoleAdmin)
	writeJSON(w, http.StatusCreated, roleBody{Role: store.RoleAdmin})
}

func (s *server) postLogin(w http.ResponseWriter, r *http.Request) {
	var b loginBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	token, err := s.Auth.Login(r.Context(), b.Role, b.Password, clientKey(r))
	if err != nil {
		if b.Role == store.RoleAdmin {
			s.log.Warn("web: admin login failed", "client", clientKey(r), "err", err)
		}
		writeError(w, r, s.log, err)
		return
	}
	setSessionCookie(w, r, token, b.Role)
	writeJSON(w, http.StatusOK, roleBody{Role: b.Role})
}

func (s *server) postLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.Logout(r.Context(), sessionToken(r)); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) getSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, roleBody{Role: roleOf(r)})
}

type adminPasswordBody struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

type newPasswordBody struct {
	New string `json:"new"`
}

// putAdminPassword changes the admin password; every admin session
// (including this one) ends.
func (s *server) putAdminPassword(w http.ResponseWriter, r *http.Request) {
	var b adminPasswordBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if err := s.Auth.ChangeAdminPassword(r.Context(), b.Current, b.New); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// putVolunteerPassword sets or changes the shared volunteer password;
// every volunteer session ends.
func (s *server) putVolunteerPassword(w http.ResponseWriter, r *http.Request) {
	var b newPasswordBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if err := s.Auth.SetVolunteerPassword(r.Context(), b.New); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
