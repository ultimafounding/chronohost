package backup_test

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackm/chronohost/internal/backup"
)

func fixture(t *testing.T) (root, data string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "backups")
	data = filepath.Join(t.TempDir(), "instance-1")
	mustMkdir(t, filepath.Join(data, "Saves"))
	mustMkdir(t, filepath.Join(data, "Mods"))
	mustWrite(t, filepath.Join(data, "Saves", "test.vcdbs"), []byte("original save"), 0640)
	mustWrite(t, filepath.Join(data, "Saves", "world.json"), []byte(`{"name":"default"}`), 0644)
	mustWrite(t, filepath.Join(data, "serverconfig.json"), []byte(`{"port":42420}`), 0644)
	mustWrite(t, filepath.Join(data, "Mods", "a.zip"), []byte("zip data"), 0644)
	return root, data
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, data, mode); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newEngine(t *testing.T, root string) *backup.Engine {
	t.Helper()
	e := backup.New(root, func(format string, args ...any) {
		t.Logf("backup: "+format, args...)
	})
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return e
}

func TestDefaultPolicy(t *testing.T) {
	want := backup.Policy{Hourly: 24, Daily: 30, Weekly: 8, Monthly: 12}
	if got := backup.DefaultPolicy(); got != want {
		t.Fatalf("DefaultPolicy() = %+v, want %+v", got, want)
	}
}

func TestCreateList(t *testing.T) {
	root, data := fixture(t)
	e := newEngine(t, root)

	first, err := e.Create("instance-1", data, "manual")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if first.ID == "" || first.InstanceID != "instance-1" || first.Kind != "manual" {
		t.Errorf("unexpected snapshot metadata: %+v", first)
	}
	if first.Size <= 0 || first.CreatedAt.IsZero() {
		t.Errorf("missing size or timestamp: %+v", first)
	}
	if filepath.Dir(first.Path) != filepath.Join(root, "instance-1") {
		t.Errorf("archive path %q outside instance directory", first.Path)
	}
	if !strings.HasSuffix(first.Path, "-manual-"+first.ID[:8]+".tar.gz") {
		t.Errorf("unexpected archive name %q", first.Path)
	}
	wantFiles := []string{"Mods/a.zip", "Saves/test.vcdbs", "Saves/world.json", "serverconfig.json"}
	if got := strings.Join(first.Files, ","); got != strings.Join(wantFiles, ",") {
		t.Errorf("Files = %v, want %v", first.Files, wantFiles)
	}
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(first.Path, ".tar.gz") + ".json"); err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}

	second, err := e.Create("instance-1", data, "scheduled")
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	snaps, err := e.List("instance-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("List returned %d snapshots, want 2", len(snaps))
	}
	if snaps[0].ID != second.ID || snaps[1].ID != first.ID {
		t.Errorf("List not newest-first: %+v", snaps)
	}

	// An archive without a sidecar is still listed using stat metadata.
	legacy := strings.TrimSuffix(first.Path, ".tar.gz") + "-legacy.tar.gz"
	b, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, legacy, b, 0644)
	snaps, err = e.List("instance-1")
	if err != nil {
		t.Fatalf("List with legacy archive: %v", err)
	}
	if len(snaps) != 3 {
		t.Fatalf("List returned %d snapshots, want 3", len(snaps))
	}
	found := false
	for _, s := range snaps {
		if s.Name == filepath.Base(legacy) {
			found = true
			if s.Size != int64(len(b)) || s.CreatedAt.IsZero() || s.InstanceID != "instance-1" {
				t.Errorf("legacy snapshot metadata = %+v", s)
			}
		}
	}
	if !found {
		t.Errorf("legacy archive not listed: %+v", snaps)
	}

	empty, err := e.List("no-such-instance")
	if err != nil || len(empty) != 0 {
		t.Errorf("List of unknown instance = %v, %v", empty, err)
	}
}

func TestCreateRequiresSaveFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backups")
	data := filepath.Join(t.TempDir(), "instance")
	mustMkdir(t, data)
	mustWrite(t, filepath.Join(data, "serverconfig.json"), []byte(`{}`), 0644)
	_, err := newEngine(t, root).Create("instance", data, "manual")
	if err == nil || !strings.Contains(err.Error(), "no save file") {
		t.Fatalf("Create without save file: got %v, want no-save-file error", err)
	}
}

func TestRestore(t *testing.T) {
	root, data := fixture(t)
	e := newEngine(t, root)
	snap, err := e.Create("instance-1", data, "manual")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate a live server having changed its files since the snapshot.
	mustWrite(t, filepath.Join(data, "Saves", "test.vcdbs"), []byte("new save"), 0644)
	mustWrite(t, filepath.Join(data, "Saves", "other.vcdbs"), []byte("other save"), 0644)
	if err := os.Remove(filepath.Join(data, "serverconfig.json")); err != nil {
		t.Fatal(err)
	}

	if err := e.Restore(snap, data); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := mustRead(t, filepath.Join(data, "Saves", "test.vcdbs")); got != "original save" {
		t.Errorf("restored save = %q", got)
	}
	info, err := os.Stat(filepath.Join(data, "Saves", "test.vcdbs"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0640 {
		t.Errorf("restored mode = %o, want 640", got)
	}
	if got := mustRead(t, filepath.Join(data, "serverconfig.json")); got != `{"port":42420}` {
		t.Errorf("restored config = %q", got)
	}
	if got := mustRead(t, filepath.Join(data, "Mods", "a.zip")); got != "zip data" {
		t.Errorf("restored mod = %q", got)
	}

	entries, err := os.ReadDir(filepath.Join(data, "Saves"))
	if err != nil {
		t.Fatal(err)
	}
	var safety string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "pre-restore-") && entry.IsDir() {
			safety = filepath.Join(data, "Saves", entry.Name())
		}
	}
	if safety == "" {
		t.Fatal("no pre-restore safety directory created")
	}
	if got := mustRead(t, filepath.Join(safety, "test.vcdbs")); got != "new save" {
		t.Errorf("stashed save = %q", got)
	}
	if got := mustRead(t, filepath.Join(safety, "other.vcdbs")); got != "other save" {
		t.Errorf("stashed other save = %q", got)
	}
}

func TestRestoreRejectsUnsafeArchive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backups")
	data := filepath.Join(t.TempDir(), "instance")
	mustMkdir(t, filepath.Join(data, "Saves"))
	mustMkdir(t, filepath.Join(root, "instance-1"))

	cases := []struct {
		name string
		hdr  tar.Header
	}{
		{"traversal", tar.Header{Name: "../escape.txt", Mode: 0644, Size: 3, Typeflag: tar.TypeReg}},
		{"absolute", tar.Header{Name: "/tmp/chronohost-escape.txt", Mode: 0644, Size: 3, Typeflag: tar.TypeReg}},
		{"hidden traversal", tar.Header{Name: "Saves/../escape.txt", Mode: 0644, Size: 3, Typeflag: tar.TypeReg}},
		{"symlink", tar.Header{Name: "Saves/link", Mode: 0777, Size: 0, Typeflag: tar.TypeSymlink, Linkname: "../../escape.txt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := filepath.Join(root, "instance-1", tc.name+".tar.gz")
			writeTarGz(t, archive, tc.hdr, []byte("bad"))
			err := newEngine(t, root).Restore(backup.Snapshot{Path: archive}, data)
			if err == nil {
				t.Fatal("Restore accepted an unsafe archive")
			}
			if !strings.Contains(err.Error(), "unsafe path") && !strings.Contains(err.Error(), "unsupported entry type") {
				t.Errorf("unexpected error: %v", err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(data), "escape.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("file escaped extraction directory: %v", err)
			}
		})
	}
}

type tarEntry struct {
	hdr  tar.Header
	body []byte
}

func writeTarGz(t *testing.T, path string, hdr tar.Header, body []byte) {
	t.Helper()
	writeTarGzEntries(t, path, []tarEntry{{hdr: hdr, body: body}})
}

func writeTarGzEntries(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if e.hdr.Typeflag == tar.TypeReg || e.hdr.Typeflag == tar.TypeRegA {
			if e.hdr.Size == 0 && len(e.body) > 0 {
				e.hdr.Size = int64(len(e.body))
			}
		} else {
			e.hdr.Size = 0
		}
		if err := tw.WriteHeader(&e.hdr); err != nil {
			t.Fatal(err)
		}
		if e.hdr.Size > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDelete(t *testing.T) {
	root, data := fixture(t)
	e := newEngine(t, root)
	snap, err := e.Create("instance-1", data, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Delete(snap); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(snap.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("archive still present: %v", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(snap.Path, ".tar.gz") + ".json"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sidecar still present: %v", err)
	}

	outside := filepath.Join(t.TempDir(), "outside.tar.gz")
	mustWrite(t, outside, []byte("keep me"), 0644)
	if err := e.Delete(backup.Snapshot{Path: outside}); err == nil {
		t.Error("Delete accepted a path outside the backup root")
	}
	if got := mustRead(t, outside); got != "keep me" {
		t.Errorf("outside file was modified: %q", got)
	}
}

func TestApplyRetention(t *testing.T) {
	root, data := fixture(t)
	e := newEngine(t, root)

	times := []time.Time{
		time.Date(2026, 11, 26, 12, 30, 0, 0, time.UTC), // newest overall
		time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC),  // same hour/day/week/month as newest
		time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),  // older week/month
		time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),   // same hour as previous
		time.Date(2024, 1, 15, 10, 45, 0, 0, time.UTC),  // newest of older week, same hour as snapshots 2 and 3
	}
	var snaps []backup.Snapshot
	for i, when := range times {
		snap, err := e.Create("instance-1", data, fmt.Sprintf("scheduled-%d", i))
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		snap.CreatedAt = when
		meta, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(strings.TrimSuffix(snap.Path, ".tar.gz")+".json", meta, 0644); err != nil {
			t.Fatal(err)
		}
		snaps = append(snaps, snap)
	}

	kept, err := e.ApplyRetention("instance-1", backup.Policy{Hourly: 1, Daily: 1, Weekly: 1, Monthly: 1})
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if len(kept) != 2 || kept[0].ID != snaps[0].ID || kept[1].ID != snaps[4].ID {
		t.Fatalf("kept = %+v, want snapshots 0 and 4", kept)
	}
	for i, s := range snaps {
		_, err := os.Stat(s.Path)
		wantGone := i != 0 && i != 4
		if wantGone && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("snapshot %d should have been deleted (stat error %v)", i, err)
		}
		if !wantGone && err != nil {
			t.Errorf("snapshot %d should have been kept: %v", i, err)
		}
	}
	remaining, err := e.List("instance-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Errorf("List after retention = %d snapshots, want 2", len(remaining))
	}
}
