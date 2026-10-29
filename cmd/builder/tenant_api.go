package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
)

// The tenant API is /new and a board's member list without a browser:
// bearer ADMIN_TOKEN, acting as the first ADMIN_LOGIN. Unrouted without one.
func (h *hub) apiRoutes(mux *http.ServeMux) {
	if h.cfg.AdminToken == "" {
		return
	}
	api := func(f http.HandlerFunc) http.Handler { return auth.Bearer(h.cfg.AdminToken, f) }
	mux.Handle("POST /api/tenants", api(h.apiCreate))
	mux.Handle("GET /api/tenants/{slug}", api(h.apiTenant))
	mux.Handle("POST /api/tenants/{slug}/members", api(h.apiAddMember))
}

// apiActor is who API calls act as: the first platform admin from ADMIN_LOGIN.
func (h *hub) apiActor() string {
	login, _, _ := strings.Cut(h.cfg.AdminLogin, ",")
	return strings.TrimSpace(login)
}

func apiError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// apiCreate takes a newTenant as JSON. 201 with the tenant; 400 when nothing
// was created; 502 when it was registered but a later step failed.
func (h *hub) apiCreate(w http.ResponseWriter, r *http.Request) {
	var n newTenant
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&n); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := h.createTenant(r.Context(), h.apiActor(), n); err != nil {
		code := http.StatusBadRequest
		if errors.As(err, &errPartial{}) {
			code = http.StatusBadGateway
		}
		apiError(w, code, err)
		return
	}
	h.writeTenant(w, r, strings.TrimSpace(n.Slug), http.StatusCreated)
}

func (h *hub) apiTenant(w http.ResponseWriter, r *http.Request) {
	h.writeTenant(w, r, r.PathValue("slug"), http.StatusOK)
}

func (h *hub) writeTenant(w http.ResponseWriter, r *http.Request, slug string, code int) {
	t, err := h.reg.Tenant(r.Context(), slug)
	if err != nil {
		apiError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, code, map[string]any{
		"slug": t.Slug, "name": t.Name, "repo": t.Repo, "status": t.Status, "existing": t.Existing,
		"error": t.Error, "steps": t.Steps, "board": h.cfg.BaseURL + "/" + t.Slug + "/", "app": h.appURL(t.Slug),
	})
}

// apiAddMember adds a member to a board or changes their role, like its
// /admin form: {"login", "role" ("member" or "admin"), "email", "github"}.
// With an email they sign in with Google (and GitHub too if "github"). It
// never invites or welcomes them.
func (h *hub) apiAddMember(w http.ResponseWriter, r *http.Request) {
	var m struct {
		Login  string `json:"login"`
		Role   string `json:"role"`
		Email  string `json:"email"`
		GitHub bool   `json:"github"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&m); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	login, email := strings.TrimPrefix(strings.TrimSpace(m.Login), "@"), strings.TrimSpace(m.Email)
	if login == "" && email != "" {
		_, a := ownerOf(email)
		login = a.Login
	}
	switch {
	case login == "" || (m.Role != "admin" && m.Role != "member"):
		apiError(w, http.StatusBadRequest, errors.New(`want "login" or "email", and "role" "member" or "admin"`))
		return
	case email != "" && !strings.Contains(email, "@"):
		apiError(w, http.StatusBadRequest, errors.New("that email doesn't look right"))
		return
	}
	ctx := r.Context()
	st, err := h.reg.Store(ctx, r.PathValue("slug"))
	if err != nil {
		apiError(w, http.StatusNotFound, err)
		return
	}
	if err := st.UpsertMember(ctx, login, m.Role, h.apiActor()); err != nil {
		apiError(w, http.StatusInternalServerError, err)
		return
	}
	if email != "" {
		if err := st.SetMemberEmail(ctx, login, email); errors.Is(err, db.ErrEmailTaken) {
			apiError(w, http.StatusConflict, err)
			return
		} else if err != nil {
			apiError(w, http.StatusInternalServerError, err)
			return
		}
		if err := st.SetMemberGitHub(ctx, login, m.GitHub); err != nil {
			apiError(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"login": login, "role": m.Role, "email": email, "github": email == "" || m.GitHub})
}
