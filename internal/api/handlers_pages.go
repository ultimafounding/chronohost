package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/jackm/chronohost/internal/binaries"
	"github.com/jackm/chronohost/internal/config"
	"github.com/jackm/chronohost/internal/process"
	"github.com/jackm/chronohost/internal/store"
)

func (a *App) basePage(r *http.Request, title string) map[string]any {
	return map[string]any{"Title": title, "Now": time.Now()}
}

func (a *App) allInstances(r *http.Request) []store.Instance {
	items, err := a.Store.ListInstances(r.Context())
	if err != nil || items == nil {
		items = []store.Instance{}
	}
	for i := range items {
		if st := a.Procs.Status(items[i].ID); st != "" && st != "stopped" {
			items[i].Status = st
		} else if items[i].Status == "" {
			items[i].Status = "stopped"
		}
	}
	return items
}

func (a *App) pageDashboard(w http.ResponseWriter, r *http.Request) {
	data := a.basePage(r, "Dashboard")
	items := a.allInstances(r)
	data["Instances"] = items
	telemetry := map[string]process.Telemetry{}
	running := 0
	for _, in := range items {
		if in.Status == "running" {
			running++
		}
		telemetry[in.ID] = a.Procs.Telemetry(in.ID)
	}
	data["Telemetry"] = telemetry
	data["Stats"] = map[string]int{
		"total": len(items), "running": running, "stopped": len(items) - running,
		"cached": len(a.Binaries.Cached()),
	}
	a.render(w, r, "dashboard.html", data)
}

func (a *App) pageWizard(w http.ResponseWriter, r *http.Request) {
	data := a.basePage(r, "Make a Server")
	form := config.DefaultWizardForm()
	if preset := r.URL.Query().Get("preset"); preset != "" {
		if err := config.ApplyPreset(&form, preset); err != nil {
			data["Error"] = err.Error()
		}
	}
	data["Form"] = form
	data["Presets"] = config.Presets()
	settings := a.settingsView(r.Context())
	data["PortStart"] = parseInt(settings["port_range_start"], 42420)
	data["PortEnd"] = parseInt(settings["port_range_end"], 42430)
	data["Cached"] = a.Binaries.Cached()
	data["Latest"] = settings["default_version"]
	if data["Latest"] == "" {
		if latest, err := a.Binaries.Latest(r.Context()); err == nil {
			data["Latest"] = latest
		}
	}
	data["ConfigJSON"] = ""
	a.render(w, r, "wizard.html", data)
}

func (a *App) pageInstance(w http.ResponseWriter, r *http.Request) {
	in, err := a.Store.GetInstance(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data := a.basePage(r, in.Name)
	in.Status = a.Procs.Status(in.ID)
	data["Instance"] = in
	cfg, _ := json.MarshalIndent(instanceConfig(in), "", "  ")
	data["ConfigJSON"] = string(cfg)
	data["Logs"] = a.Procs.Logs(in.ID, 200)
	data["Telemetry"] = a.Procs.Telemetry(in.ID)
	if mods, err := a.Store.ListMods(r.Context(), in.ID); err == nil {
		data["Mods"] = mods
	}
	if backups, err := a.Store.ListBackups(r.Context(), in.ID); err == nil {
		data["Backups"] = backups
	}
	if jobs, err := a.Store.ListSchedules(r.Context(), in.ID); err == nil {
		data["Schedules"] = jobs
	}
	data["Cached"] = a.Binaries.Cached()
	a.render(w, r, "instance.html", data)
}

func (a *App) pageVersions(w http.ResponseWriter, r *http.Request) {
	data := a.basePage(r, "Version Manager")
	stable, errS := a.versions(r.Context(), "stable")
	unstable, errU := a.versions(r.Context(), "unstable")
	if stable == nil {
		stable = []binaries.Release{}
	}
	if unstable == nil {
		unstable = []binaries.Release{}
	}
	data["Stable"] = stable
	data["Unstable"] = unstable
	data["Cached"] = a.Binaries.Cached()
	data["Latest"] = ""
	_ = sortedVersions(stable)
	if latest, err := a.Binaries.Latest(r.Context()); err == nil {
		data["Latest"] = latest
	}
	data["Errors"] = map[string]string{"stable": errString(errS), "unstable": errString(errU)}
	a.render(w, r, "versions.html", data)
}

func (a *App) pageMods(w http.ResponseWriter, r *http.Request) {
	data := a.basePage(r, "Mod Manager")
	instances := a.allInstances(r)
	data["Instances"] = instances
	var in *store.Instance
	for i := range instances {
		if instances[i].ID == r.URL.Query().Get("instance") {
			in = &instances[i]
			break
		}
	}
	if in == nil && len(instances) > 0 {
		in = &instances[0]
	}
	if in != nil {
		data["Instance"] = *in
		query := r.URL.Query().Get("q")
		data["Query"] = query
		gv := in.Version
		if r.URL.Query().Get("gv") != "" {
			gv = r.URL.Query().Get("gv")
		}
		data["GameVersion"] = gv
		mods, _ := a.Store.ListMods(r.Context(), in.ID)
		data["Mods"] = buildModViews(mods, gv)
		if query != "" {
			results, err := a.ModDB.Search(r.Context(), query, gv, 20)
			if err != nil {
				data["Error"] = err.Error()
			}
			data["Search"] = results
		}
	}
	a.render(w, r, "mods.html", data)
}

func (a *App) pageBackups(w http.ResponseWriter, r *http.Request) {
	data := a.basePage(r, "Temporal Rollbacks")
	instances := a.allInstances(r)
	data["Instances"] = instances
	var in *store.Instance
	for i := range instances {
		if instances[i].ID == r.URL.Query().Get("instance") {
			in = &instances[i]
			break
		}
	}
	if in == nil && len(instances) > 0 {
		in = &instances[0]
	}
	if in != nil {
		data["Instance"] = *in
		backups, _ := a.Store.ListBackups(r.Context(), in.ID)
		data["Backups"] = backups
		data["Policy"] = a.retentionPolicy(r.Context())
	}
	a.render(w, r, "backups.html", data)
}

func (a *App) pageSettings(w http.ResponseWriter, r *http.Request) {
	data := a.basePage(r, "Settings")
	data["Settings"] = a.settingsView(r.Context())
	data["Defaults"] = settingDefaults
	a.render(w, r, "settings.html", data)
}

func (a *App) pageUsers(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleAdmin) == nil {
		a.render(w, r, "error.html", map[string]any{"Title": "Forbidden", "Error": "Administrator access required."})
		return
	}
	data := a.basePage(r, "Users & Access")
	users, err := a.Store.ListUsers(r.Context())
	if err != nil {
		users = []store.User{}
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
	data["Users"] = users
	audit, _ := a.Store.ListAudit(r.Context(), 100)
	data["Audit"] = audit
	data["Roles"] = []string{store.RoleAdmin, store.RoleOperator, store.RoleViewer}
	a.render(w, r, "users.html", data)
}
