package config

import "testing"

func TestDefaultRolesCoverDefaultRoleCode(t *testing.T) {
	roles := DefaultRoles()
	if len(roles) != 9 {
		t.Fatalf("got %d stock roles, want 9", len(roles))
	}
	if !hasRoleCode(roles, defaultRoleCode) {
		t.Fatalf("stock roles do not define %q", defaultRoleCode)
	}
	// Defensive copy: mutating one result must not corrupt later calls.
	first := DefaultRoles()[0].(map[string]any)
	first["Code"] = "mutated"
	if hasRoleCode(DefaultRoles(), "mutated") {
		t.Fatal("DefaultRoles returned a shared mutable value")
	}
}

func TestBuildServerConfigEmitsGamePlayStyleAndRoles(t *testing.T) {
	cfg, err := BuildServerConfig(DefaultWizardForm())
	if err != nil {
		t.Fatal(err)
	}
	wc := cfg["WorldConfig"].(map[string]any)
	if got := wc["PlayStyle"]; got != "surviveandbuild" {
		t.Errorf("PlayStyle = %v, want surviveandbuild", got)
	}
	if got := wc["PlayStyleLangCode"]; got != "surviveandbuild-bands" {
		t.Errorf("PlayStyleLangCode = %v, want surviveandbuild-bands", got)
	}
	if !hasRoleCode(cfg["Roles"], defaultRoleCode) {
		t.Fatalf("generated config has no %q role", defaultRoleCode)
	}
	if _, ok := cfg["NextPlayerGroupUid"]; !ok {
		t.Error("generated config has no NextPlayerGroupUid")
	}
}

func TestEnsureRolesRepairsLegacyConfig(t *testing.T) {
	cfg := map[string]any{"DefaultRoleCode": defaultRoleCode}
	if !EnsureRoles(cfg) {
		t.Fatal("missing Roles were not repaired")
	}
	if !hasRoleCode(cfg["Roles"], defaultRoleCode) {
		t.Fatal("repaired config still lacks the default role")
	}

	cfg = map[string]any{
		"DefaultRoleCode": defaultRoleCode,
		"Roles": []any{
			map[string]any{"Code": "custom", "PrivilegeLevel": 42},
		},
	}
	if !EnsureRoles(cfg) {
		t.Fatal("default role was not appended")
	}
	roles := cfg["Roles"].([]any)
	if len(roles) != 2 || roleCode(roles[0]) != "custom" {
		t.Fatalf("custom roles were not preserved: %+v", roles)
	}
	if EnsureRoles(cfg) {
		t.Fatal("valid config was changed unnecessarily")
	}
}

func hasRoleCode(v any, code string) bool {
	roles, ok := roleSlice(v)
	if !ok {
		return false
	}
	for _, role := range roles {
		if roleCode(role) == code {
			return true
		}
	}
	return false
}
