package api

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestModDependenciesAcceptsObjectFormAndSkipsGame(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("modinfo.json")
	if err != nil {
		t.Fatal(err)
	}
	info := `{"modid":"nbcartographer","dependencies":{"game":"1.22.0-pre.3","survival":"1.22.0"}}`
	if _, err := f.Write([]byte(info)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mod.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	deps := modDependencies(path)
	if len(deps) != 1 || deps[0] != "survival" {
		t.Fatalf("deps = %+v", deps)
	}
}
