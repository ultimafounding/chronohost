// Package api implements ChronoHost's HTTP control plane: HTML portal,
// JSON API, WebSocket console and event fan-out.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackm/chronohost/internal/backup"
	"github.com/jackm/chronohost/internal/binaries"
	"github.com/jackm/chronohost/internal/config"
	"github.com/jackm/chronohost/internal/moddb"
	"github.com/jackm/chronohost/internal/notify"
	"github.com/jackm/chronohost/internal/process"
	"github.com/jackm/chronohost/internal/scheduler"
	"github.com/jackm/chronohost/internal/store"
	"github.com/jackm/chronohost/internal/web"
	"golang.org/x/crypto/bcrypt"
)

const sessionCookie = "chronohost_session"

type contextKey string

const userContextKey contextKey = "user"

// App holds all application dependencies.
type App struct {
	Root      string
	Store     *store.Store
	Binaries  *binaries.Client
	ModDB     *moddb.Client
	Procs     *process.Manager
	Backups   *backup.Engine
	Notifier  *notify.Notifier
	Scheduler *scheduler.Scheduler
	Templates *template.Template
	Static    fs.FS
	Log       *log.Logger

	versionMu    sync.Mutex
	versionCache map[string]cachedVersions
	httpClient   *http.Client
}

type cachedVersions struct {
	at  time.Time
	rel []binaries.Release
}

// New assembles the application and bootstraps the administrator account.
func New(root string, logger *log.Logger) (*App, error) {
	if logger == nil {
		logger = log.Default()
	}
	for _, dir := range []string{
		filepath.Join(root, "binaries"),
		filepath.Join(root, "instances"),
		filepath.Join(root, "backups"),
		filepath.Join(root, "db"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	st, err := store.Open(filepath.Join(root, "db", "chronohost.db"))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	tmpl, err := web.Templates()
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	staticFS, err := web.Static()
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("load static assets: %w", err)
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	a := &App{
		Root: root, Store: st, Log: logger,
		Binaries:  binaries.NewClient(filepath.Join(root, "binaries"), hc),
		ModDB:     moddb.NewClient("", hc),
		Procs:     process.New(logger.Printf),
		Backups:   backup.New(filepath.Join(root, "backups"), logger.Printf),
		Templates: tmpl, Static: staticFS, httpClient: hc,
		versionCache: map[string]cachedVersions{},
	}
	a.Notifier = notify.New(func() (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return st.GetSetting(ctx, "discord_webhook", ""), st.GetSettingBool(ctx, "discord_enabled", false)
	}, hc)
	a.Procs.SetEventHandler(&eventHandler{app: a})
	a.Scheduler = scheduler.New(st, a.Procs, a.Backups, a.Notifier, a.specFor, logger.Printf)

	if err := a.bootstrapAdmin(); err != nil {
		st.Close()
		return nil, err
	}
	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			_ = st.DeleteExpiredSessions(context.Background())
		}
	}()
	return a, nil
}

func (a *App) bootstrapAdmin() error {
	ctx := context.Background()
	n, err := a.Store.UserCount(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	user := envOr("CHRONOHOST_ADMIN_USER", "admin")
	pass := envOr("CHRONOHOST_ADMIN_PASSWORD", "chronohost")
	if user == "admin" && pass == "chronohost" {
		a.Log.Printf("WARNING: using default admin credentials (%s / chronohost). Set CHRONOHOST_ADMIN_PASSWORD before exposing the portal.", user)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if _, err := a.Store.CreateUser(ctx, user, string(hash), store.RoleAdmin); err != nil {
		return err
	}
	a.Log.Printf("Created administrator account %q", user)
	return nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// StartBackground restores auto-start instances and launches the scheduler.
func (a *App) StartBackground(ctx context.Context) {
	a.Scheduler.Start(ctx)
	instances, err := a.Store.ListInstances(ctx)
	if err != nil {
		a.Log.Printf("restore instances: %v", err)
		return
	}
	for _, in := range instances {
		if in.AutoStart && !a.Procs.Running(in.ID) {
			spec, err := a.specFor(in)
			if err != nil {
				a.Log.Printf("auto-start %s: %v", in.Name, err)
				continue
			}
			if err := a.Procs.Start(spec); err != nil {
				a.Log.Printf("auto-start %s: %v", in.Name, err)
			}
		}
	}
}

// Close stops managed processes and closes the database.
func (a *App) Close() error {
	a.Procs.Shutdown()
	backupErr := a.Backups.Close()
	storeErr := a.Store.Close()
	if backupErr != nil {
		return backupErr
	}
	return storeErr
}

// Handler returns the root HTTP handler with all routes.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public pages and assets.
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(a.Static))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { a.json(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /login", a.pageLogin)
	mux.HandleFunc("POST /login", a.handleLogin)
	mux.HandleFunc("POST /logout", a.handleLogout)

	// Portal pages.
	mux.HandleFunc("GET /{$}", a.pageDashboard)
	mux.HandleFunc("GET /instances/new", a.pageWizard)
	mux.HandleFunc("GET /instances/{id}", a.pageInstance)
	mux.HandleFunc("GET /versions", a.pageVersions)
	mux.HandleFunc("GET /mods", a.pageMods)
	mux.HandleFunc("GET /backups", a.pageBackups)
	mux.HandleFunc("GET /settings", a.pageSettings)
	mux.HandleFunc("GET /users", a.pageUsers)

	// Instance and wizard API.
	mux.HandleFunc("POST /api/instances/preview", a.apiPreview)
	mux.HandleFunc("POST /api/instances/preset", a.apiPreset)
	mux.HandleFunc("GET /api/instances", a.apiListInstances)
	mux.HandleFunc("POST /api/instances", a.apiCreateInstance)
	mux.HandleFunc("GET /api/instances/{id}", a.apiGetInstance)
	mux.HandleFunc("DELETE /api/instances/{id}", a.apiDeleteInstance)
	mux.HandleFunc("PUT /api/instances/{id}", a.apiUpdateInstance)
	mux.HandleFunc("POST /api/instances/{id}/start", a.apiStart)
	mux.HandleFunc("POST /api/instances/{id}/stop", a.apiStop)
	mux.HandleFunc("POST /api/instances/{id}/restart", a.apiRestart)
	mux.HandleFunc("POST /api/instances/{id}/kill", a.apiKill)
	mux.HandleFunc("POST /api/instances/{id}/command", a.apiCommand)
	mux.HandleFunc("GET /api/instances/{id}/logs", a.apiLogs)
	mux.HandleFunc("GET /api/instances/{id}/telemetry", a.apiTelemetry)
	mux.HandleFunc("GET /api/instances/{id}/config", a.apiGetConfig)
	mux.HandleFunc("PUT /api/instances/{id}/config", a.apiPutConfig)
	mux.HandleFunc("GET /api/instances/{id}/schedules", a.apiListSchedules)
	mux.HandleFunc("POST /api/instances/{id}/schedules", a.apiAddSchedule)
	mux.HandleFunc("DELETE /api/schedules/{sid}", a.apiDeleteSchedule)

	// Mods, backups and versions API.
	mux.HandleFunc("GET /api/instances/{id}/mods", a.apiListMods)
	mux.HandleFunc("POST /api/instances/{id}/mods/install", a.apiInstallMod)
	mux.HandleFunc("DELETE /api/instances/{id}/mods/{mid}", a.apiDeleteMod)
	mux.HandleFunc("POST /api/instances/{id}/mods/{mid}/pin", a.apiPinMod)
	mux.HandleFunc("GET /api/moddb/search", a.apiModDBSearch)
	mux.HandleFunc("GET /api/moddb/mod/{id}", a.apiModDBDetail)
	mux.HandleFunc("GET /api/instances/{id}/backups", a.apiListBackups)
	mux.HandleFunc("POST /api/instances/{id}/backups", a.apiCreateBackup)
	mux.HandleFunc("POST /api/instances/{id}/backups/{bid}/restore", a.apiRestoreBackup)
	mux.HandleFunc("DELETE /api/instances/{id}/backups/{bid}", a.apiDeleteBackup)
	mux.HandleFunc("GET /api/versions", a.apiVersions)
	mux.HandleFunc("POST /api/versions/download", a.apiDownloadVersion)
	mux.HandleFunc("DELETE /api/versions/{version}", a.apiDeleteVersion)

	// Administration API.
	mux.HandleFunc("GET /api/settings", a.apiGetSettings)
	mux.HandleFunc("PUT /api/settings", a.apiPutSettings)
	mux.HandleFunc("GET /api/users", a.apiListUsers)
	mux.HandleFunc("POST /api/users", a.apiCreateUser)
	mux.HandleFunc("DELETE /api/users/{uid}", a.apiDeleteUser)
	mux.HandleFunc("PUT /api/users/{uid}", a.apiUpdateUser)
	mux.HandleFunc("GET /api/audit", a.apiAudit)

	// Live console.
	mux.HandleFunc("GET /ws/console", a.wsConsole)

	return a.authMiddleware(mux)
}

// --- Shared helpers ---

func (a *App) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		public := path == "/login" || path == "/healthz" ||
			strings.HasPrefix(path, "/static/")
		ctx := r.Context()
		if c, err := r.Cookie(sessionCookie); err == nil {
			if u, err := a.Store.SessionUser(ctx, c.Value); err == nil {
				ctx = context.WithValue(ctx, userContextKey, &u)
			}
		}
		r = r.WithContext(ctx)
		if !public && currentUser(r) == nil {
			if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/ws/") {
				a.json(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			} else {
				http.Redirect(w, r, "/login?next="+urlQueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(userContextKey).(*store.User)
	return u
}

func (a *App) require(w http.ResponseWriter, r *http.Request, role string) *store.User {
	u := currentUser(r)
	if u == nil {
		a.json(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return nil
	}
	rank := map[string]int{store.RoleViewer: 1, store.RoleOperator: 2, store.RoleAdmin: 3}
	if rank[u.Role] < rank[role] {
		a.json(w, http.StatusForbidden, map[string]string{"error": "insufficient permissions"})
		return nil
	}
	return u
}

func (a *App) json(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *App) fail(w http.ResponseWriter, status int, err error) {
	a.json(w, status, map[string]string{"error": err.Error()})
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["User"] = currentUser(r)
	data["Path"] = r.URL.Path
	var buf strings.Builder
	if err := a.Templates.ExecuteTemplate(&buf, name, data); err != nil {
		a.Log.Printf("render %s: %v", name, err)
		http.Error(w, "Template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(buf.String()))
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-_.~", r) {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}

func randomToken(bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func timeNow() time.Time { return time.Now() }

func parseInt(s string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fallback
	}
	return n
}

func (a *App) specFor(in store.Instance) (process.Spec, error) {
	ctx := context.Background()
	if !a.Binaries.IsCached(in.Version) {
		return process.Spec{}, fmt.Errorf("version %s is not downloaded yet", in.Version)
	}
	dll, runtime, err := a.Binaries.DLL(in.Version)
	if err != nil {
		return process.Spec{}, err
	}
	backoff, err := time.ParseDuration(a.Store.GetSetting(ctx, "watchdog_backoff", "5s"))
	if err != nil {
		backoff = 5 * time.Second
	}
	stopTimeout, err := time.ParseDuration(a.Store.GetSetting(ctx, "stop_timeout", "20s"))
	if err != nil {
		stopTimeout = 20 * time.Second
	}
	_ = runtime // the image installs both 8.0 and 10.0 runtimes.
	return process.Spec{
		ID: in.ID, Version: in.Version, DataPath: in.DataPath,
		BinaryDir: filepath.Dir(dll), DLL: dll,
		Dotnet:     a.Store.GetSetting(ctx, "dotnet_binary", "dotnet"),
		Port:       in.Port,
		Watchdog:   a.Store.GetSettingBool(ctx, "watchdog_enabled", true),
		MaxRetries: a.Store.GetSettingInt(ctx, "watchdog_retries", 3),
		Backoff:    backoff, StopTimeout: stopTimeout,
	}, nil
}

// eventHandler forwards process events to Discord and the audit log.
type eventHandler struct{ app *App }

func (e *eventHandler) OnEvent(instanceID, event, detail string) {
	ctx := context.Background()
	in, err := e.app.Store.GetInstance(ctx, instanceID)
	name := instanceID
	if err == nil {
		name = in.Name
	}
	switch event {
	case "start", "stop":
		_ = e.app.Store.SetInstanceStatus(ctx, instanceID, event)
	case "crash":
		_ = e.app.Store.SetInstanceStatus(ctx, instanceID, "crashed")
	}
	_ = e.app.Notifier.Send(ctx, notify.Event{Instance: name, Type: event, Detail: detail, Time: time.Now()})
}

// instanceConfig decodes the stored serverconfig.json for display.
func instanceConfig(in store.Instance) map[string]any {
	var cfg map[string]any
	if err := json.Unmarshal([]byte(in.ServerConfig), &cfg); err != nil || cfg == nil {
		return map[string]any{}
	}
	return cfg
}

// wizardFromConfig attempts to reconstruct a wizard form from a stored config
// so the advanced editor can pre-fill values.
func wizardFromConfig(in store.Instance) config.WizardForm {
	f := config.DefaultWizardForm()
	f.ServerName, f.Port = in.Name, in.Port
	f.Preset, f.GameMode = in.PlayStyle, in.GameMode
	f.Password = ""
	var cfg map[string]any
	if json.Unmarshal([]byte(in.ServerConfig), &cfg) != nil {
		return f
	}
	if v, ok := cfg["MaxClients"].(float64); ok {
		f.MaxClients = int(v)
	}
	if v, ok := cfg["WelcomeMessage"].(string); ok {
		f.WelcomeMessage = v
	}
	if v, ok := cfg["ServerDescription"].(string); ok && v != "" {
		f.ServerDescription = v
	}
	if v, ok := cfg["Password"].(string); ok {
		f.Password = v
	}
	wc, _ := cfg["WorldConfig"].(map[string]any)
	if v, ok := wc["Seed"].(string); ok {
		f.Seed = v
	}
	w, _ := wc["WorldConfiguration"].(map[string]any)
	getStr := func(k string) string {
		if v, ok := w[k].(string); ok {
			return v
		}
		return ""
	}
	getFloat := func(k string, fallback float64) float64 {
		if v, ok := getStrNum(w[k]); ok {
			return v
		}
		return fallback
	}
	f.Landcover = getFloat("landcover", f.Landcover)
	f.LandcoverScale = getFloat("oceanscale", f.LandcoverScale)
	f.UpheavalRate = getFloat("upheavelCommonness", f.UpheavalRate)
	f.LandformScale = getFloat("landformScale", f.LandformScale)
	f.GlobalTemperature = getFloat("globalTemperature", f.GlobalTemperature)
	f.GlobalPrecipitation = getFloat("globalPrecipitation", f.GlobalPrecipitation)
	f.Forestation = getFloat("globalForestation", f.Forestation)
	f.TemporalStorms = defaultString(getStr("temporalStorms"), f.TemporalStorms)
	f.TemporalRifts = defaultString(getStr("temporalRifts"), f.TemporalRifts)
	f.CreatureHostility = defaultString(getStr("creatureHostility"), f.CreatureHostility)
	f.MicroblockChiseling = defaultString(getStr("microblockChiseling"), f.MicroblockChiseling)
	f.DeathPunishment = defaultString(getStr("deathPunishment"), f.DeathPunishment)
	f.StartingClimate = defaultString(getStr("startingClimate"), f.StartingClimate)
	return f
}

func getStrNum(v any) (float64, bool) {
	switch t := v.(type) {
	case string:
		n, err := strconv.ParseFloat(t, 64)
		return n, err == nil
	case float64:
		return t, true
	case int:
		return float64(t), true
	}
	return 0, false
}

func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func (a *App) audit(r *http.Request, action, detail string) {
	if u := currentUser(r); u != nil {
		a.Store.Audit(r.Context(), u.Username, action, detail)
	}
}

// ErrNotFound is used by handlers for missing resources.
var ErrNotFound = errors.New("resource not found")
