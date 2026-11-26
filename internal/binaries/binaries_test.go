package binaries

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	// Point the package feed constants at the test server via a custom
	// transport that rewrites all api.vintagestory.at requests locally.
	base := srv.URL
	transport := &rewriteTransport{base: base, inner: http.DefaultTransport}
	return NewClient(t.TempDir(), &http.Client{Transport: transport}), srv
}

type rewriteTransport struct {
	base  string
	inner http.RoundTripper
}

func (r *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.String(), "https://api.vintagestory.at/") {
		u := *req.URL
		u.Scheme = "http"
		u.Host = strings.TrimPrefix(r.base, "http://")
		clone := req.Clone(req.Context())
		clone.URL = &u
		return r.inner.RoundTrip(clone)
	}
	return r.inner.RoundTrip(req)
}

func TestListParsesFeedsAndSorts(t *testing.T) {
	stable := `{
	  "1.21.0": {"linuxserver": {"filename":"vs1.tar.gz","filesize":"10","md5":"abc","urls":{"cdn":"https://cdn/1","local":"https://local/1"}}},
	  "1.22.10": {"linuxserver": {"filename":"vs2.tar.gz","filesize":"20","md5":"def","urls":{"cdn":"https://cdn/2","local":"-"},"latest":1}},
	  "1.22.7": {"server": {"filename":"old.tar.gz","filesize":"30","md5":"ghi","urls":{"cdn":"","local":"https://local/old"}}},
	  "1.20.0": {"linuxserver": {"filename":"bad.tar.gz","filesize":"1","md5":"x","urls":{"cdn":"","local":""}}}
	}`
	unstable := `{
	  "1.23.0-rc.1": {"linuxserver": {"filename":"rc.tar.gz","filesize":"5","md5":"jkl","urls":{"cdn":"https://cdn/rc"}}}
	}`
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stable.json":
			fmt.Fprint(w, stable)
		case "/unstable.json":
			fmt.Fprint(w, unstable)
		default:
			http.NotFound(w, r)
		}
	}))
	releases, err := c.List(context.Background(), "all")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"1.23.0-rc.1", "1.22.10", "1.22.7", "1.21.0"}
	got := make([]string, 0, len(releases))
	for _, rel := range releases {
		got = append(got, rel.Version)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("versions = %v, want %v", got, want)
	}
	// Verify fallback to local URL and legacy platform key.
	for _, rel := range releases {
		if rel.Version == "1.22.7" {
			if rel.URL != "https://local/old" || rel.Channel != "stable" {
				t.Errorf("legacy entry: %+v", rel)
			}
		}
		if rel.Version == "1.22.10" && (!rel.Latest || rel.Size != "20") {
			t.Errorf("latest entry: %+v", rel)
		}
	}
	if _, err := c.List(context.Background(), "bogus"); err == nil {
		t.Error("expected error for unknown channel")
	}
}

func TestLatest(t *testing.T) {
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/lateststable.txt" {
			fmt.Fprint(w, "1.22.7\n")
			return
		}
		http.NotFound(w, r)
	}))
	v, err := c.Latest(context.Background())
	if err != nil || v != "1.22.7" {
		t.Fatalf("Latest = %q, %v", v, err)
	}
}

func makeTarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func serveArchive(t *testing.T, data []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	}))
}

func TestDownloadVerifyMD5ExtractAndCached(t *testing.T) {
	archive := makeTarGz(t, map[string]string{
		"VintagestoryServer.dll": "binary",
		"vs.runtimeconfig.json":  `{"runtimeOptions":{"framework":{"name":"Microsoft.NETCore.App","version":"10.0.0"}}}`,
	})
	sum := md5.Sum(archive)
	srv := serveArchive(t, archive)
	defer srv.Close()

	c := NewClient(t.TempDir(), nil)
	rel := Release{Version: "1.22.7", URL: srv.URL + "/vs.tar.gz", MD5: hex.EncodeToString(sum[:])}
	var progressCalls int
	_, err := c.Download(context.Background(), rel, func(n, total int64) { progressCalls++ })
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if progressCalls == 0 {
		t.Error("progress callback never invoked")
	}
	if !c.IsCached("1.22.7") {
		t.Error("version not cached")
	}
	if got := c.Cached(); len(got) != 1 || got[0] != "1.22.7" {
		t.Fatalf("Cached = %v", got)
	}
	dll, runtime, err := c.DLL("1.22.7")
	if err != nil {
		t.Fatalf("DLL: %v", err)
	}
	if filepath.Base(dll) != "VintagestoryServer.dll" || runtime != "10.0" {
		t.Fatalf("DLL = %s, %s", dll, runtime)
	}
	// Cached download returns immediately without another HTTP request.
	srv.Close()
	if _, err := c.Download(context.Background(), rel, nil); err != nil {
		t.Fatalf("cached Download: %v", err)
	}
	if err := c.Remove("1.22.7"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if c.IsCached("1.22.7") {
		t.Error("still cached after Remove")
	}
}

func TestDownloadRejectsBadMD5AndUnsafePaths(t *testing.T) {
	good := makeTarGz(t, map[string]string{"VintagestoryServer.dll": "x"})
	srv := serveArchive(t, good)
	defer srv.Close()
	c := NewClient(t.TempDir(), nil)
	rel := Release{Version: "1.22.7", URL: srv.URL + "/a.tar.gz", MD5: strings.Repeat("0", 32)}
	if _, err := c.Download(context.Background(), rel, nil); err == nil || !strings.Contains(err.Error(), "md5 mismatch") {
		t.Fatalf("expected md5 mismatch, got %v", err)
	}
	entries, err := os.ReadDir(c.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".download-") {
			t.Errorf("temp dir left behind: %s", e.Name())
		}
	}

	evil := makeTarGz(t, map[string]string{"../escape.txt": "bad"})
	srv2 := serveArchive(t, evil)
	defer srv2.Close()
	rel2 := Release{Version: "1.22.8", URL: srv2.URL + "/a.tar.gz"}
	if _, err := c.Download(context.Background(), rel2, nil); err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("expected unsafe path error, got %v", err)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.22.10", "1.22.7", 1},
		{"1.21.0", "1.22.0", -1},
		{"1.22.7", "1.22.7", 0},
		{"2.0.0", "1.99.99", 1},
	}
	for _, tc := range cases {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
