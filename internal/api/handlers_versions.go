package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackm/chronohost/internal/binaries"
	"github.com/jackm/chronohost/internal/store"
)

func (a *App) versions(ctx context.Context, channel string) ([]binaries.Release, error) {
	if channel == "all" {
		stable, err := a.versions(ctx, "stable")
		if err != nil {
			return nil, err
		}
		unstable, err := a.versions(ctx, "unstable")
		if err != nil {
			return stable, nil // unstable feed is optional
		}
		seen := map[string]bool{}
		out := make([]binaries.Release, 0, len(stable)+len(unstable))
		for _, rel := range append(stable, unstable...) {
			if !seen[rel.Version] {
				seen[rel.Version] = true
				out = append(out, rel)
			}
		}
		return sortedVersions(out), nil
	}
	a.versionMu.Lock()
	cached, ok := a.versionCache[channel]
	a.versionMu.Unlock()
	if ok && time.Since(cached.at) < 10*time.Minute {
		return cached.rel, nil
	}
	rels, err := a.Binaries.List(ctx, channel)
	if err != nil {
		if ok {
			return cached.rel, nil // serve stale cache when the feed is down
		}
		return nil, err
	}
	a.versionMu.Lock()
	a.versionCache[channel] = cachedVersions{at: time.Now(), rel: rels}
	a.versionMu.Unlock()
	return rels, nil
}

func (a *App) apiVersions(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleViewer) == nil {
		return
	}
	stable, errStable := a.versions(r.Context(), "stable")
	unstable, errUnstable := a.versions(r.Context(), "unstable")
	latest, errLatest := a.Binaries.Latest(r.Context())
	if errStable != nil && errUnstable != nil {
		a.fail(w, 502, errStable)
		return
	}
	cached := a.Binaries.Cached()
	for i := range stable {
		stable[i].Latest = stable[i].Version == latest
	}
	a.json(w, 200, map[string]any{
		"stable": stable, "unstable": unstable, "cached": cached,
		"latest": latest, "errors": map[string]string{
			"stable": errString(errStable), "unstable": errString(errUnstable), "latest": errString(errLatest),
		},
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (a *App) apiDownloadVersion(w http.ResponseWriter, r *http.Request) {
	u := a.require(w, r, store.RoleOperator)
	if u == nil {
		return
	}
	var req struct {
		Version string `json:"version"`
		Channel string `json:"channel"`
	}
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, 400, err)
		return
	}
	if strings.TrimSpace(req.Version) == "" {
		a.fail(w, 400, errBadRequest("version is required"))
		return
	}
	if req.Channel == "" {
		req.Channel = "stable"
	}
	if err := a.ensureVersion(r.Context(), req.Version); err != nil {
		a.fail(w, 502, err)
		return
	}
	a.audit(r, "version.download", req.Version)
	a.json(w, 200, map[string]any{"version": req.Version, "cached": true, "dll": func() string {
		dll, _, err := a.Binaries.DLL(req.Version)
		if err != nil {
			return ""
		}
		return dll
	}()})
}

func (a *App) apiDeleteVersion(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleAdmin) == nil {
		return
	}
	version := r.PathValue("version")
	if err := a.Binaries.Remove(version); err != nil {
		a.fail(w, 500, err)
		return
	}
	a.audit(r, "version.delete", version)
	a.json(w, 200, map[string]bool{"deleted": true})
}
