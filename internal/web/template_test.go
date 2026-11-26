package web

import (
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
