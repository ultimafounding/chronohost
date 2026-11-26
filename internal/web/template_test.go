package web

import (
	"io/fs"
	"strings"
	"testing"
)

func TestInstanceConsoleUsesCommandScope(t *testing.T) {
	raw, err := templatesFS.ReadFile("templates/instance.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)

	// The console markup must live in the same Alpine component that defines
	// sendCommand(); otherwise clicking Send silently does nothing.
	if !strings.Contains(page, `<div x-data="controls()">`) {
		t.Fatal("instance page no longer mounts controls() for the console")
	}
	if strings.Contains(page, `x-data="{ tab: 'console' }"`) {
		t.Fatal("console tabs still use a scope without sendCommand()")
	}
	if !strings.Contains(page, "sendCommand() {") {
		t.Fatal("controls() no longer defines sendCommand()")
	}
	if !strings.Contains(page, `x-on:keydown.enter.prevent="sendCommand()"`) {
		t.Fatal("console Enter key is not wired to sendCommand()")
	}
	if !strings.Contains(page, `@click="sendCommand()"`) {
		t.Fatal("console Send button is not wired to sendCommand()")
	}
}

func TestVendoredFontsAreEmbedded(t *testing.T) {
	static, err := Static()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"fonts/plex-sans-400.woff2",
		"fonts/plex-sans-600.woff2",
		"fonts/plex-mono-400.woff2",
	} {
		file, err := static.Open(name)
		if err != nil {
			t.Fatalf("font %s is not embedded: %v", name, err)
		}
		_ = file.Close()
	}

	raw, err := fs.ReadFile(static, "style.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)
	if !strings.Contains(css, `font-family: "IBM Plex Sans"`) || !strings.Contains(css, `font-family: "IBM Plex Mono"`) {
		t.Fatal("style.css no longer declares the vendored font families")
	}
	if strings.Contains(css, "Georgia") {
		t.Fatal("style.css still uses the old serif display font")
	}
}
