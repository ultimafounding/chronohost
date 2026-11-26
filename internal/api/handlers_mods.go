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

	"github.com/jackm/chronohost/internal/moddb"
	"github.com/jackm/chronohost/internal/notify"
	"github.com/jackm/chronohost/internal/store"
)

func modDependencies(path string) []string {
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
		out := make([]string, 0, len(deps))
		for _, dep := range deps {
			if !dep.Enabled || dep.ModID == "" || strings.EqualFold(dep.ModID, "game") {
				continue
			}
			out = append(out, dep.ModID)
		}
		return out
	}
	return nil
}

func (a *App) installMod(ctx context.Context, in store.Instance, modID, releaseID int) (store.Mod, []string, error) {
	detail, err := a.ModDB.Detail(ctx, strconv.Itoa(modID))
	if err != nil {
		return store.Mod{}, nil, err
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
		return store.Mod{}, nil, fmt.Errorf("no suitable release found for %s", detail.Name)
	}
	modsDir := filepath.Join(in.DataPath, "Mods")
	if err := os.MkdirAll(modsDir, 0o755); err != nil {
		return store.Mod{}, nil, err
	}
	path, err := a.ModDB.Download(ctx, rel, modsDir, nil)
	if err != nil {
		return store.Mod{}, nil, err
	}
	tags := append([]string(nil), rel.Tags...)
	var warnings []string
	if len(tags) > 0 && !versionInTags(tags, in.Version) {
		warnings = append(warnings, fmt.Sprintf("%s %s targets %s, but the server runs %s", detail.Name, rel.ModVersion, strings.Join(tags, ", "), in.Version))
	}
	m, err := a.Store.AddMod(ctx, store.Mod{
		InstanceID: in.ID, ModID: detail.ModID, Name: detail.Name,
		Version: rel.ModVersion, Filename: filepath.Base(path),
		GameVersions: tags, Dependencies: modDependencies(path),
		InstalledAt: timeNow(),
	})
	if err != nil {
		return store.Mod{}, warnings, err
	}
	for _, dep := range m.Dependencies {
		warnings = append(warnings, fmt.Sprintf("%s requires %s — verify it is installed", detail.Name, dep))
	}
	return m, warnings, nil
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
	type modView struct {
		store.Mod
		Compatible bool `json:"compatible"`
	}
	views := make([]modView, 0, len(mods))
	for _, m := range mods {
		views = append(views, modView{Mod: m, Compatible: len(m.GameVersions) == 0 || versionInTags(m.GameVersions, in.Version)})
	}
	a.json(w, 200, views)
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
	m, warnings, err := a.installMod(r.Context(), in, req.ModID, req.ReleaseID)
	if err != nil {
		a.fail(w, 502, err)
		return
	}
	a.audit(r, "mod.install", in.Name+": "+m.Name+" "+m.Version)
	a.Notifier.Send(r.Context(), notify.Event{Instance: in.Name, Type: "other", Detail: "Installed " + m.Name + " " + m.Version, Time: timeNow()})
	a.json(w, 201, map[string]any{"mod": m, "warnings": warnings})
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
