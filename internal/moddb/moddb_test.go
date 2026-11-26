package moddb

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchAndDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/mods":
			if got := r.URL.Query().Get("text"); got != "carry" {
				t.Errorf("text = %q", got)
			}
			if got := r.URL.Query().Get("gv"); got != "1.22.7" {
				t.Errorf("gv = %q", got)
			}
			fmt.Fprint(w, `{"statuscode":"200","mods":[
			  {"modid":890,"assetid":4405,"name":"Carry On","author":"x","side":"both","tags":["1.22.x"],"downloads":12},
			  {"modid":891,"name":"Other"}
			]}`)
		case "/api/mod/carryon":
			fmt.Fprint(w, `{"mod":{"modid":890,"name":"Carry On","author":"x","text":"<p>hi</p>","side":"both","releases":[
			  {"releaseid":1,"mainfile":"http://example/a.zip","filename":"A.zip","tags":["1.22.7"]},
			  {"releaseid":2,"mainfile":"http://example/b.zip","filename":"B.zip","tags":["1.21.5"]},
			  {"releaseid":3,"mainfile":"http://example/c.zip","filename":"C.zip","tags":["1.22.0"]}
			]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil)
	mods, err := c.Search(context.Background(), "carry", "1.22.7", 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(mods) != 1 || mods[0].ModID != 890 || mods[0].Name != "Carry On" {
		t.Fatalf("mods = %+v", mods)
	}
	d, err := c.Detail(context.Background(), "carryon")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if d.ModID != 890 || len(d.Releases) != 3 {
		t.Fatalf("detail = %+v", d)
	}
	compatible := c.Compatible(d, "1.22.7")
	if len(compatible) != 2 || compatible[0].ReleaseID != 1 || compatible[1].ReleaseID != 3 {
		t.Fatalf("compatible = %+v", compatible)
	}
	if got := c.Compatible(d, "1.20.0"); len(got) != 0 {
		t.Fatalf("compatible 1.20 = %+v", got)
	}
}

func TestSearchErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"statuscode":"500","message":"boom"}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, nil)
	if _, err := c.Search(context.Background(), "x", "", 0); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected status error, got %v", err)
	}
}

func zipWithModinfo(t *testing.T, info string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("modinfo.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(info)); err != nil {
		t.Fatal(err)
	}
	other, _ := zw.Create("assets/test.png")
	other.Write([]byte("PNG"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDownloadZipValidatesAndSanitizes(t *testing.T) {
	data := zipWithModinfo(t, `{"modid":"carryon","version":"1.0.0","dependencies":[{"modid":"other","version":"1.2.0","enabled":true}]}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	}))
	defer srv.Close()
	c := NewClient("", nil)
	dest := t.TempDir()
	rel := Release{MainFile: srv.URL + "/mod.zip?dl=mod.zip", Filename: "../../evil/name.zip"}
	var calls int
	path, err := c.Download(context.Background(), rel, dest, func(n, total int64) { calls++ })
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if filepath.Base(path) != "name.zip" || filepath.Dir(path) != dest {
		t.Fatalf("path = %q", path)
	}
	if calls == 0 {
		t.Error("progress callback not called")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Dependencies are validated by zipDependencies via a direct call here.
	deps, err := zipDependencies(path)
	if err != nil || len(deps) != 1 || deps[0].ModID != "other" || !deps[0].Enabled {
		t.Fatalf("deps = %+v, err = %v", deps, err)
	}
}

func TestDownloadRejectsInvalidZip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not a zip")
	}))
	defer srv.Close()
	c := NewClient("", nil)
	if _, err := c.Download(context.Background(), Release{MainFile: srv.URL + "/x.zip"}, t.TempDir(), nil); err == nil {
		t.Fatal("expected invalid zip error")
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"../evil.zip":   "evil.zip",
		`C:\mods\x.zip`: "x.zip",
		"a/b/c.zip":     "c.zip",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
