// Package moddb is a client for the Vintage Story ModDB API.
package moddb

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const defaultBase = "https://mods.vintagestory.at"

// Dependency is a mod dependency declared in a mod's modinfo.json.
// Legacy array entries omit "enabled", which means enabled.
type Dependency struct {
	ModID   string `json:"modid"`
	Version string `json:"version"`
	Enabled bool   `json:"enabled"`
}

func (d *Dependency) UnmarshalJSON(data []byte) error {
	var raw struct {
		ModID   string `json:"modid"`
		Version string `json:"version"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*d = Dependency{ModID: raw.ModID, Version: raw.Version, Enabled: raw.Enabled == nil || *raw.Enabled}
	return nil
}

// DependencyList accepts both dependency encodings found in the wild:
// the legacy array of objects and the current modinfo.json map of
// mod id to version string. Map entries are sorted so callers see a
// stable order.
type DependencyList []Dependency

func (d *DependencyList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*d = nil
		return nil
	}

	var list []Dependency
	if err := json.Unmarshal(trimmed, &list); err == nil {
		*d = list
		return nil
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return fmt.Errorf("dependencies must be an array or object: %w", err)
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := make(DependencyList, 0, len(keys))
	for _, key := range keys {
		dep := Dependency{ModID: key, Enabled: true}
		value := bytes.TrimSpace(object[key])
		switch {
		case len(value) == 0 || bytes.Equal(value, []byte("null")):
		case value[0] == '"':
			if err := json.Unmarshal(value, &dep.Version); err != nil {
				return fmt.Errorf("dependency %s version: %w", key, err)
			}
		case value[0] == '{':
			var details struct {
				Version string `json:"version"`
				Enabled *bool  `json:"enabled"`
			}
			if err := json.Unmarshal(value, &details); err != nil {
				return fmt.Errorf("dependency %s details: %w", key, err)
			}
			dep.Version = details.Version
			if details.Enabled != nil {
				dep.Enabled = *details.Enabled
			}
		default:
			var anyValue any
			if err := json.Unmarshal(value, &anyValue); err != nil {
				return fmt.Errorf("dependency %s value: %w", key, err)
			}
			dep.Version = fmt.Sprint(anyValue)
		}
		result = append(result, dep)
	}
	*d = result
	return nil
}

// ReadDependencies decodes modinfo.json dependencies from r. A missing
// dependencies field returns an empty list. The reader is capped because
// modinfo files are small and untrusted archives can be large.
func ReadDependencies(r io.Reader) (DependencyList, error) {
	var info struct {
		Dependencies DependencyList `json:"dependencies"`
	}
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&info); err != nil {
		return nil, err
	}
	return info.Dependencies, nil
}

// Mod is a search result from /api/mods.
type Mod struct {
	ModID        int      `json:"modid"`
	AssetID      int      `json:"assetid"`
	Name         string   `json:"name"`
	Summary      string   `json:"summary"`
	Author       string   `json:"author"`
	Side         string   `json:"side"`
	Logo         string   `json:"logo"`
	Tags         []string `json:"tags"`
	LastReleased string   `json:"lastreleased"`
	Downloads    int      `json:"downloads"`
	Follows      int      `json:"follows"`
	URLAlias     string   `json:"urlalias"`
	ModIDStrs    []string `json:"modidstrs"`
	Type         string   `json:"type"`
}

// Release is one release listed in a mod's detail response.
type Release struct {
	ReleaseID    int            `json:"releaseid"`
	MainFile     string         `json:"mainfile"`
	Filename     string         `json:"filename"`
	Tags         []string       `json:"tags"`
	ModIDStr     string         `json:"modidstr"`
	ModVersion   string         `json:"modversion"`
	Created      string         `json:"created"`
	Changelog    string         `json:"changelog"`
	Dependencies DependencyList `json:"dependencies"`
}

// ModDetail is the response from /api/mod/{idOrIdentifier}.
type ModDetail struct {
	ModID         int       `json:"modid"`
	Name          string    `json:"name"`
	Author        string    `json:"author"`
	Text          string    `json:"text"`
	Side          string    `json:"side"`
	HomepageURL   string    `json:"homepageurl"`
	SourceCodeURL string    `json:"sourcecodeurl"`
	Tags          []string  `json:"tags"`
	Releases      []Release `json:"releases"`
}

// Client accesses the ModDB over HTTP.
type Client struct {
	base string
	hc   *http.Client
}

// NewClient returns a client for base, defaulting to the public ModDB when
// base is empty. hc defaults to http.DefaultClient.
func NewClient(base string, hc *http.Client) *Client {
	if base == "" {
		base = defaultBase
	}
	base = strings.TrimRight(base, "/")
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: base, hc: hc}
}

// Search finds mods matching query and optional gameVersion. A non-positive
// limit means no client-side truncation.
func (c *Client) Search(ctx context.Context, query, gameVersion string, limit int) ([]Mod, error) {
	q := url.Values{}
	if query != "" {
		q.Set("text", query)
	}
	if gameVersion != "" {
		q.Set("gv", gameVersion)
	}
	endpoint := c.base + "/api/mods"
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	body, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("moddb: search mods: %w", err)
	}
	var resp struct {
		StatusCode string `json:"statuscode"`
		Message    string `json:"message"`
		Mods       []Mod  `json:"mods"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("moddb: parse search response: %w", err)
	}
	if resp.StatusCode != "200" {
		return nil, fmt.Errorf("moddb: search failed (status %s): %s", resp.StatusCode, resp.Message)
	}
	mods := resp.Mods
	if query != "" {
		rankMods(mods, query)
	}
	if limit > 0 && len(mods) > limit {
		mods = mods[:limit]
	}
	if mods == nil {
		mods = []Mod{}
	}
	return mods, nil
}

// rankMods puts exact name and alias matches ahead of the ModDB's default
// newest-first order. The API returns every text match, so without this a
// popular mod such as Carry On can be buried behind dozens of newer addons
// and truncated by the caller's result limit.
func rankMods(mods []Mod, query string) {
	queryCompact := compactText(query)
	if queryCompact == "" {
		return
	}
	queryTokens := textTokens(query)

	type scored struct {
		mod   Mod
		score int
	}
	ranked := make([]scored, len(mods))
	for i, m := range mods {
		ranked[i] = scored{mod: m, score: searchScore(m, queryCompact, queryTokens)}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		if ranked[i].mod.Downloads != ranked[j].mod.Downloads {
			return ranked[i].mod.Downloads > ranked[j].mod.Downloads
		}
		if ranked[i].mod.Follows != ranked[j].mod.Follows {
			return ranked[i].mod.Follows > ranked[j].mod.Follows
		}
		return false
	})
	for i := range ranked {
		mods[i] = ranked[i].mod
	}
}

func compactText(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func textTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
}

func containsTokens(haystack string, tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	for _, token := range tokens {
		if token == "" || !strings.Contains(haystack, token) {
			return false
		}
	}
	return true
}

func searchScore(m Mod, queryCompact string, queryTokens []string) int {
	score := 0
	bump := func(value int) {
		if value > score {
			score = value
		}
	}

	if name := compactText(m.Name); name != "" {
		switch {
		case name == queryCompact:
			bump(1200)
		case strings.HasPrefix(name, queryCompact):
			bump(900)
		case strings.Contains(name, queryCompact):
			bump(700)
		}
		if containsTokens(name, queryTokens) {
			bump(500)
		}
	}

	aliases := append([]string{m.URLAlias}, m.ModIDStrs...)
	for _, raw := range aliases {
		alias := compactText(raw)
		if alias == "" {
			continue
		}
		switch {
		case alias == queryCompact:
			bump(1100)
		case strings.HasPrefix(alias, queryCompact):
			bump(800)
		case strings.Contains(alias, queryCompact):
			bump(600)
		}
		if containsTokens(alias, queryTokens) {
			bump(450)
		}
	}

	if strings.Contains(compactText(m.Summary), queryCompact) {
		bump(120)
	}
	for _, tag := range m.Tags {
		if strings.Contains(compactText(tag), queryCompact) {
			bump(80)
			break
		}
	}
	if strings.Contains(compactText(m.Author), queryCompact) {
		bump(60)
	}
	return score
}

// Detail fetches a single mod by numeric ID or URL alias.
func (c *Client) Detail(ctx context.Context, id string) (*ModDetail, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("moddb: empty mod id")
	}
	endpoint := c.base + "/api/mod/" + url.PathEscape(id)
	body, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("moddb: fetch mod %s: %w", id, err)
	}
	var resp struct {
		StatusCode string    `json:"statuscode"`
		Message    string    `json:"message"`
		Mod        ModDetail `json:"mod"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("moddb: parse mod %s: %w", id, err)
	}
	if resp.StatusCode != "" && resp.StatusCode != "200" {
		return nil, fmt.Errorf("moddb: fetch mod %s failed (status %s): %s", id, resp.StatusCode, resp.Message)
	}
	if resp.Mod.ModID == 0 && resp.Mod.Name == "" {
		return nil, fmt.Errorf("moddb: mod %s not found in response", id)
	}
	return &resp.Mod, nil
}

// Compatible returns releases whose tags contain gameVersion exactly or share
// its major.minor prefix (e.g. "1.22" matches "1.22.7"). API releases are
// newest first; that order is preserved and dependencies are decoded from the
// optional API field if present.
func (c *Client) Compatible(d *ModDetail, gameVersion string) []Release {
	if d == nil {
		return nil
	}
	var out []Release
	for _, rel := range d.Releases {
		if !tagMatches(rel.Tags, gameVersion) {
			continue
		}
		out = append(out, rel)
	}
	return out
}

// Download downloads rel.MainFile to destDir, validates that it is a ZIP
// archive, reads modinfo.json inside it for dependencies, and writes the ZIP
// under a sanitized filename. It returns the path to the saved ZIP.
func (c *Client) Download(ctx context.Context, rel Release, destDir string, onProgress func(downloaded, total int64)) (string, error) {
	rawURL := strings.TrimSpace(rel.MainFile)
	if rawURL == "" {
		return "", errors.New("moddb: release has no download URL")
	}
	if u, err := url.Parse(rawURL); err != nil {
		return "", fmt.Errorf("moddb: invalid download URL %q: %w", rawURL, err)
	} else if !u.IsAbs() {
		rawURL = c.base + "/" + strings.TrimLeft(rawURL, "/")
	}

	name := filepath.Base(sanitizeFilename(rel.Filename))
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = "mod.zip"
	}
	if !strings.HasSuffix(strings.ToLower(name), ".zip") {
		name += ".zip"
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("moddb: create destination dir: %w", err)
	}
	finalPath := filepath.Join(destDir, name)
	tmp, err := os.CreateTemp(destDir, ".moddb-download-*.zip")
	if err != nil {
		return "", fmt.Errorf("moddb: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		tmp.Close()
		return "", fmt.Errorf("moddb: build download request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		tmp.Close()
		return "", fmt.Errorf("moddb: download %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		tmp.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return "", fmt.Errorf("moddb: download %s: unexpected status %s", rawURL, resp.Status)
	}

	total := resp.ContentLength
	if total < 0 {
		total = 0
	}
	pr := &progressReader{r: resp.Body, read: func(n int64) {
		if onProgress != nil {
			onProgress(n, total)
		}
	}}
	written, err := io.Copy(tmp, pr)
	if err != nil {
		tmp.Close()
		return "", fmt.Errorf("moddb: write download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("moddb: close download: %w", err)
	}
	if written == 0 {
		return "", errors.New("moddb: downloaded file is empty")
	}
	if err := validateZip(tmpPath); err != nil {
		return "", fmt.Errorf("moddb: invalid zip for %s: %w", rel.Filename, err)
	}
	if deps, err := zipDependencies(tmpPath); err != nil {
		return "", fmt.Errorf("moddb: read modinfo from %s: %w", rel.Filename, err)
	} else {
		rel.Dependencies = deps
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return "", fmt.Errorf("moddb: install %s: %w", finalPath, err)
	}
	return finalPath, nil
}

// validateZip confirms the file is a readable ZIP with at least one entry.
func validateZip(path string) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer zr.Close()
	if len(zr.File) == 0 {
		return errors.New("archive has no entries")
	}
	return nil
}

// zipDependencies reads modinfo.json (at any depth) from the ZIP and returns
// its dependencies. A missing modinfo.json is not an error.
func zipDependencies(path string) (DependencyList, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.ToLower(filepath.Base(f.Name)) != "modinfo.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", f.Name, err)
		}
		deps, err := ReadDependencies(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", f.Name, err)
		}
		return deps, nil
	}
	return nil, nil
}

func (c *Client) get(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
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

// tagMatches reports whether a release tag exactly equals gameVersion or
// starts with its major.minor prefix (e.g. gameVersion 1.22.7 matches tag
// "1.22.0" or "1.22.7", but not "1.21.5").
func tagMatches(tags []string, gameVersion string) bool {
	gv := strings.TrimSpace(gameVersion)
	if gv == "" {
		return true // no version filter requested
	}
	parts := strings.Split(gv, ".")
	prefix := gv
	if len(parts) >= 2 {
		prefix = parts[0] + "." + parts[1]
	}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == gv || strings.HasPrefix(tag, prefix+".") || tag == prefix {
			return true
		}
	}
	return false
}

// sanitizeFilename strips path components and control characters from a
// filename supplied by a remote API before writing it into destDir.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/:*?"<>|`, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

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
