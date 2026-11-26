package config

import (
	"strings"
	"testing"
)

func TestDefaultsValidate(t *testing.T) {
	f := DefaultWizardForm()
	if errs := Validate(f); len(errs) != 0 {
		t.Fatalf("default form has validation errors: %+v", errs)
	}
}

func TestBuildServerConfigCoversGameSchema(t *testing.T) {
	f := DefaultWizardForm()
	f.ServerName = "Temporal Test"
	f.Seed = "12345"
	f.Port = 42420
	cfg, err := BuildServerConfig(f)
	if err != nil {
		t.Fatal(err)
	}

	rootKeys := []string{
		"ServerName", "ServerDescription", "WelcomeMessage", "Password", "MaxClients",
		"Port", "AdvertiseServer", "WhitelistMode", "ModPaths", "MapSizeX", "MapSizeY",
		"MapSizeZ", "WorldConfig",
	}
	for _, key := range rootKeys {
		if _, ok := cfg[key]; !ok {
			t.Errorf("missing root key %q", key)
		}
	}
	wc, ok := cfg["WorldConfig"].(map[string]any)
	if !ok {
		t.Fatal("WorldConfig is not an object")
	}
	wcKeys := []string{
		"Seed", "SaveFileLocation", "WorldName", "AllowCreativeMode", "PlayStyle",
		"PlayStyleLangCode", "WorldType", "WorldConfiguration", "MapSizeY",
		"CreatedByPlayerName", "DisabledMods", "RepairMode",
	}
	for _, key := range wcKeys {
		if _, ok := wc[key]; !ok {
			t.Errorf("missing WorldConfig key %q", key)
		}
	}
	if got := wc["Seed"]; got != "12345" {
		t.Errorf("seed = %v, want 12345", got)
	}

	world, ok := wc["WorldConfiguration"].(map[string]any)
	if !ok {
		t.Fatal("WorldConfiguration is not an object")
	}
	wantKeys := []string{
		"gameMode", "startingClimate", "spawnRadius", "graceTimer", "deathPunishment",
		"droppedItemsTimer", "seasons", "playerlives", "lungCapacity", "daysPerMonth",
		"harshWinters", "blockGravity", "caveIns", "allowUndergroundFarming",
		"noLiquidSourceTransport", "bodyTemperatureResistance", "creatureHostility",
		"creatureStrength", "creatureSwimSpeed", "playerHealthPoints", "playerHungerSpeed",
		"playerHealthRegenSpeed", "playerMoveSpeed", "foodSpoilSpeed", "saplingGrowthRate",
		"toolDurability", "toolMiningSpeed", "propickNodeSearchRadius", "microblockChiseling",
		"allowCoordinateHud", "allowMap", "colorAccurateWorldmap", "loreContent",
		"clutterObtainable", "lightningFires", "allowTimeswitch", "temporalStability",
		"temporalStorms", "tempstormDurationMul", "temporalRifts", "temporalGearRespawnUses",
		"temporalStormSleeping", "worldClimate", "landcover", "oceanscale",
		"upheavelCommonness", "geologicActivity", "landformScale", "worldWidth",
		"worldLength", "worldEdge", "polarEquatorDistance", "storyStructuresDistScaling",
		"globalTemperature", "globalPrecipitation", "globalForestation",
		"globalDepositSpawnRate", "surfaceCopperDeposits", "surfaceTinDeposits", "snowAccum",
		"allowLandClaiming", "classExclusiveRecipes", "auctionHouse", "cropGrowthRateMul",
	}
	for _, key := range wantKeys {
		value, ok := world[key]
		if !ok {
			t.Errorf("missing WorldConfiguration key %q", key)
			continue
		}
		// Numeric settings must be quoted strings for VS world generation.
		if _, isString := value.(string); !isString {
			boolKeys := map[string]bool{
				"loreContent": true, "temporalStability": true, "colorAccurateWorldmap": true,
				"lightningFires": true, "classExclusiveRecipes": true, "auctionHouse": true,
			}
			if strings.Contains(key, "allow") || boolKeys[key] {
				continue // booleans are emitted as JSON booleans, not numeric strings
			}
			if key == "gameMode" || key == "startingClimate" || key == "deathPunishment" ||
				key == "blockGravity" || key == "caveIns" || key == "creatureHostility" ||
				key == "microblockChiseling" || key == "temporalStorms" || key == "temporalRifts" ||
				key == "worldClimate" || key == "worldEdge" || key == "clutterObtainable" {
				continue
			}
			t.Errorf("WorldConfiguration[%q] = %T %v, want string", key, value, value)
		}
	}
	if got := world["droppedItemsTimer"]; got != "600" {
		t.Errorf("droppedItemsTimer = %v, want 600 (10 minutes as seconds)", got)
	}
	if got := world["worldWidth"]; got != "1024000" {
		t.Errorf("worldWidth = %v, want 1024000", got)
	}
}

func TestApplyPresetPreservesIdentity(t *testing.T) {
	f := DefaultWizardForm()
	f.ServerName = "My World"
	f.Password = "secret"
	f.Port = 42425
	if err := ApplyPreset(&f, "creativebuilding"); err != nil {
		t.Fatal(err)
	}
	if f.ServerName != "My World" || f.Password != "secret" || f.Port != 42425 {
		t.Fatalf("preset overwrote identity fields: %+v", f)
	}
	if f.GameMode != "creative" || f.TemporalStorms != "off" {
		t.Fatalf("creative preset not applied: %+v", f)
	}
	if err := ApplyPreset(&f, "wildernesssurvival"); err != nil {
		t.Fatal(err)
	}
	if f.GameMode != "survival" || f.HungerSpeed < 1 {
		t.Fatalf("wilderness preset not applied: %+v", f)
	}
	if err := ApplyPreset(&f, "does-not-exist"); err == nil {
		t.Fatal("expected error for unknown preset")
	}
}

func TestValidateReportsFieldErrors(t *testing.T) {
	f := DefaultWizardForm()
	f.ServerName = ""
	f.MaxClients = 0
	f.Port = 80
	f.TemporalStorms = "sometimes!"
	errs := Validate(f)
	got := map[string]bool{}
	for _, e := range errs {
		got[e.Field] = true
	}
	for _, field := range []string{"serverName", "maxClients", "port", "temporalStorms"} {
		if !got[field] {
			t.Errorf("expected validation error for %q, got %+v", field, errs)
		}
	}
}

func TestMergeConfigPreservesGameManagedKeys(t *testing.T) {
	base := map[string]any{
		"Roles": map[string]any{"admin": true},
		"WorldConfig": map[string]any{
			"NextPlayerGroupUid": 7,
			"Seed":               "old",
		},
	}
	next := map[string]any{
		"WorldConfig": map[string]any{"Seed": "new"},
		"Port":        42420,
	}
	merged := MergeConfig(base, next)
	if _, ok := merged["Roles"]; !ok {
		t.Error("lost Roles key")
	}
	wc := merged["WorldConfig"].(map[string]any)
	if wc["NextPlayerGroupUid"] != 7 || wc["Seed"] != "new" {
		t.Errorf("merge produced %+v", wc)
	}
}

func TestPresetsMatchInGameList(t *testing.T) {
	want := []string{
		"standard", "wildernesssurvival", "exploration", "homosapiens", "creativebuilding",
	}
	presets := Presets()
	if len(presets) != len(want) {
		t.Fatalf("got %d presets, want %d", len(presets), len(want))
	}
	for i, p := range presets {
		if p.ID != want[i] {
			t.Errorf("preset %d = %q, want %q", i, p.ID, want[i])
		}
		if err := ApplyPreset(&WizardForm{}, p.ID); err != nil {
			t.Errorf("preset %q cannot be applied: %v", p.ID, err)
		}
	}
}
