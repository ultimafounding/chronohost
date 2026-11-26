// Package binaries downloads, caches, and inspects Vintage Story dedicated
// server distributions from the official version feeds.
package binaries

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const stableFeed = "https://api.vintagestory.at/stable.json"
const unstableFeed = "https://api.vintagestory.at/unstable.json"
const latestStable = "https://api.vintagestory.at/lateststable.txt"

// Release describes one server download listed in a version feed.
type Release struct {
	Version  string `json:"version"`
	Channel  string `json:"channel"`
	Filename string `json:"filename"`
	Size     string `json:"size"`
	MD5      string `json:"md5"`
	URL      string `json:"url"`
	Latest   bool   `json:"latest"`
}

// Client accesses version feeds and manages a local binary cache.
type Client struct {
	root string // /data/binaries
	hc   *http.Client
}

// feedEntry mirrors the per-platform objects in the version feed.
type feedEntry struct {
	Filename string `json:"filename"`
	Filesize string `json:"filesize"`
	MD5      string `json:"md5"`
	URLs     struct {
		CDN   string `json:"cdn"`
		Local string `json:"local"`
	} `json:"urls"`
	Latest int `json:"latest"`
}

// NewClient returns a client backed by root (e.g. /data/binaries).
func NewClient(root string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{root: root, hc: hc}
}

// List fetches available releases for channel ("stable", "unstable", "all"),
// sorted by version descending. Old feeds use "server" instead of
// "linuxserver" for the platform key; both are handled.
func (c *Client) List(ctx context.Context, channel string) ([]Release, error) {
	var feeds []struct{ name, url string }
	switch channel {
	case "stable":
		feeds = append(feeds, struct{ name, url string }{"stable", stableFeed})
	case "unstable":
		feeds = append(feeds, struct{ name, url string }{"unstable", unstableFeed})
	case "all":
		feeds = append(feeds,
			struct{ name, url string }{"stable", stableFeed},
			struct{ name, url string }{"unstable", unstableFeed},
		)
	default:
		return nil, fmt.Errorf("binaries: unknown channel %q (want stable, unstable, or all)", channel)
	}

	var out []Release
	seen := make(map[string]bool)
	for _, feed := range feeds {
		body, err := c.get(ctx, feed.url)
		if err != nil {
			return nil, fmt.Errorf("binaries: fetch %s feed: %w", feed.name, err)
		}
		var raw map[string]map[string]feedEntry
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("binaries: parse %s feed: %w", feed.name, err)
		}
		for version, platforms := range raw {
			key := feed.name + "/" + version
			if seen[key] {
				continue
			}
			// Prefer linuxserver; fall back to the legacy "server" key when
			// it is missing or has no usable download URL.
			entry, ok := platforms["linuxserver"]
			if !entryURL(entry) {
				if legacy, exists := platforms["server"]; exists && entryURL(legacy) {
					entry, ok = legacy, true
				}
			}
			if !ok {
				continue
			}
			downloadURL := entry.URLs.CDN
			if downloadURL == "" {
				downloadURL = entry.URLs.Local
			}
			if downloadURL == "" || downloadURL == "-" {
				continue // no usable server download for this platform
			}
			seen[key] = true
			out = append(out, Release{
				Version:  version,
				Channel:  feed.name,
				Filename: entry.Filename,
				Size:     entry.Filesize,
				MD5:      entry.MD5,
				URL:      downloadURL,
				Latest:   entry.Latest == 1,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if c := compareVersions(out[i].Version, out[j].Version); c != 0 {
			return c > 0
		}
		return out[i].Channel < out[j].Channel
	})
	return out, nil
}

// Latest returns the newest stable version string from the official feed.
func (c *Client) Latest(ctx context.Context) (string, error) {
	body, err := c.get(ctx, latestStable)
	if err != nil {
		return "", fmt.Errorf("binaries: fetch latest version: %w", err)
	}
	v := strings.TrimSpace(string(body))
	if v == "" {
		return "", errors.New("binaries: empty latest version response")
	}
	return v, nil
}

// IsCached reports whether a fully extracted version directory exists.
func (c *Client) IsCached(version string) bool {
	if !validVersion(version) {
		return false
	}
	st, err := os.Stat(filepath.Join(c.root, version))
	return err == nil && st.IsDir()
}

// Cached returns cached version names sorted descending.
func (c *Client) Cached() []string {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return nil
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() && validVersion(e.Name()) {
			versions = append(versions, e.Name())
		}
	}
	sort.Slice(versions, func(i, j int) bool {
		return compareVersions(versions[i], versions[j]) > 0
	})
	return versions
}

// Download retrieves rel into a temporary directory, verifies its MD5 sum,
// safely extracts it, and atomically renames the result into root/<version>.
// onProgress is called with downloaded and total byte counts when non-nil.
func (c *Client) Download(ctx context.Context, rel Release, onProgress func(downloaded, total int64)) (string, error) {
	if !validVersion(rel.Version) {
		return "", fmt.Errorf("binaries: invalid version %q", rel.Version)
	}
	finalDir := filepath.Join(c.root, rel.Version)
	if st, err := os.Stat(finalDir); err == nil && st.IsDir() {
		return finalDir, nil
	}

	if err := os.MkdirAll(c.root, 0o755); err != nil {
		return "", fmt.Errorf("binaries: create cache root: %w", err)
	}
	workDir, err := os.MkdirTemp(c.root, ".download-"+rel.Version+"-")
	if err != nil {
		return "", fmt.Errorf("binaries: create temp dir: %w", err)
	}
	defer os.RemoveAll(workDir) // no-op after a successful rename

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.URL, nil)
	if err != nil {
		return "", fmt.Errorf("binaries: build request for %s: %w", rel.URL, err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("binaries: download %s: %w", rel.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return "", fmt.Errorf("binaries: download %s: unexpected status %s", rel.URL, resp.Status)
	}

	archivePath := filepath.Join(workDir, "server.tar.gz")
	f, err := os.Create(archivePath)
	if err != nil {
		return "", fmt.Errorf("binaries: create archive: %w", err)
	}
	total := resp.ContentLength
	if total < 0 {
		total = 0
	}
	report := func(n int64) {
		if onProgress != nil {
			onProgress(n, total)
		}
	}
	// Wrap the body so progress is reported as bytes arrive.
	pr := &progressReader{r: resp.Body, read: report}
	n, err := io.Copy(f, pr)
	if err != nil {
		f.Close()
		return "", fmt.Errorf("binaries: write archive: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("binaries: close archive: %w", err)
	}
	report(n)

	if rel.MD5 != "" {
		sum, err := fileMD5(archivePath)
		if err != nil {
			return "", err
		}
		want := strings.ToLower(strings.TrimSpace(rel.MD5))
		if want != "" && !strings.EqualFold(sum, want) {
			return "", fmt.Errorf("binaries: md5 mismatch for %s: got %s, want %s", rel.Version, sum, want)
		}
	}

	extractDir := filepath.Join(workDir, "extracted")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return "", fmt.Errorf("binaries: create extraction dir: %w", err)
	}
	if err := extractTarGz(archivePath, extractDir); err != nil {
		return "", fmt.Errorf("binaries: extract %s: %w", rel.Version, err)
	}
	// Remove the archive so the cached directory only contains server files.
	if err := os.Remove(archivePath); err != nil {
		return "", fmt.Errorf("binaries: remove archive: %w", err)
	}

	// A concurrent download may have won the race to install this version.
	if st, err := os.Stat(finalDir); err == nil && st.IsDir() {
		return finalDir, nil
	}
	if err := os.Rename(extractDir, finalDir); err != nil {
		return "", fmt.Errorf("binaries: install %s: %w", rel.Version, err)
	}
	return finalDir, nil
}

// DLL finds the server DLL in a cached version and reports the major .NET
// runtime version declared by its *.runtimeconfig.json (e.g. "8.0" or "10.0").
func (c *Client) DLL(version string) (string, string, error) {
	if !validVersion(version) {
		return "", "", fmt.Errorf("binaries: invalid version %q", version)
	}
	dir := filepath.Join(c.root, version)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", fmt.Errorf("binaries: read version %s: %w", version, err)
	}
	var dllPath, runtimeConfig string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, "server.dll") {
			// Prefer VintagestoryServer.dll but accept any *Server.dll.
			if dllPath == "" || strings.Contains(lower, "vintagestory") {
				dllPath = filepath.Join(dir, name)
			}
			continue
		}
		if strings.HasSuffix(lower, ".runtimeconfig.json") {
			runtimeConfig = filepath.Join(dir, name)
		}
	}
	if dllPath == "" {
		return "", "", fmt.Errorf("binaries: no server DLL found in %s", dir)
	}
	if runtimeConfig == "" {
		return "", "", fmt.Errorf("binaries: no runtimeconfig.json found in %s", dir)
	}
	data, err := os.ReadFile(runtimeConfig)
	if err != nil {
		return "", "", fmt.Errorf("binaries: read runtime config: %w", err)
	}
	var cfg struct {
		RuntimeOptions struct {
			Framework struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"framework"`
		} `json:"runtimeOptions"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", "", fmt.Errorf("binaries: parse runtime config: %w", err)
	}
	v := cfg.RuntimeOptions.Framework.Version
	if v == "" {
		return "", "", fmt.Errorf("binaries: no framework version in %s", runtimeConfig)
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || parts[0] == "" {
		return "", "", fmt.Errorf("binaries: invalid framework version %q", v)
	}
	major := parts[0]
	if len(parts) > 1 {
		major += "." + parts[1]
	}
	return dllPath, major, nil
}

// Remove deletes a cached version directory.
func (c *Client) Remove(version string) error {
	if !validVersion(version) {
		return fmt.Errorf("binaries: invalid version %q", version)
	}
	if err := os.RemoveAll(filepath.Join(c.root, version)); err != nil {
		return fmt.Errorf("binaries: remove %s: %w", version, err)
	}
	return nil
}

// entryURL reports whether an entry has a download URL that is not the
// feed's "-" placeholder for unavailable downloads.
func entryURL(e feedEntry) bool {
	return (e.URLs.CDN != "" && e.URLs.CDN != "-") || (e.URLs.Local != "" && e.URLs.Local != "-")
}

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// progressReader reports the cumulative number of bytes read.
type progressReader struct {
	r    io.Reader
	read func(int64)
	n    int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.n += int64(n)
		p.read(p.n)
	}
	return n, err
}

func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("binaries: open archive for checksum: %w", err)
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("binaries: checksum archive: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractTarGz extracts only regular files and directories. Entries with
// traversal paths, absolute paths, symlinks, or other special types are
// rejected outright to prevent escaping the destination.
func extractTarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe path in archive: %q", hdr.Name)
		}
		target := filepath.Join(dest, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create dir %s: %w", name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create parent for %s: %w", name, err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return fmt.Errorf("create %s: %w", name, err)
			}
			if _, err := io.Copy(out, io.LimitReader(tr, hdr.Size)); err != nil {
				out.Close()
				return fmt.Errorf("write %s: %w", name, err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("close %s: %w", name, err)
			}
		default:
			return fmt.Errorf("unsupported archive entry %q (type %c)", hdr.Name, hdr.Typeflag)
		}
	}
}

// validVersion accepts conservative version strings: no path separators, no
// "..", no leading dots, no URI schemes. Feed versions are always numeric
// dotted strings (e.g. "1.22.7"), so this is intentionally strict.
func validVersion(v string) bool {
	if v == "" || v == "." || v == ".." || strings.ContainsAny(v, `/\`) || strings.Contains(v, "..") {
		return false
	}
	if strings.ContainsAny(v, ":*?\"<>|") {
		return false
	}
	return true
}

// compareVersions orders dotted numeric versions numerically; non-numeric
// segments fall back to lexical comparison.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		xi, xerr := strconv.Atoi(x)
		yi, yerr := strconv.Atoi(y)
		if xerr == nil && yerr == nil {
			if xi < yi {
				return -1
			}
			if xi > yi {
				return 1
			}
			continue
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}
