package backup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Restore extracts a snapshot into dataPath. Existing .vcdbs files are moved
// into Saves/pre-restore-<timestamp>/ before the archived files are written.
// The caller must stop the server before calling Restore and restart it
// afterwards.
func (e *Engine) Restore(s Snapshot, dataPath string) error {
	if e == nil {
		return errors.New("backup: nil engine")
	}
	archive, err := e.archivePath(s.Path)
	if err != nil {
		return err
	}
	if dataPath == "" {
		return errors.New("backup: empty data path")
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	stage, err := os.MkdirTemp("", "chronohost-restore-")
	if err != nil {
		return fmt.Errorf("backup: create staging directory: %w", err)
	}
	defer os.RemoveAll(stage)

	if err := extractArchive(archive, stage); err != nil {
		return err
	}
	if err := os.MkdirAll(dataPath, 0755); err != nil {
		return fmt.Errorf("backup: create data directory: %w", err)
	}
	saves := filepath.Join(dataPath, "Saves")
	if err := os.MkdirAll(saves, 0755); err != nil {
		return fmt.Errorf("backup: create saves directory: %w", err)
	}
	if err := stashSaves(saves); err != nil {
		return err
	}
	if err := installFiles(stage, dataPath); err != nil {
		return err
	}

	e.log("backup: restored %s into %s", archive, dataPath)
	return nil
}

// extractArchive unpacks a snapshot into stage, rejecting symlinks, links,
// device files and any path that escapes stage.
func extractArchive(archive, stage string) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("backup: open archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("backup: open gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	seen := make(map[string]bool)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("backup: read archive: %w", err)
		}

		rel, err := safeArchivePath(hdr.Name)
		if err != nil {
			return fmt.Errorf("backup: unsafe path %q in archive: %w", hdr.Name, err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			// A "." entry is just the archive root.
		case tar.TypeReg, tar.TypeRegA:
			if rel == "" {
				return fmt.Errorf("backup: invalid file entry %q in archive", hdr.Name)
			}
		default:
			return fmt.Errorf("backup: unsupported entry type %q for %q in archive", hdr.Typeflag, hdr.Name)
		}
		if rel == "" {
			continue
		}
		if seen[rel] {
			return fmt.Errorf("backup: duplicate path %q in archive", rel)
		}
		seen[rel] = true

		target := filepath.Join(stage, rel)
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("backup: create directory %q: %w", rel, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return fmt.Errorf("backup: create parent of %q: %w", rel, err)
		}
		mode := fs.FileMode(hdr.Mode).Perm()
		if mode == 0 {
			mode = 0644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return fmt.Errorf("backup: create extracted file %q: %w", rel, err)
		}
		_, copyErr := io.Copy(out, tr)
		chmodErr := out.Chmod(mode) // defeat a restrictive umask
		closeErr := out.Close()
		if copyErr != nil || chmodErr != nil || closeErr != nil {
			return fmt.Errorf("backup: write extracted file %q: %v %v %v", rel, copyErr, chmodErr, closeErr)
		}
	}
	return nil
}

// safeArchivePath converts an archive entry name to a path relative to the
// extraction directory. Anything absolute, traversing upwards, or containing
// Windows separators is rejected.
func safeArchivePath(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", errors.New("invalid path")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", errors.New("path contains '..'")
		}
	}
	cleaned := path.Clean(name)
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return "", errors.New("path escapes extraction directory")
	}
	rel := filepath.FromSlash(cleaned)
	if rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes extraction directory")
	}
	return rel, nil
}

// stashSaves moves the current save files into a safety directory.
func stashSaves(saves string) error {
	base := fmt.Sprintf("pre-restore-%s", time.Now().UTC().Format("20060102-150405.000000000"))
	safety := filepath.Join(saves, base)
	for n := 1; ; n++ {
		if _, err := os.Lstat(safety); errors.Is(err, fs.ErrNotExist) {
			break
		} else if err != nil {
			return fmt.Errorf("backup: check safety directory: %w", err)
		}
		safety = filepath.Join(saves, fmt.Sprintf("%s-%d", base, n))
	}
	if err := os.Mkdir(safety, 0755); err != nil {
		return fmt.Errorf("backup: create safety directory: %w", err)
	}

	entries, err := os.ReadDir(saves)
	if err != nil {
		return fmt.Errorf("backup: read saves directory: %w", err)
	}
	for _, entry := range entries {
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".vcdbs") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("backup: stat save %q: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		from := filepath.Join(saves, entry.Name())
		to := filepath.Join(safety, entry.Name())
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("backup: preserve existing save %q: %w", entry.Name(), err)
		}
	}
	return nil
}

// installFiles copies the staged archive contents into dataPath. Each file is
// written to a temporary file in its destination directory and renamed into
// place so a failed copy cannot leave a half-written file.
func installFiles(stage, dataPath string) error {
	return filepath.WalkDir(stage, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("backup: walk staged files: %w", err)
		}
		rel, err := filepath.Rel(stage, p)
		if err != nil {
			return fmt.Errorf("backup: staged relative path: %w", err)
		}
		if rel == "." {
			return nil
		}
		dst := filepath.Join(dataPath, rel)
		if d.IsDir() {
			if err := os.MkdirAll(dst, 0755); err != nil {
				return fmt.Errorf("backup: create directory %q: %w", rel, err)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("backup: unsupported staged file type for %q", rel)
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("backup: stat staged file %q: %w", rel, err)
		}
		mode := info.Mode().Perm()
		if mode == 0 {
			mode = 0644
		}
		if err := copyIntoPlace(p, dst, mode); err != nil {
			return fmt.Errorf("backup: install %q: %w", rel, err)
		}
		return nil
	})
}

func copyIntoPlace(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".restore-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	return nil
}

// ApplyRetention keeps the newest snapshot in each GFS bucket (plus the newest
// snapshot overall) and deletes the rest. It returns the kept snapshots,
// newest first.
func (e *Engine) ApplyRetention(instanceID string, p Policy) ([]Snapshot, error) {
	if e == nil {
		return nil, errors.New("backup: nil engine")
	}
	instanceID, err := cleanInstanceID(instanceID)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	snaps, err := e.listLocked(instanceID)
	if err != nil {
		return nil, err
	}
	limits := [4]int{p.Hourly, p.Daily, p.Weekly, p.Monthly}
	counts := [4]map[string]int{{}, {}, {}, {}}
	kept := make([]Snapshot, 0, len(snaps))
	for i, s := range snaps {
		t := s.CreatedAt.UTC()
		year, week := t.ISOWeek()
		keys := [4]string{
			t.Format("2006-01-02T15"),
			t.Format("2006-01-02"),
			fmt.Sprintf("%04d-W%02d", year, week),
			t.Format("2006-01"),
		}
		keep := i == 0 // always keep the newest overall snapshot
		for b, key := range keys {
			counts[b][key]++
			if limits[b] > 0 && counts[b][key] <= limits[b] {
				keep = true
			}
		}
		if keep {
			kept = append(kept, s)
			continue
		}
		if delErr := e.deleteLocked(s); delErr != nil {
			err = errors.Join(err, delErr)
		}
	}
	return kept, err
}
