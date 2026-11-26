package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackm/chronohost/internal/backup"
	"github.com/jackm/chronohost/internal/notify"
	"github.com/jackm/chronohost/internal/store"
)

func snapshotFromRecord(b store.Backup) backup.Snapshot {
	return backup.Snapshot{
		ID: fmt.Sprintf("%d", b.ID), InstanceID: b.InstanceID, Name: b.Name,
		Path: b.Path, Size: b.Size, Kind: b.Kind, CreatedAt: b.CreatedAt, Files: b.Files,
	}
}

func (a *App) retentionPolicy(ctx context.Context) backup.Policy {
	return backup.Policy{
		Hourly:  parseInt(a.Store.GetSetting(ctx, "backup_hourly", "24"), 24),
		Daily:   parseInt(a.Store.GetSetting(ctx, "backup_daily", "30"), 30),
		Weekly:  parseInt(a.Store.GetSetting(ctx, "backup_weekly", "8"), 8),
		Monthly: parseInt(a.Store.GetSetting(ctx, "backup_monthly", "12"), 12),
	}
}

func (a *App) createBackup(ctx context.Context, in store.Instance, kind string) (store.Backup, error) {
	snap, err := a.Backups.Create(in.ID, in.DataPath, kind)
	if err != nil {
		return store.Backup{}, err
	}
	record, err := a.Store.AddBackup(ctx, store.Backup{
		InstanceID: in.ID, Name: snap.Name, Path: snap.Path, Size: snap.Size,
		Kind: kind, CreatedAt: snap.CreatedAt, Files: snap.Files,
	})
	if err != nil {
		return store.Backup{}, err
	}
	kept, err := a.Backups.ApplyRetention(in.ID, a.retentionPolicy(ctx))
	if err != nil {
		a.Log.Printf("retention for %s: %v", in.Name, err)
		return record, nil
	}
	keepPaths := map[string]bool{}
	for _, k := range kept {
		keepPaths[k.Path] = true
	}
	records, _ := a.Store.ListBackups(ctx, in.ID)
	for _, r := range records {
		if !keepPaths[r.Path] && r.ID != record.ID {
			if err := a.Backups.Delete(snapshotFromRecord(r)); err == nil {
				_ = a.Store.DeleteBackup(ctx, r.ID)
			}
		}
	}
	return record, nil
}

func (a *App) apiListBackups(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleViewer) == nil {
		return
	}
	in, ok := a.withInstance(w, r)
	if !ok {
		return
	}
	records, err := a.Store.ListBackups(r.Context(), in.ID)
	if err != nil {
		a.fail(w, 500, err)
		return
	}
	a.json(w, 200, records)
}

func (a *App) apiCreateBackup(w http.ResponseWriter, r *http.Request) {
	u := a.require(w, r, store.RoleOperator)
	if u == nil {
		return
	}
	in, ok := a.withInstance(w, r)
	if !ok {
		return
	}
	var req struct {
		Kind string `json:"kind"`
	}
	_ = decodeJSON(r, &req)
	if req.Kind == "" {
		req.Kind = "manual"
	}
	record, err := a.createBackup(r.Context(), in, req.Kind)
	if err != nil {
		a.fail(w, 500, err)
		return
	}
	a.Notifier.Send(r.Context(), notify.Event{Instance: in.Name, Type: "backup", Detail: "Manual snapshot " + record.Name, Time: time.Now()})
	a.audit(r, "backup.create", in.Name+": "+record.Name)
	a.json(w, 201, record)
}

func (a *App) apiRestoreBackup(w http.ResponseWriter, r *http.Request) {
	u := a.require(w, r, store.RoleOperator)
	if u == nil {
		return
	}
	in, ok := a.withInstance(w, r)
	if !ok {
		return
	}
	var req struct {
		Confirm bool `json:"confirm"`
		Restart bool `json:"restart"`
	}
	if err := decodeJSON(r, &req); err != nil {
		a.fail(w, 400, err)
		return
	}
	if !req.Confirm {
		a.fail(w, 400, errBadRequest("set confirm=true to restore a snapshot"))
		return
	}
	id, err := strconv.ParseInt(r.PathValue("bid"), 10, 64)
	if err != nil {
		a.fail(w, 400, errBadRequest("invalid backup id"))
		return
	}
	record, err := a.Store.GetBackup(r.Context(), id)
	if err != nil {
		a.fail(w, 404, err)
		return
	}
	wasRunning := a.Procs.Running(in.ID)
	if wasRunning {
		if err := a.Procs.Stop(in.ID, true); err != nil {
			a.fail(w, 500, err)
			return
		}
	}
	if err := a.Backups.Restore(snapshotFromRecord(record), in.DataPath); err != nil {
		a.fail(w, 500, err)
		return
	}
	_ = a.Store.SetInstanceStatus(r.Context(), in.ID, "stopped")
	if wasRunning || req.Restart {
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
	}
	a.Notifier.Send(r.Context(), notify.Event{Instance: in.Name, Type: "backup", Detail: "Restored snapshot " + record.Name, Time: time.Now()})
	a.audit(r, "backup.restore", in.Name+": "+record.Name)
	a.json(w, 200, map[string]bool{"restored": true})
}

func (a *App) apiDeleteBackup(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r, store.RoleOperator) == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("bid"), 10, 64)
	if err != nil {
		a.fail(w, 400, errBadRequest("invalid backup id"))
		return
	}
	record, err := a.Store.GetBackup(r.Context(), id)
	if err != nil {
		a.fail(w, 404, err)
		return
	}
	if err := a.Backups.Delete(snapshotFromRecord(record)); err != nil {
		a.fail(w, 500, err)
		return
	}
	if err := a.Store.DeleteBackup(r.Context(), id); err != nil {
		a.fail(w, 500, err)
		return
	}
	a.json(w, 200, map[string]bool{"deleted": true})
}
