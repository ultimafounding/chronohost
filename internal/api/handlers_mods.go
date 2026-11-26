package api

import (
	"archive/zip"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/jackm/chronohost/internal/moddb"
	"github.com/jackm/chronohost/internal/notify"
	"github.com/jackm/chronohost/internal/store"
)

type dependencyStatus struct {
	ModID     string `json:"modId"`
	Version   string `json:"version,omitempty"`
	Installed bool   `json:"installed"`
}

type modView struct {
	store.Mod
	Compatible       bool               `json:"compatible"`
	DependencyStatus []dependencyStatus `json:"dependencyStatus"`
	Dependents       []string           `json:"dependents"`
}

// InstallOutcome describes a top-level install and every dependency that was
// pulled in with it.
type InstallOutcome struct {
	Mod       store.Mod   `json:"mod"`
	Installed []store.Mod `json:"installed"`
	Warnings  []string    `json:"warnings"`
}

func normalizeModID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func buildModViews(mods []store.Mod, gameVersion string) []modView {
	installedIDs := make(map[int]bool, len(mods))
	installedNames := make(map[string]bool, len(mods))
	for _, m := range mods {
		installedIDs[m.ModID] = true
		if name := normalizeModID(m.Name); name != "" {
			installedNames[name] = true
		}
	}

	views := make([]modView, 0, len(mods))
	for _, m := range mods {
		view := modView{
			Mod:              m,
			Compatible:       len(m.GameVersions) == 0 || versionInTags(m.GameVersions, gameVersion),
			DependencyStatus: make([]dependencyStatus, 0, len(m.Dependencies)),
			Dependents:       findDependents(mods, m),
		}
		for _, dep := range m.Dependencies {
			installed := dep.ResolvedID > 0 && installedIDs[dep.ResolvedID]
			if !installed {
				installed = installedNames[normalizeModID(dep.ModID)]
			}
			view.DependencyStatus = append(view.DependencyStatus, dependencyStatus{
				ModID:     dep.ModID,
				Version:   dep.Version,
				Installed: installed,
			})
		}
		views = append(views, view)
	}
	return views
}

func findDependents(mods []store.Mod, target store.Mod) []string {
	seen := make(map[string]bool)
	var out []string
	for _, m := range mods {
		if m.ID == target.ID {
			continue
		}
		for _, dep := range m.Dependencies {
			matched := dep.ResolvedID > 0 && dep.ResolvedID == target.ModID
			if !matched && dep.ResolvedID == 0 {
				matched = normalizeModID(dep.ModID) != "" && normalizeModID(dep.ModID) == normalizeModID(target.Name)
			}
			if matched && !seen[m.Name] {
				seen[m.Name] = true
				out = append(out, m.Name)
				break
			}
		}
	}
	return out
}

// modDependencies reads the enabled, non-engine dependencies from an archive's
// modinfo.json. Engine dependencies are handled by game version checks.
func modDependencies(path string) moddb.DependencyList {
	r, err := zip.OpenReader(path)
	if err != nil {
		return nil
	}
	defer r.Close()
	for _, f := range r.File {
		if filepath.Base(f.Name) != "modinfo.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil
		}
		deps, err := moddb.ReadDependencies(rc)
		rc.Close()
		if err != nil {
			return nil
		}
		out := make(moddb.DependencyList, 0, len(deps))
		for _, dep := range deps {
			if !dep.Enabled || dep.ModID == "" || strings.EqualFold(dep.ModID, "game") {
				continue
			}
			out = append(out, dep)
		}
		return out
	}
	return nil
}

func (a *App) resolveDependencyID(ctx context.Context, id, gameVersion string) (int, error) {
	if numeric, err := strconv.Atoi(strings.TrimSpace(id)); err == nil && numeric > 0 {
		return numeric, nil
	}
	detail, detailErr := a.ModDB.Detail(ctx, id)
	if detailErr == nil && detail.ModID > 0 {
		return detail.ModID, nil
	}
	// Some modinfo ids are not URL aliases. Search for an exact alias or
	// modidstr match before giving up on the dependency.
	wanted := normalizeModID(id)
	if wanted != "" {
		mods, err := a.ModDB.Search(ctx, id, gameVersion, 25)
		if err == nil {
			for _, m := range mods {
				if normalizeModID(m.URLAlias) == wanted || normalizeModID(m.Name) == wanted {
					return m.ModID, nil
				}
				for _, alias := range m.ModIDStrs {
					if normalizeModID(alias) == wanted {
						return m.ModID, nil
					}
				}
			}
		}
	}
	if detailErr != nil {
		return 0, detailErr
	}
	return 0, fmt.Errorf("no numeric id for %s", id)
}

func versionsMatch(required, installed string) bool {
	required = strings.TrimSpace(required)
	installed = strings.TrimSpace(installed)
	return required == "" || installed == "" || strings.EqualFold(required, installed)
}

// installMod installs modID and recursively installs any ModDB dependencies
// declared by its modinfo.json.
func (a *App) installMod(ctx context.Context, in store.Instance, modID, releaseID int) (InstallOutcome, error) {
	return a.installModTree(ctx, in, modID, releaseID, make(map[int]bool), 0)
}

func (a *App) installModTree(ctx context.Context, in store.Instance, modID, releaseID int, visiting map[int]bool, depth int) (InstallOutcome, error) {
	out := InstallOutcome{Installed: []store.Mod{}, Warnings: []string{}}
	if depth > 8 {
		return out, fmt.Errorf("dependency chain too deep while installing %d", modID)
	}
	if modID <= 0 {
		return out, fmt.Errorf("invalid mod id %d", modID)
	}
	if visiting[modID] {
		out.Warnings = append(out.Warnings, fmt.Sprintf("Skipped mod %d: dependency cycle detected", modID))
		return out, nil
	}
	visiting[modID] = true
	defer delete(visiting, modID)

	detail, err := a.ModDB.Detail(ctx, strconv.Itoa(modID))
	if err != nil {
		return out, err
	}
	compatible := a.ModDB.Compatible(detail, in.Version)
	var rel moddb.Release
	found := false
	if releaseID > 0 {
		for _, candidate := range detail.Releases {
			if candidate.ReleaseID == releaseID {
				rel, found = candidate, true
				break
			}
		}
	} else if len(compatible) > 0 {
		rel, found = compatible[0], true
	} else if len(detail.Releases) > 0 {
		rel, found = detail.Releases[0], true
	}
	if !found {
		return out, fmt.Errorf("no suitable release found for %s", detail.Name)
	}

	modsDir := filepath.Join(in.DataPath, "Mods")
	if err := os.MkdirAll(modsDir, 0o755); err != nil {
		return out, err
	}
	path, err := a.ModDB.Download(ctx, rel, modsDir, nil)
	if err != nil {
		return out, err
	}

	deps := modDependencies(path)
	resolved := make(map[string]int, len(deps))
	for _, dep := range deps {
		id, err := a.resolveDependencyID(ctx, dep.ModID, in.Version)
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s requires %s but it could not be found on ModDB: %v", detail.Name, dep.ModID, err))
			continue
		}
		resolved[dep.ModID] = id
	}
	storeDeps := make([]store.ModDependency, 0, len(deps))
	for _, dep := range deps {
		storeDeps = append(storeDeps, store.ModDependency{
			ModID:      dep.ModID,
			Version:    dep.Version,
			ResolvedID: resolved[dep.ModID],
		})
	}

	tags := append([]string(nil), rel.Tags...)
	m, err := a.Store.AddMod(ctx, store.Mod{
		InstanceID: in.ID, ModID: detail.ModID, Name: detail.Name,
		Version: rel.ModVersion, Filename: filepath.Base(path),
		GameVersions: tags, Dependencies: storeDeps,
		InstalledAt: timeNow(),
	})
	if err != nil {
		return out, err
	}
	out.Mod = m
	out.Installed = append(out.Installed, m)

	if len(tags) > 0 && !versionInTags(tags, in.Version) {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s %s targets %s, but the server runs %s", detail.Name, rel.ModVersion, strings.Join(tags, ", "), in.Version))
	}

	for _, dep := range deps {
		depID := resolved[dep.ModID]
		if depID == 0 {
			continue
		}
		installed, err := a.Store.GetMod(ctx, in.ID, depID)
		if err == nil {
			if !versionsMatch(dep.Version, installed.Version) {
				out.Warnings = append(out.Warnings, fmt.Sprintf("%s requires %s %s, but %s is installed", detail.Name, dep.ModID, dep.Version, installed.Version))
			}
			continue
		}
		if visiting[depID] {
			out.Warnings = append(out.Warnings, fmt.Sprintf("Skipped dependency %s of %s: cycle detected", dep.ModID, detail.Name))
			continue
		}
		sub, err := a.installModTree(ctx, in, depID, 0, visiting, depth+1)
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("Could not install %s required by %s: %v", dep.ModID, detail.Name, err))
			continue
		}
		out.Installed = append(out.Installed, sub.Installed...)
		out.Warnings = append(out.Warnings, sub.Warnings...)
	}
	return out, nil
}

func versionInTags(tags []string, version string) bool {
	if version == "" {
		return true
	}
	for _, tag := range tags {
		if tag == version || strings.HasPrefix(tag, version+"-") || strings.HasPrefix(version, tag+".") {
			return true
		}
	}
	return false
}

func (a *App) apiListMods(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleViewer) == nil {
		return
	}
	in, ok := a.withInstance(w, r)
	if !ok {
		return
	}
	mods, err := a.Store.ListMods(r.Context(), in.ID)
	if err != nil {
		a.fail(w, 500, err)
		return
	}
	a.json(w, 200, buildModViews(mods, in.Version))
}

func (a *App) apiInstallMod(w http.ResponseWriter, r *http.Request) {
	u := a.require(w, r, store.RoleOperator)
	if u == nil {
		return
	}
	in, ok := a.withInstance(w, r)
	if !ok {
		return
	}
	var req struct {
		ModID     int `json:"modId"`
		ReleaseID int `json:"releaseId"`
	}
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, 400, err)
		return
	}
	out, err := a.installMod(r.Context(), in, req.ModID, req.ReleaseID)
	if err != nil {
		a.fail(w, 502, err)
		return
	}
	detail := "Installed " + out.Mod.Name + " " + out.Mod.Version
	if len(out.Installed) > 1 {
		detail += fmt.Sprintf(" with %d dependencies", len(out.Installed)-1)
	}
	a.audit(r, "mod.install", in.Name+": "+detail)
	a.Notifier.Send(r.Context(), notify.Event{Instance: in.Name, Type: "other", Detail: detail, Time: timeNow()})
	a.json(w, 201, out)
}

func (a *App) apiDeleteMod(w http.ResponseWriter, r *http.Request) {
	u := a.require(w, r, store.RoleOperator)
	if u == nil {
		return
	}
	in, ok := a.withInstance(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("mid"), 10, 64)
	if err != nil {
		a.fail(w, 400, errBadRequest("invalid mod id"))
		return
	}
	mods, err := a.Store.ListMods(r.Context(), in.ID)
	if err != nil {
		a.fail(w, 500, err)
		return
	}
	var target *store.Mod
	for i := range mods {
		if mods[i].ID == id {
			target = &mods[i]
			break
		}
	}
	if target == nil {
		a.fail(w, 404, errBadRequest("mod not found"))
		return
	}
	if dependents := findDependents(mods, *target); len(dependents) > 0 && r.URL.Query().Get("force") != "true" {
		a.fail(w, 409, fmt.Errorf("%s is required by %s; pass force=true to remove it anyway", target.Name, strings.Join(dependents, ", ")))
		return
	}
	// Move the archive into ModsDisabled instead of deleting it outright.
	modsDir := filepath.Join(in.DataPath, "Mods")
	disabledDir := filepath.Join(in.DataPath, "ModsDisabled")
	_ = os.MkdirAll(disabledDir, 0o755)
	src := filepath.Join(modsDir, filepath.Base(target.Filename))
	if _, err := os.Stat(src); err == nil {
		if err := os.Rename(src, filepath.Join(disabledDir, filepath.Base(target.Filename))); err != nil {
			a.fail(w, 500, err)
			return
		}
	}
	if err := a.Store.DeleteMod(r.Context(), id); err != nil {
		a.fail(w, 500, err)
		return
	}
	a.audit(r, "mod.remove", in.Name+": "+target.Name)
	a.json(w, 200, map[string]bool{"removed": true})
}

func (a *App) apiPinMod(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleOperator) == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("mid"), 10, 64)
	if err != nil {
		a.fail(w, 400, errBadRequest("invalid mod id"))
		return
	}
	var req struct {
		Pinned bool `json:"pinned"`
	}
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, 400, err)
		return
	}
	if err := a.Store.SetModPinned(r.Context(), id, req.Pinned); err != nil {
		a.fail(w, 500, err)
		return
	}
	a.json(w, 200, map[string]bool{"pinned": req.Pinned})
}

func (a *App) apiModDBSearch(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleViewer) == nil {
		return
	}
	limit := parseInt(r.URL.Query().Get("limit"), 20)
	if limit < 1 || limit > 100 {
		limit = 20
	}
	mods, err := a.ModDB.Search(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("gv"), limit)
	if err != nil {
		a.fail(w, 502, err)
		return
	}
	a.json(w, 200, mods)
}

func (a *App) apiModDBDetail(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleViewer) == nil {
		return
	}
	detail, err := a.ModDB.Detail(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, 502, err)
		return
	}
	gv := r.URL.Query().Get("gv")
	var compatible []moddb.Release
	if gv != "" {
		compatible = a.ModDB.Compatible(detail, gv)
	}
	a.json(w, 200, map[string]any{"mod": detail, "compatible": compatible})
}
