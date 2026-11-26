package api

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackm/chronohost/internal/moddb"
	"github.com/jackm/chronohost/internal/store"
)

func zipModinfo(t *testing.T, info string) []byte {
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
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestModDependenciesAcceptsObjectFormAndSkipsGame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mod.zip")
	if err := os.WriteFile(path, zipModinfo(t, `{"modid":"nbcartographer","dependencies":{"game":"1.22.0-pre.3","survival":"1.22.0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := modDependencies(path)
	if len(deps) != 1 || deps[0].ModID != "survival" || deps[0].Version != "1.22.0" || !deps[0].Enabled {
		t.Fatalf("deps = %+v", deps)
	}
}

func TestModDependenciesArrayEntriesAreEnabledByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mod.zip")
	if err := os.WriteFile(path, zipModinfo(t, `{"modid":"top","dependencies":[{"modid":"carryonlib","version":"1.0.0"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := modDependencies(path)
	if len(deps) != 1 || deps[0].ModID != "carryonlib" || deps[0].Version != "1.0.0" || !deps[0].Enabled {
		t.Fatalf("deps = %+v", deps)
	}
}

func TestResolveDependencyIDSearchFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/mod/odd-id":
			fmt.Fprint(w, `{"statuscode":"404","message":"not found"}`)
		case "/api/mods":
			fmt.Fprint(w, `{"statuscode":"200","mods":[{"modid":555,"name":"Odd Mod","urlalias":"oddmod","modidstrs":["odd-id"]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	app := &App{ModDB: moddb.NewClient(srv.URL, nil)}
	id, err := app.resolveDependencyID(context.Background(), "odd-id", "1.22.7")
	if err != nil {
		t.Fatalf("resolveDependencyID: %v", err)
	}
	if id != 555 {
		t.Fatalf("id = %d, want 555", id)
	}
}

func TestBuildModViewsDependencyStatus(t *testing.T) {
	mods := []store.Mod{
		{ID: 1, ModID: 100, Name: "Top", Version: "1.0.0", Dependencies: []store.ModDependency{
			{ModID: "dep", Version: "2.0.0", ResolvedID: 200},
		}},
		{ID: 2, ModID: 200, Name: "Dep", Version: "2.0.0"},
		{ID: 3, ModID: 300, Name: "Orphan", Dependencies: []store.ModDependency{{ModID: "missing"}}},
	}
	views := buildModViews(mods, "1.22.7")
	if len(views) != 3 {
		t.Fatalf("views = %+v", views)
	}
	if len(views[0].DependencyStatus) != 1 || !views[0].DependencyStatus[0].Installed {
		t.Errorf("top dependency status = %+v", views[0].DependencyStatus)
	}
	if len(views[1].Dependents) != 1 || views[1].Dependents[0] != "Top" {
		t.Errorf("dep dependents = %+v", views[1].Dependents)
	}
	if len(views[2].DependencyStatus) != 1 || views[2].DependencyStatus[0].Installed {
		t.Errorf("orphan dependency status = %+v", views[2].DependencyStatus)
	}
}

func TestInstallModResolvesDependencies(t *testing.T) {
	root := t.TempDir()
	app, err := New(root, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	topZip := zipModinfo(t, `{"modid":"top","dependencies":{"dep":"2.0.0"}}`)
	depZip := zipModinfo(t, `{"modid":"dep"}`)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/mod/100":
			fmt.Fprintf(w, `{"statuscode":"200","mod":{"modid":100,"name":"Top","releases":[{"releaseid":1,"mainfile":%q,"filename":"top.zip","modversion":"1.0.0","tags":["1.22.7"]}]}}`, srv.URL+"/files/top.zip")
		case "/api/mod/dep", "/api/mod/200":
			fmt.Fprintf(w, `{"statuscode":"200","mod":{"modid":200,"name":"Dep","releases":[{"releaseid":2,"mainfile":%q,"filename":"dep.zip","modversion":"2.0.0","tags":["1.22.7"]}]}}`, srv.URL+"/files/dep.zip")
		case "/files/top.zip":
			_, _ = w.Write(topZip)
		case "/files/dep.zip":
			_, _ = w.Write(depZip)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	app.ModDB = moddb.NewClient(srv.URL, nil)

	ctx := context.Background()
	in := store.Instance{ID: "dep-test", Name: "Dependency Test", Version: "1.22.7", DataPath: filepath.Join(root, "instances", "dep-test")}
	if err := os.MkdirAll(in.DataPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := app.Store.CreateInstance(ctx, in); err != nil {
		t.Fatal(err)
	}

	out, err := app.installMod(ctx, in, 100, 0)
	if err != nil {
		t.Fatalf("installMod: %v", err)
	}
	if out.Mod.ModID != 100 || len(out.Installed) != 2 {
		t.Fatalf("outcome = %+v", out)
	}
	if len(out.Warnings) != 0 {
		t.Errorf("warnings = %+v", out.Warnings)
	}
	mods, err := app.Store.ListMods(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mods) != 2 {
		t.Fatalf("mods = %+v", mods)
	}
	var top store.Mod
	for _, m := range mods {
		if m.ModID == 100 {
			top = m
		}
	}
	if len(top.Dependencies) != 1 || top.Dependencies[0].ModID != "dep" || top.Dependencies[0].ResolvedID != 200 {
		t.Fatalf("top dependencies = %+v", top.Dependencies)
	}
	views := buildModViews(mods, in.Version)
	for _, view := range views {
		if view.ModID == 100 && (len(view.DependencyStatus) != 1 || !view.DependencyStatus[0].Installed) {
			t.Errorf("top view = %+v", view)
		}
	}
}

func TestModsPageRendersDependencyBadges(t *testing.T) {
	t.Setenv("CHRONOHOST_ADMIN_USER", "root")
	t.Setenv("CHRONOHOST_ADMIN_PASSWORD", "a-very-secret-password")
	root := t.TempDir()
	app, err := New(root, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	ctx := context.Background()
	in := store.Instance{ID: "page-test", Name: "Page Test", Version: "1.22.7", DataPath: filepath.Join(root, "instances", "page-test")}
	if err := app.Store.CreateInstance(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Store.AddMod(ctx, store.Mod{
		InstanceID: in.ID, ModID: 100, Name: "Top", Version: "1.0.0", Filename: "top.zip",
		GameVersions: []string{"1.22.7"}, Dependencies: []store.ModDependency{{ModID: "dep", ResolvedID: 200}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Store.AddMod(ctx, store.Mod{
		InstanceID: in.ID, ModID: 200, Name: "Dep", Version: "2.0.0", Filename: "dep.zip",
		GameVersions: []string{"1.22.7"},
	}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(app.Handler())
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login, err := client.PostForm(server.URL+"/login", url.Values{
		"username": {"root"}, "password": {"a-very-secret-password"}, "next": {"/mods?instance=page-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	res, err := client.Get(server.URL + "/mods?instance=page-test")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("mods page = %d: %s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "dep · installed") {
		t.Fatalf("dependency badge missing from page: %s", body)
	}
	if !strings.Contains(string(body), "Required by Top") {
		t.Fatalf("dependent note missing from page: %s", body)
	}
}

func TestVersionsMatch(t *testing.T) {
	cases := []struct {
		required  string
		installed string
		want      bool
	}{
		{"", "1.0.0", true},
		{"1.0.0", "1.0.0", true},
		{"1.0.0", "1.0.1", false},
		{"1.0.0", "", true},
	}
	for _, tc := range cases {
		if got := versionsMatch(tc.required, tc.installed); got != tc.want {
			t.Errorf("versionsMatch(%q, %q) = %v, want %v", tc.required, tc.installed, got, tc.want)
		}
	}
}
