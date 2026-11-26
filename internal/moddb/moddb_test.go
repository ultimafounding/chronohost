package moddb

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

func TestSearchRanksExactMatchesFirst(t *testing.T) {
	cases := []struct {
		query string
		name  string
		alias string
		id    int
	}{
		{query: "better ruins", name: "BetterRuins", alias: "betterruins", id: 792},
		{query: "carry on", name: "Carry On", alias: "carryon", id: 890},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/mods" {
					http.NotFound(w, r)
					return
				}
				fmt.Fprint(w, `{"statuscode":"200","mods":[`)
				for i := 0; i < 30; i++ {
					if i > 0 {
						fmt.Fprint(w, ",")
					}
					fmt.Fprintf(w, `{"modid":%d,"name":%q,"urlalias":%q,"modidstrs":[%q],"downloads":%d}`,
						1000+i, tc.query+" Addon "+strconv.Itoa(i), "addon"+strconv.Itoa(i), "addon"+strconv.Itoa(i), i)
				}
				fmt.Fprintf(w, `,{"modid":%d,"name":%q,"urlalias":%q,"modidstrs":[%q],"downloads":1251007}]}`,
					tc.id, tc.name, tc.alias, tc.alias)
			}))
			defer srv.Close()

			c := NewClient(srv.URL, nil)
			mods, err := c.Search(context.Background(), tc.query, "", 5)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(mods) != 5 {
				t.Fatalf("len(mods) = %d", len(mods))
			}
			if mods[0].ModID != tc.id || mods[0].Name != tc.name {
				t.Fatalf("first result = %+v, want %q", mods[0], tc.name)
			}
		})
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

func TestDependencyListAcceptsObjectForm(t *testing.T) {
	raw := `{"game":"1.22.0-pre.3","another-mod":"1.2.0"}`
	var deps DependencyList
	if err := json.Unmarshal([]byte(raw), &deps); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("deps = %+v", deps)
	}
	if deps[0].ModID != "another-mod" || deps[0].Version != "1.2.0" || !deps[0].Enabled {
		t.Errorf("deps[0] = %+v", deps[0])
	}
	if deps[1].ModID != "game" || deps[1].Version != "1.22.0-pre.3" || !deps[1].Enabled {
		t.Errorf("deps[1] = %+v", deps[1])
	}
}

func TestZipDependenciesAcceptsObjectForm(t *testing.T) {
	data := zipWithModinfo(t, `{"modid":"nbcartographer","version":"2.3.0","dependencies":{"game":"1.22.0-pre.3"}}`)
	path := filepath.Join(t.TempDir(), "nbcartographer_2.3.0.zip")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	deps, err := zipDependencies(path)
	if err != nil {
		t.Fatalf("zipDependencies: %v", err)
	}
	if len(deps) != 1 || deps[0].ModID != "game" || deps[0].Version != "1.22.0-pre.3" || !deps[0].Enabled {
		t.Fatalf("deps = %+v", deps)
	}
}
