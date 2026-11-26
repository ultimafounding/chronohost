package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackm/chronohost/internal/binaries"
	"github.com/jackm/chronohost/internal/config"
	"github.com/jackm/chronohost/internal/notify"
	"github.com/jackm/chronohost/internal/scheduler"
	"github.com/jackm/chronohost/internal/store"
)

type previewRequest struct {
	Form config.WizardForm `json:"form"`
}

func (a *App) apiPreview(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleViewer) == nil {
		return
	}
	var req previewRequest
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, 400, err)
		return
	}
	errs := config.Validate(req.Form)
	cfg, err := safeBuildConfig(req.Form)
	if err != nil {
		a.json(w, 200, map[string]any{"ok": false, "errors": errs, "preview": ""})
		return
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	a.json(w, 200, map[string]any{"ok": len(errs) == 0, "errors": errs, "preview": string(b)})
}

func (a *App) apiPreset(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleViewer) == nil {
		return
	}
	var req struct {
		ID   string            `json:"id"`
		Form config.WizardForm `json:"form"`
	}
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, 400, err)
		return
	}
	if req.ID == "" {
		req.ID = "standard"
	}
	form := req.Form
	if err := config.ApplyPreset(&form, req.ID); err != nil {
		a.fail(w, 400, err)
		return
	}
	a.json(w, 200, form)
}

func safeBuildConfig(f config.WizardForm) (map[string]any, error) {
	return config.BuildServerConfig(f)
}

type createInstanceRequest struct {
	Form    config.WizardForm `json:"form"`
	Version string            `json:"version"`
	Start   bool              `json:"start"`
	Mods    []struct {
		ModID     int `json:"modId"`
		ReleaseID int `json:"releaseId"`
	} `json:"mods"`
}

func (a *App) apiCreateInstance(w http.ResponseWriter, r *http.Request) {
	u := a.require(w, r, store.RoleOperator)
	if u == nil {
		return
	}
	var req createInstanceRequest
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, 400, err)
		return
	}
	if errs := config.Validate(req.Form); len(errs) > 0 {
		a.json(w, 400, map[string]any{"errors": errs})
		return
	}
	settings := a.settingsView(r.Context())
	startPort, endPort := parseInt(settings["port_range_start"], 42420), parseInt(settings["port_range_end"], 42430)
	if req.Form.Port < startPort || req.Form.Port > endPort {
		a.fail(w, 400, errBadRequest(fmt.Sprintf("port must be inside the configured range %d-%d", startPort, endPort)))
		return
	}
	if used, err := a.Store.PortInUse(r.Context(), req.Form.Port, ""); err != nil {
		a.fail(w, 500, err)
		return
	} else if used {
		a.fail(w, 409, errBadRequest("that port is already assigned to another instance"))
		return
	}
	if req.Version == "" {
		req.Version = settings["default_version"]
	}
	if req.Version == "" {
		latest, err := a.Binaries.Latest(r.Context())
		if err != nil {
			a.fail(w, 502, err)
			return
		}
		req.Version = latest
	}
	if err := a.ensureVersion(r.Context(), req.Version); err != nil {
		a.fail(w, 502, err)
		return
	}

	id := makeInstanceID(req.Form.ServerName)
	dataPath := filepath.Join(a.Root, "instances", id)
	if err := os.MkdirAll(filepath.Join(dataPath, "Saves"), 0o755); err != nil {
		a.fail(w, 500, err)
		return
	}
	if err := os.MkdirAll(filepath.Join(dataPath, "Mods"), 0o755); err != nil {
		a.fail(w, 500, err)
		return
	}
	cfg, err := config.BuildServerConfig(req.Form)
	if err != nil {
		a.fail(w, 400, err)
		return
	}
	if err := writeConfigFile(filepath.Join(dataPath, "serverconfig.json"), cfg); err != nil {
		a.fail(w, 500, err)
		return
	}
	cfgJSON, _ := json.Marshal(cfg)
	in := store.Instance{
		ID: id, Name: strings.TrimSpace(req.Form.ServerName), Version: req.Version,
		Port: req.Form.Port, DataPath: dataPath, Status: "stopped",
		PlayStyle: req.Form.Preset, GameMode: req.Form.GameMode,
		ServerConfig: string(cfgJSON), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := a.Store.CreateInstance(r.Context(), in); err != nil {
		a.fail(w, 500, err)
		return
	}
	// Every world starts with the default nightly snapshot job.
	for _, job := range scheduler.Defaults() {
		job.InstanceID = in.ID
		if _, err := a.Store.AddSchedule(r.Context(), job); err != nil {
			a.Log.Printf("create default backup schedule for %s: %v", in.Name, err)
		}
	}
	for _, m := range req.Mods {
		installed, warnings, err := a.installMod(r.Context(), in, m.ModID, m.ReleaseID)
		if err != nil {
			a.Log.Printf("install mod %d during creation: %v", m.ModID, err)
			continue
		}
		for _, warning := range warnings {
			a.Log.Printf("mod warning: %s", warning)
		}
		a.Log.Printf("installed %s %s", installed.Name, installed.Version)
	}
	if req.Start {
		spec, err := a.specFor(in)
		if err != nil {
			a.fail(w, 500, err)
			return
		}
		if err := a.Procs.Start(spec); err != nil {
			a.fail(w, 500, err)
			return
		}
		in.Status = "running"
		_ = a.Store.SetInstanceStatus(r.Context(), in.ID, in.Status)
	}
	a.Store.Audit(r.Context(), u.Username, "instance.create", in.Name+" on port "+fmt.Sprint(in.Port))
	a.Notifier.Send(r.Context(), notify.Event{Instance: in.Name, Type: "start", Detail: fmt.Sprintf("Instance provisioned on port %d (version %s)", in.Port, in.Version), Time: time.Now()})
	a.json(w, 201, in)
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func makeInstanceID(name string) string {
	slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if slug == "" {
		slug = "world"
	}
	if len(slug) > 24 {
		slug = slug[:24]
	}
	suffix, err := randomToken(3)
	if err != nil {
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return slug + "-" + suffix[:6]
}

func writeConfigFile(path string, cfg map[string]any) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

// ensureVersion downloads a build if it is not in the local cache yet.
func (a *App) ensureVersion(ctx context.Context, version string) error {
	if a.Binaries.IsCached(version) {
		return nil
	}
	channel := "stable"
	if strings.Contains(strings.ToLower(version), "pre") || strings.Contains(strings.ToLower(version), "rc") {
		channel = "unstable"
	}
	rels, err := a.versions(ctx, channel)
	if err != nil {
		return err
	}
	for _, rel := range rels {
		if rel.Version == version {
			_, err := a.Binaries.Download(ctx, rel, nil)
			return err
		}
	}
	return errBadRequest("version " + version + " was not found in the official feed")
}

func (a *App) apiListInstances(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleViewer) == nil {
		return
	}
	items, err := a.Store.ListInstances(r.Context())
	if err != nil {
		a.fail(w, 500, err)
		return
	}
	for i := range items {
		if st := a.Procs.Status(items[i].ID); st != "" && st != "stopped" {
			items[i].Status = st
		}
	}
	a.json(w, 200, items)
}

func (a *App) apiGetInstance(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleViewer) == nil {
		return
	}
	in, err := a.Store.GetInstance(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, 404, err)
		return
	}
	in.Status = a.Procs.Status(in.ID)
	a.json(w, 200, in)
}

type updateInstanceRequest struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Port      int    `json:"port"`
	AutoStart bool   `json:"autoStart"`
}

func (a *App) apiUpdateInstance(w http.ResponseWriter, r *http.Request) {
	u := a.require(w, r, store.RoleOperator)
	if u == nil {
		return
	}
	in, err := a.Store.GetInstance(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, 404, err)
		return
	}
	var req updateInstanceRequest
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, 400, err)
		return
	}
	if strings.TrimSpace(req.Name) != "" {
		in.Name = strings.TrimSpace(req.Name)
	}
	if req.Port > 0 {
		settings := a.settingsView(r.Context())
		if req.Port < parseInt(settings["port_range_start"], 42420) || req.Port > parseInt(settings["port_range_end"], 42430) {
			a.fail(w, 400, errBadRequest("port is outside the configured range"))
			return
		}
		if used, _ := a.Store.PortInUse(r.Context(), req.Port, in.ID); used {
			a.fail(w, 409, errBadRequest("port already in use"))
			return
		}
		in.Port = req.Port
	}
	if req.Version != "" {
		if err := a.ensureVersion(r.Context(), req.Version); err != nil {
			a.fail(w, 502, err)
			return
		}
		in.Version = req.Version
	}
	in.AutoStart = req.AutoStart
	if err := a.Store.UpdateInstance(r.Context(), in); err != nil {
		a.fail(w, 500, err)
		return
	}
	cfg := instanceConfig(in)
	cfg["ServerName"] = in.Name
	cfg["Port"] = in.Port
	if err := a.writeConfig(r.Context(), in, cfg); err != nil {
		a.Log.Printf("update config for %s: %v", in.Name, err)
	}
	a.audit(r, "instance.update", in.Name)
	a.json(w, 200, in)
}

func (a *App) writeConfig(ctx context.Context, in store.Instance, cfg map[string]any) error {
	b, _ := json.Marshal(cfg)
	in.ServerConfig = string(b)
	if err := a.Store.UpdateInstance(ctx, in); err != nil {
		return err
	}
	return writeConfigFile(filepath.Join(in.DataPath, "serverconfig.json"), cfg)
}

func (a *App) apiDeleteInstance(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleAdmin) == nil {
		return
	}
	if r.URL.Query().Get("confirm") != "true" {
		a.fail(w, 400, errBadRequest("add ?confirm=true to delete an instance"))
		return
	}
	in, err := a.Store.GetInstance(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, 404, err)
		return
	}
	if a.Procs.Running(in.ID) {
		if err := a.Procs.Stop(in.ID, true); err != nil {
			a.Log.Printf("stop before delete %s: %v", in.Name, err)
		}
	}
	if err := a.Store.DeleteInstance(r.Context(), in.ID); err != nil {
		a.fail(w, 500, err)
		return
	}
	if err := os.RemoveAll(in.DataPath); err != nil {
		a.Log.Printf("remove data dir %s: %v", in.DataPath, err)
	}
	backupsDir := filepath.Join(a.Root, "backups", in.ID)
	if err := os.RemoveAll(backupsDir); err != nil {
		a.Log.Printf("remove backups %s: %v", backupsDir, err)
	}
	a.audit(r, "instance.delete", in.Name)
	a.json(w, 200, map[string]bool{"deleted": true})
}

func (a *App) withInstance(w http.ResponseWriter, r *http.Request) (store.Instance, bool) {
	in, err := a.Store.GetInstance(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, 404, err)
		return in, false
	}
	return in, true
}

func (a *App) apiStart(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleOperator) == nil {
		return
	}
	in, ok := a.withInstance(w, r)
	if !ok {
		return
	}
	spec, err := a.specFor(in)
	if err != nil {
		a.fail(w, 500, err)
		return
	}
	if err := a.Procs.Start(spec); err != nil {
		a.fail(w, 500, err)
		return
	}
	_ = a.Store.SetInstanceStatus(r.Context(), in.ID, "running")
	a.audit(r, "instance.start", in.Name)
	a.json(w, 200, map[string]string{"status": "running"})
}

func (a *App) apiStop(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleOperator) == nil {
		return
	}
	in, ok := a.withInstance(w, r)
	if !ok {
		return
	}
	if err := a.Procs.Stop(in.ID, true); err != nil {
		a.fail(w, 500, err)
		return
	}
	_ = a.Store.SetInstanceStatus(r.Context(), in.ID, "stopped")
	a.audit(r, "instance.stop", in.Name)
	a.json(w, 200, map[string]string{"status": "stopped"})
}

func (a *App) apiRestart(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleOperator) == nil {
		return
	}
	in, ok := a.withInstance(w, r)
	if !ok {
		return
	}
	spec, err := a.specFor(in)
	if err != nil {
		a.fail(w, 500, err)
		return
	}
	if err := a.Procs.Restart(spec); err != nil {
		a.fail(w, 500, err)
		return
	}
	_ = a.Store.SetInstanceStatus(r.Context(), in.ID, "running")
	a.audit(r, "instance.restart", in.Name)
	a.json(w, 200, map[string]string{"status": "running"})
}

func (a *App) apiKill(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleOperator) == nil {
		return
	}
	in, ok := a.withInstance(w, r)
	if !ok {
		return
	}
	if err := a.Procs.Stop(in.ID, false); err != nil {
		a.fail(w, 500, err)
		return
	}
	_ = a.Store.SetInstanceStatus(r.Context(), in.ID, "stopped")
	a.audit(r, "instance.kill", in.Name)
	a.json(w, 200, map[string]string{"status": "stopped"})
}

func (a *App) apiCommand(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleOperator) == nil {
		return
	}
	in, ok := a.withInstance(w, r)
	if !ok {
		return
	}
	var req struct {
		Command string `json:"command"`
	}
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, 400, err)
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		a.fail(w, 400, errBadRequest("command must not be empty"))
		return
	}
	if err := a.Procs.Command(in.ID, req.Command); err != nil {
		a.fail(w, 500, err)
		return
	}
	a.audit(r, "console.command", in.Name+": "+req.Command)
	a.json(w, 200, map[string]bool{"ok": true})
}

func (a *App) apiLogs(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleViewer) == nil {
		return
	}
	limit := parseInt(r.URL.Query().Get("limit"), 200)
	if limit < 1 || limit > 2000 {
		limit = 200
	}
	a.json(w, 200, a.Procs.Logs(r.PathValue("id"), limit))
}

func (a *App) apiTelemetry(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleViewer) == nil {
		return
	}
	id := r.PathValue("id")
	in, err := a.Store.GetInstance(r.Context(), id)
	if err != nil {
		a.fail(w, 404, err)
		return
	}
	a.json(w, 200, map[string]any{
		"telemetry": a.Procs.Telemetry(id),
		"status":    a.Procs.Status(id),
		"instance":  in,
	})
}

func (a *App) apiGetConfig(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleViewer) == nil {
		return
	}
	in, ok := a.withInstance(w, r)
	if !ok {
		return
	}
	cfg := instanceConfig(in)
	if raw, err := os.ReadFile(filepath.Join(in.DataPath, "serverconfig.json")); err == nil {
		var disk map[string]any
		if json.Unmarshal(raw, &disk) == nil {
			cfg = disk
		}
	}
	a.json(w, 200, cfg)
}

func (a *App) apiPutConfig(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleAdmin) == nil {
		return
	}
	in, ok := a.withInstance(w, r)
	if !ok {
		return
	}
	var cfg map[string]any
	if err := decodeJSON(r, &cfg); err != nil {
		a.fail(w, 400, err)
		return
	}
	config.EnsureRoles(cfg)
	if a.Procs.Running(in.ID) {
		a.fail(w, 409, errBadRequest("stop the server before editing serverconfig.json"))
		return
	}
	if err := a.writeConfig(r.Context(), in, cfg); err != nil {
		a.fail(w, 500, err)
		return
	}
	a.audit(r, "config.update", in.Name)
	a.json(w, 200, map[string]bool{"ok": true})
}

// --- Schedules ---

type scheduleRequest struct {
	Kind    string `json:"kind"`
	Expr    string `json:"expr"`
	Message string `json:"message"`
	Enabled bool   `json:"enabled"`
}

func (a *App) apiListSchedules(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleViewer) == nil {
		return
	}
	jobs, err := a.Store.ListSchedules(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, 500, err)
		return
	}
	a.json(w, 200, jobs)
}

func (a *App) apiAddSchedule(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleOperator) == nil {
		return
	}
	in, ok := a.withInstance(w, r)
	if !ok {
		return
	}
	var req scheduleRequest
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, 400, err)
		return
	}
	if req.Kind != "backup" && req.Kind != "restart" && req.Kind != "announce" {
		a.fail(w, 400, errBadRequest("kind must be backup, restart or announce"))
		return
	}
	job, err := a.Store.AddSchedule(r.Context(), store.Schedule{
		InstanceID: in.ID, Kind: req.Kind, Expr: req.Expr,
		Message: req.Message, Enabled: true, CreatedAt: time.Now(),
	})
	if err != nil {
		a.fail(w, 500, err)
		return
	}
	a.audit(r, "schedule.create", in.Name+": "+req.Kind+" "+req.Expr)
	a.json(w, 201, job)
}

func (a *App) apiDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleOperator) == nil {
		return
	}
	id, err := parseInt64(r.PathValue("sid"))
	if err != nil {
		a.fail(w, 400, errBadRequest("invalid schedule id"))
		return
	}
	if err := a.Store.DeleteSchedule(r.Context(), id); err != nil {
		a.fail(w, 500, err)
		return
	}
	a.json(w, 200, map[string]bool{"deleted": true})
}

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// sortedVersions is used by the UI to keep dropdowns stable.
func sortedVersions(rels []binaries.Release) []binaries.Release {
	out := append([]binaries.Release(nil), rels...)
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out
}
