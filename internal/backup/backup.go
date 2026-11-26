// Package backup creates, lists, restores and prunes compressed snapshots of
// Vintage Story server instances.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Policy describes how many snapshots to retain per Grandfather-Father-Son
// bucket. Zero means "keep no snapshots in that bucket" (the newest overall
// snapshot is always kept).
type Policy struct {
	Hourly  int `json:"hourly"`
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
}

// DefaultPolicy is the recommended retention policy.
func DefaultPolicy() Policy {
	return Policy{Hourly: 24, Daily: 30, Weekly: 8, Monthly: 12}
}

// Snapshot is the metadata describing one backup archive.
type Snapshot struct {
	ID         string    `json:"id"`
	InstanceID string    `json:"instanceId"`
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	Kind       string    `json:"kind"` // manual|scheduled|pre-update|pre-restore
	CreatedAt  time.Time `json:"createdAt"`
	Files      []string  `json:"files"`
}

// Engine manages backup archives below root (normally /data/backups).
type Engine struct {
	root string
	logf func(format string, args ...any)

	mu sync.Mutex
}

// New returns an Engine storing archives under root. A nil logf disables
// logging.
func New(root string, logf func(format string, args ...any)) *Engine {
	if root == "" {
		root = "."
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Engine{root: filepath.Clean(root), logf: logf}
}

func (e *Engine) log(format string, args ...any) {
	if e != nil && e.logf != nil {
		e.logf(format, args...)
	}
}

// Create writes a tar.gz snapshot of the save files, server configuration and
// installed mods of an instance. It returns an error when no .vcdbs save file
// is found.
func (e *Engine) Create(instanceID, dataPath, kind string) (Snapshot, error) {
	var zero Snapshot
	if e == nil {
		return zero, errors.New("backup: nil engine")
	}
	instanceID, err := cleanInstanceID(instanceID)
	if err != nil {
		return zero, err
	}
	if kind = cleanKind(kind); kind == "" {
		return zero, errors.New("backup: empty backup kind")
	}
	info, err := os.Stat(dataPath)
	if err != nil {
		return zero, fmt.Errorf("backup: stat instance data: %w", err)
	}
	if !info.IsDir() {
		return zero, fmt.Errorf("backup: instance data %q is not a directory", dataPath)
	}

	files, err := collectFiles(dataPath)
	if err != nil {
		return zero, err
	}
	saveFound := false
	for _, f := range files {
		if strings.EqualFold(filepath.Ext(f.rel), ".vcdbs") {
			saveFound = true
			break
		}
	}
	if !saveFound {
		return zero, fmt.Errorf("backup: no save file found under %q", dataPath)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	dir := filepath.Join(e.root, instanceID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return zero, fmt.Errorf("backup: create backup directory: %w", err)
	}

	now := time.Now().UTC()
	id, err := randomID()
	if err != nil {
		return zero, err
	}
	base := fmt.Sprintf("%s-%s-%s.tar.gz", now.Format("20060102-150405"), kind, id[:8])
	archive := filepath.Join(dir, base)

	tmp, err := os.CreateTemp(dir, ".snapshot-*.tar.gz")
	if err != nil {
		return zero, fmt.Errorf("backup: create temporary archive: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := writeArchive(tmp, dataPath, files); err != nil {
		tmp.Close()
		return zero, err
	}
	if err := tmp.Close(); err != nil {
		return zero, fmt.Errorf("backup: close archive: %w", err)
	}
	st, err := os.Stat(tmpName)
	if err != nil {
		return zero, fmt.Errorf("backup: stat archive: %w", err)
	}

	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.rel)
	}
	snap := Snapshot{
		ID:         id,
		InstanceID: instanceID,
		Name:       base,
		Path:       archive,
		Size:       st.Size(),
		Kind:       kind,
		CreatedAt:  now,
		Files:      names,
	}
	if err := writeSidecar(dir, base, snap); err != nil {
		return zero, err
	}
	if err := os.Rename(tmpName, archive); err != nil {
		removeSidecars(archive)
		return zero, fmt.Errorf("backup: publish archive: %w", err)
	}

	e.log("backup: created %s (%d files, %d bytes)", archive, len(files), snap.Size)
	return snap, nil
}

// List returns all snapshots of an instance, newest first. Archives without a
// readable sidecar are still listed using their file metadata.
func (e *Engine) List(instanceID string) ([]Snapshot, error) {
	if e == nil {
		return nil, errors.New("backup: nil engine")
	}
	instanceID, err := cleanInstanceID(instanceID)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	return e.listLocked(instanceID)
}

func (e *Engine) listLocked(instanceID string) ([]Snapshot, error) {
	dir := filepath.Join(e.root, instanceID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []Snapshot{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("backup: read backup directory: %w", err)
	}

	snaps := make([]Snapshot, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".tar.gz") {
			continue
		}
		archive := filepath.Join(dir, entry.Name())
		st, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("backup: stat archive %q: %w", archive, err)
		}
		snap, ok := readSidecar(archive)
		if !ok {
			snap = Snapshot{}
		}
		// The on-disk location is authoritative, never the sidecar's copy.
		snap.Path = archive
		snap.Name = entry.Name()
		if snap.ID == "" {
			snap.ID = strings.TrimSuffix(entry.Name(), ".tar.gz")
		}
		if snap.InstanceID == "" {
			snap.InstanceID = instanceID
		}
		if snap.CreatedAt.IsZero() {
			snap.CreatedAt = st.ModTime()
		}
		if snap.Size <= 0 {
			snap.Size = st.Size()
		}
		snaps = append(snaps, snap)
	}

	sort.Slice(snaps, func(i, j int) bool {
		if !snaps[i].CreatedAt.Equal(snaps[j].CreatedAt) {
			return snaps[i].CreatedAt.After(snaps[j].CreatedAt)
		}
		return snaps[i].Name > snaps[j].Name
	})
	return snaps, nil
}

// Delete removes an archive and its sidecar. Paths outside the backup root are
// rejected.
func (e *Engine) Delete(s Snapshot) error {
	if e == nil {
		return errors.New("backup: nil engine")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deleteLocked(s)
}

func (e *Engine) deleteLocked(s Snapshot) error {
	archive, err := e.archivePath(s.Path)
	if err != nil {
		return err
	}
	if err := os.Remove(archive); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: remove archive: %w", err)
	}
	removeSidecars(archive)
	e.log("backup: deleted %s", archive)
	return nil
}

// Close releases resources held by the engine. No resources are currently
// held between calls.
func (e *Engine) Close() error { return nil }

// archivePath verifies that p points at a file inside the backup root.
func (e *Engine) archivePath(p string) (string, error) {
	if p == "" {
		return "", errors.New("backup: empty snapshot path")
	}
	root, err := filepath.Abs(e.root)
	if err != nil {
		return "", fmt.Errorf("backup: resolve backup root: %w", err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("backup: resolve snapshot path: %w", err)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("backup: snapshot path %q is outside backup root", p)
	}
	return abs, nil
}

type sourceFile struct {
	rel  string // slash-separated path inside the archive
	abs  string
	mode fs.FileMode
	size int64
}

// collectFiles finds the files included in a snapshot. Symlinks and anything
// that is not a regular file are skipped.
func collectFiles(dataPath string) ([]sourceFile, error) {
	var files []sourceFile
	add := func(abs string, allow func(string) bool) error {
		info, err := os.Lstat(abs)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("backup: stat %q: %w", abs, err)
		}
		if !info.Mode().IsRegular() || !allow(info.Name()) {
			return nil
		}
		rel, err := filepath.Rel(dataPath, abs)
		if err != nil {
			return fmt.Errorf("backup: relative path for %q: %w", abs, err)
		}
		files = append(files, sourceFile{
			rel:  filepath.ToSlash(rel),
			abs:  abs,
			mode: info.Mode().Perm(),
			size: info.Size(),
		})
		return nil
	}

	saves := filepath.Join(dataPath, "Saves")
	err := filepath.WalkDir(saves, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("backup: walk saves: %w", err)
		}
		if p == saves || d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		return add(p, func(name string) bool {
			ext := strings.ToLower(filepath.Ext(name))
			return ext == ".vcdbs" || ext == ".json"
		})
	})
	if err != nil {
		return nil, err
	}

	if err := add(filepath.Join(dataPath, "serverconfig.json"), func(string) bool { return true }); err != nil {
		return nil, err
	}

	mods := filepath.Join(dataPath, "Mods")
	entries, err := os.ReadDir(mods)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("backup: read mods: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), ".zip") {
			if err := add(filepath.Join(mods, entry.Name()), func(string) bool { return true }); err != nil {
				return nil, err
			}
		}
	}

	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
}

func writeArchive(w io.Writer, dataPath string, files []sourceFile) error {
	gzw := gzip.NewWriter(w)
	tw := tar.NewWriter(gzw)
	for _, f := range files {
		hdr := &tar.Header{
			Name:     f.rel,
			Mode:     int64(f.mode),
			Size:     f.size,
			ModTime:  time.Now().UTC(),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("backup: write tar header for %q: %w", f.rel, err)
		}
		src, err := os.Open(f.abs)
		if err != nil {
			return fmt.Errorf("backup: open %q: %w", f.abs, err)
		}
		_, copyErr := io.Copy(tw, src)
		closeErr := src.Close()
		if copyErr != nil {
			return fmt.Errorf("backup: copy %q into archive: %w", f.rel, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("backup: close %q: %w", f.abs, closeErr)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("backup: close tar writer: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return fmt.Errorf("backup: close gzip writer: %w", err)
	}
	return nil
}

func sidecars(archive string) [2]string {
	dir, base := filepath.Split(archive)
	stem := strings.TrimSuffix(base, ".tar.gz")
	return [2]string{
		filepath.Join(dir, stem+".json"),
		archive + ".json",
	}
}

func writeSidecar(dir, archiveBase string, snap Snapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: encode metadata: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".metadata-*.json")
	if err != nil {
		return fmt.Errorf("backup: create temporary metadata: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("backup: write metadata: %w", err)
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return fmt.Errorf("backup: chmod metadata: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("backup: sync metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("backup: close metadata: %w", err)
	}
	target := sidecars(filepath.Join(dir, archiveBase))[0]
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("backup: publish metadata: %w", err)
	}
	return nil
}

func readSidecar(archive string) (Snapshot, bool) {
	var snap Snapshot
	for _, p := range sidecars(archive) {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(data, &snap); err == nil {
			return snap, true
		}
	}
	return snap, false
}

func removeSidecars(archive string) {
	for _, p := range sidecars(archive) {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			// Best effort: a stale sidecar is ignored by List if unreadable.
			continue
		}
	}
}

func cleanInstanceID(id string) (string, error) {
	if id == "" || id == "." || id == ".." {
		return "", fmt.Errorf("backup: invalid instance ID %q", id)
	}
	for _, r := range id {
		if r == '/' || r == '\\' || r == 0 {
			return "", fmt.Errorf("backup: invalid instance ID %q", id)
		}
	}
	return id, nil
}

func cleanKind(kind string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(kind)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-_")
}

func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("backup: generate snapshot id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
