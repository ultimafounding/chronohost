package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackm/chronohost/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func (a *App) pageLogin(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, r, "login.html", map[string]any{"Next": r.URL.Query().Get("next")})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	u, err := a.Store.UserByUsername(r.Context(), username)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(r.FormValue("password"))) != nil {
		a.render(w, r, "login.html", map[string]any{"Next": r.FormValue("next"), "Error": "Invalid username or password."})
		return
	}
	token, err := randomToken(32)
	if err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	expires := time.Now().Add(24 * time.Hour)
	if err := a.Store.CreateSession(r.Context(), token, u.ID, expires); err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure:   r.TLS != nil || envOr("CHRONOHOST_COOKIE_SECURE", "false") == "true",
		SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: 86400,
	})
	next := r.FormValue("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	a.Store.Audit(r.Context(), u.Username, "login", "signed in")
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.Store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) apiListUsers(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleAdmin) == nil {
		return
	}
	users, err := a.Store.ListUsers(r.Context())
	if err != nil {
		a.fail(w, 500, err)
		return
	}
	a.json(w, 200, users)
}

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

func (a *App) apiCreateUser(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleAdmin) == nil {
		return
	}
	var req createUserRequest
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, 400, err)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if len(req.Username) < 2 || len(req.Username) > 32 || req.Password == "" {
		a.fail(w, 400, errBadRequest("username must be 2-32 characters and a password is required"))
		return
	}
	if !validRole(req.Role) {
		a.fail(w, 400, errBadRequest("role must be admin, operator or viewer"))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		a.fail(w, 500, err)
		return
	}
	u, err := a.Store.CreateUser(r.Context(), req.Username, string(hash), req.Role)
	if err != nil {
		a.fail(w, 400, err)
		return
	}
	a.audit(r, "user.create", u.Username+" ("+u.Role+")")
	a.json(w, 201, u)
}

type updateUserRequest struct {
	Role     string `json:"role"`
	Password string `json:"password"`
}

func (a *App) apiUpdateUser(w http.ResponseWriter, r *http.Request) {
	u := a.require(w, r, store.RoleAdmin)
	if u == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("uid"), 10, 64)
	if err != nil {
		a.fail(w, 400, errBadRequest("invalid user id"))
		return
	}
	var req updateUserRequest
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, 400, err)
		return
	}
	if req.Role != "" {
		if !validRole(req.Role) {
			a.fail(w, 400, errBadRequest("invalid role"))
			return
		}
		if id == u.ID && req.Role != store.RoleAdmin {
			admins, _ := a.adminCount(r.Context())
			if admins <= 1 {
				a.fail(w, 400, errBadRequest("at least one administrator is required"))
				return
			}
		}
		if err := a.Store.SetUserRole(r.Context(), id, req.Role); err != nil {
			a.fail(w, 500, err)
			return
		}
	}
	if req.Password != "" {
		if len(req.Password) < 8 {
			a.fail(w, 400, errBadRequest("password must be at least 8 characters"))
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			a.fail(w, 500, err)
			return
		}
		if err := a.Store.SetUserPassword(r.Context(), id, string(hash)); err != nil {
			a.fail(w, 500, err)
			return
		}
	}
	updated, err := a.Store.GetUser(r.Context(), id)
	if err != nil {
		a.fail(w, 404, err)
		return
	}
	a.audit(r, "user.update", updated.Username)
	a.json(w, 200, updated)
}

func (a *App) apiDeleteUser(w http.ResponseWriter, r *http.Request) {
	u := a.require(w, r, store.RoleAdmin)
	if u == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("uid"), 10, 64)
	if err != nil {
		a.fail(w, 400, errBadRequest("invalid user id"))
		return
	}
	if id == u.ID {
		a.fail(w, 400, errBadRequest("you cannot delete your own account"))
		return
	}
	target, err := a.Store.GetUser(r.Context(), id)
	if err != nil {
		a.fail(w, 404, err)
		return
	}
	if target.Role == store.RoleAdmin {
		admins, _ := a.adminCount(r.Context())
		if admins <= 1 {
			a.fail(w, 400, errBadRequest("at least one administrator is required"))
			return
		}
	}
	if err := a.Store.DeleteUser(r.Context(), id); err != nil {
		a.fail(w, 500, err)
		return
	}
	a.audit(r, "user.delete", target.Username)
	a.json(w, 200, map[string]bool{"deleted": true})
}

func (a *App) adminCount(ctx context.Context) (int, error) {
	users, err := a.Store.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, u := range users {
		if u.Role == store.RoleAdmin {
			n++
		}
	}
	return n, nil
}

func validRole(role string) bool {
	return role == store.RoleAdmin || role == store.RoleOperator || role == store.RoleViewer
}

// --- Settings ---

var settingDefaults = map[string]string{
	"watchdog_enabled":     "true",
	"watchdog_retries":     "3",
	"watchdog_backoff":     "5s",
	"stop_timeout":         "20s",
	"port_range_start":     "42420",
	"port_range_end":       "42430",
	"backup_hourly":        "24",
	"backup_daily":         "30",
	"backup_weekly":        "8",
	"backup_monthly":       "12",
	"backup_before_update": "true",
	"discord_enabled":      "false",
	"discord_webhook":      "",
	"dotnet_binary":        "dotnet",
	"default_version":      "",
	"auto_update":          "false",
}

func (a *App) settingsView(ctx context.Context) map[string]string {
	settings, _ := a.Store.AllSettings(ctx)
	out := map[string]string{}
	for k, v := range settingDefaults {
		out[k] = v
	}
	for k, v := range settings {
		out[k] = v
	}
	return out
}

func (a *App) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleViewer) == nil {
		return
	}
	a.json(w, 200, a.settingsView(r.Context()))
}

func (a *App) apiPutSettings(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleAdmin) == nil {
		return
	}
	var req map[string]string
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, 400, err)
		return
	}
	for k := range req {
		if _, ok := settingDefaults[k]; !ok {
			a.fail(w, 400, errBadRequest("unknown setting "+k))
			return
		}
	}
	for k, v := range req {
		if err := a.Store.SetSetting(r.Context(), k, v); err != nil {
			a.fail(w, 500, err)
			return
		}
	}
	a.audit(r, "settings.update", strings.Join(keysOf(req), ", "))
	a.json(w, 200, a.settingsView(r.Context()))
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (a *App) apiAudit(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleAdmin) == nil {
		return
	}
	entries, err := a.Store.ListAudit(r.Context(), parseInt(r.URL.Query().Get("limit"), 100))
	if err != nil {
		a.fail(w, 500, err)
		return
	}
	a.json(w, 200, entries)
}

type badRequestError struct{ msg string }

func (e badRequestError) Error() string { return e.msg }

func errBadRequest(msg string) error { return badRequestError{msg: msg} }
