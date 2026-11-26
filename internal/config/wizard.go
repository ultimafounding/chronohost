// Package config models the Vintage Story serverconfig.json schema and
// translates the ChronoHost creation wizard into a valid config document.
//
// WorldConfiguration values are deliberately emitted as strings. Vintage Story
// parses these attributes from string literals; unquoted numbers in a freshly
// generated serverconfig.json are silently ignored by the world generator.
package config

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// WizardForm is the complete set of values exposed by the "Make a Server"
// wizard. JSON tags match the form field names used by the web UI.
type WizardForm struct {
	// Step 1 — identity and core rules.
	ServerName        string `json:"serverName"`
	ServerDescription string `json:"serverDescription"`
	WelcomeMessage    string `json:"welcomeMessage"`
	Password          string `json:"password"`
	MaxClients        int    `json:"maxClients"`
	Port              int    `json:"port"`
	Advertise         bool   `json:"advertise"`
	Preset            string `json:"preset"`
	GameMode          string `json:"gameMode"`

	// Step 2 — world generation and geography.
	Seed                 string  `json:"seed"`
	MapSizeX             int     `json:"mapSizeX"`
	MapSizeZ             int     `json:"mapSizeZ"`
	WorldHeight          int     `json:"worldHeight"`
	StartingClimate      string  `json:"startingClimate"`
	Landcover            float64 `json:"landcover"`
	LandcoverScale       float64 `json:"landcoverScale"`
	UpheavalRate         float64 `json:"upheavalRate"`
	LandformScale        float64 `json:"landformScale"`
	GeologicActivity     string  `json:"geologicActivity"`
	GlobalTemperature    float64 `json:"globalTemperature"`
	GlobalPrecipitation  float64 `json:"globalPrecipitation"`
	PolarEquatorDistance int     `json:"polarEquatorDistance"`
	Forestation          float64 `json:"forestation"`
	CopperDeposits       string  `json:"copperDeposits"`
	TinDeposits          string  `json:"tinDeposits"`
	SaplingGrowthRate    float64 `json:"saplingGrowthRate"`
	CropGrowthRate       float64 `json:"cropGrowthRate"`

	// Step 3 — survival, combat and temporal mechanics.
	TemporalStability        bool    `json:"temporalStability"`
	TemporalStorms           string  `json:"temporalStorms"`
	TemporalRifts            string  `json:"temporalRifts"`
	TemporalGearRespawnUses  int     `json:"temporalGearRespawnUses"`
	PlayerHealthPoints       int     `json:"playerHealthPoints"`
	HungerSpeed              float64 `json:"hungerSpeed"`
	BodyTemperatureHardiness int     `json:"bodyTemperatureHardiness"`
	DeathPunishment          string  `json:"deathPunishment"`
	DroppedItemsTimer        int     `json:"droppedItemsTimer"` // minutes in the UI, seconds in the game
	MicroblockChiseling      string  `json:"microblockChiseling"`
	AllowUndergroundFarming  bool    `json:"allowUndergroundFarming"`
	AllowCoordinateHUD       bool    `json:"allowCoordinateHUD"`
	AllowMap                 bool    `json:"allowMap"`
	CreatureHostility        string  `json:"creatureHostility"`
	CreatureStrength         float64 `json:"creatureStrength"`
	CreatureHealth           float64 `json:"creatureHealth"`
	GraceTimer               int     `json:"graceTimer"`
	HarshWinters             bool    `json:"harshWinters"`
	FoodSpoilageRate         float64 `json:"foodSpoilageRate"`
	FireSpread               bool    `json:"fireSpread"`
	LightningFires           bool    `json:"lightningFires"`
	CaveIns                  string  `json:"caveIns"`
}

// FieldError describes a validation failure for a single wizard field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Preset is a playstyle template matching the in-game "Customize World" presets.
type Preset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Presets returns the playstyle templates in menu order.
func Presets() []Preset {
	return []Preset{
		{"standard", "Standard", "A balanced Vintage Story world: survive, build and explore."},
		{"wildernesssurvival", "Wilderness Survival", "Harsher survival. Hostile wildlife, cave-ins and a tougher climate."},
		{"exploration", "Exploration", "More oceans and varied landforms. Made for long journeys."},
		{"homosapiens", "Homo Sapiens", "Primitive start: hostile world, limited map and scarce surface metals."},
		{"creativebuilding", "Creative Building", "Creative mode, no hostiles, no temporal storms."},
	}
}

// DefaultWizardForm returns the game's standard playstyle defaults.
func DefaultWizardForm() WizardForm {
	return WizardForm{
		ServerName:               "Vintage Story Server",
		ServerDescription:        "A world kept stable by ChronoHost.",
		WelcomeMessage:           "Welcome {0}, may you survive well and prosper",
		MaxClients:               16,
		Port:                     42420,
		Advertise:                false,
		Preset:                   "standard",
		GameMode:                 "survival",
		Seed:                     "",
		MapSizeX:                 1024000,
		MapSizeZ:                 1024000,
		WorldHeight:              256,
		StartingClimate:          "temperate",
		Landcover:                1,
		LandcoverScale:           1,
		UpheavalRate:             0.3,
		LandformScale:            1,
		GeologicActivity:         "rare",
		GlobalTemperature:        1,
		GlobalPrecipitation:      1,
		PolarEquatorDistance:     100000,
		Forestation:              0,
		CopperDeposits:           "rare",
		TinDeposits:              "rare",
		SaplingGrowthRate:        1,
		CropGrowthRate:           1,
		TemporalStability:        true,
		TemporalStorms:           "sometimes",
		TemporalRifts:            "visible",
		TemporalGearRespawnUses:  20,
		PlayerHealthPoints:       15,
		HungerSpeed:              1,
		BodyTemperatureHardiness: 0,
		DeathPunishment:          "drop",
		DroppedItemsTimer:        10,
		MicroblockChiseling:      "stonewood",
		AllowUndergroundFarming:  false,
		AllowCoordinateHUD:       true,
		AllowMap:                 true,
		CreatureHostility:        "aggressive",
		CreatureStrength:         1,
		CreatureHealth:           1,
		GraceTimer:               3,
		HarshWinters:             true,
		FoodSpoilageRate:         1,
		FireSpread:               true,
		LightningFires:           false,
		CaveIns:                  "off",
	}
}

var (
	nameRe          = regexp.MustCompile(`^[^\x00-\x1f]{1,60}$`)
	allowedMode     = map[string]bool{"survival": true, "creative": true, "spectator": true}
	allowedClimates = map[string]bool{
		"temperate": true, "warm": true, "hot": true, "scorching": true,
		"cold": true, "icy": true,
	}
	allowedGeo    = map[string]bool{"none": true, "rare": true, "uncommon": true, "common": true}
	allowedStorms = map[string]bool{
		"off": true, "veryrare": true, "rare": true, "sometimes": true,
		"often": true, "veryoften": true,
	}
	allowedRifts     = map[string]bool{"off": true, "invisible": true, "visible": true}
	allowedDeath     = map[string]bool{"drop": true, "keep": true}
	allowedChisel    = map[string]bool{"off": true, "stonewood": true, "all": true}
	allowedHostility = map[string]bool{"aggressive": true, "passive": true, "off": true}
	allowedCaveIns   = map[string]bool{"off": true, "sandgravel": true, "sandgravelsoil": true}
	allowedDeposits  = map[string]bool{
		"never": true, "veryrare": true, "rare": true, "common": true, "verycommon": true,
	}
)

// Validate checks every wizard field and returns all problems found.
func Validate(f WizardForm) []FieldError {
	var errs []FieldError
	add := func(field, msg string) { errs = append(errs, FieldError{Field: field, Message: msg}) }

	if !nameRe.MatchString(f.ServerName) || strings.TrimSpace(f.ServerName) == "" {
		add("serverName", "Server name must be between 1 and 60 characters.")
	}
	if len(f.ServerDescription) > 512 {
		add("serverDescription", "Description must be 512 characters or fewer.")
	}
	if len(f.WelcomeMessage) > 512 {
		add("welcomeMessage", "Welcome message must be 512 characters or fewer.")
	}
	if f.MaxClients < 1 || f.MaxClients > 200 {
		add("maxClients", "Max clients must be between 1 and 200.")
	}
	if f.Port < 1024 || f.Port > 65535 {
		add("port", "Port must be between 1024 and 65535.")
	}
	if !allowedMode[f.GameMode] {
		add("gameMode", "Unknown game mode.")
	}
	if len(f.Seed) > 128 {
		add("seed", "Seed must be 128 characters or fewer.")
	}
	if f.MapSizeX < 1024 || f.MapSizeX > 10000000 {
		add("mapSizeX", "Map width must be between 1,024 and 10,000,000.")
	}
	if f.MapSizeZ < 1024 || f.MapSizeZ > 10000000 {
		add("mapSizeZ", "Map length must be between 1,024 and 10,000,000.")
	}
	if !oneOf(f.WorldHeight, 256, 320, 384, 512) {
		add("worldHeight", "World height must be one of 256, 320, 384 or 512.")
	}
	if !allowedClimates[f.StartingClimate] {
		add("startingClimate", "Unknown starting climate.")
	}
	if between(f.Landcover, 0, 1) != nil {
		add("landcover", "Landcover must be between 0% and 100%.")
	}
	if between(f.LandcoverScale, 0.1, 10) != nil {
		add("landcoverScale", "Landcover scale must be between 10% and 1000%.")
	}
	if between(f.UpheavalRate, 0, 1) != nil {
		add("upheavalRate", "Upheaval rate must be between 0% and 100%.")
	}
	if between(f.LandformScale, 0.2, 2) != nil {
		add("landformScale", "Landform scale must be between 20% and 200%.")
	}
	if !allowedGeo[f.GeologicActivity] {
		add("geologicActivity", "Unknown geologic activity level.")
	}
	if between(f.GlobalTemperature, 0.1, 5) != nil {
		add("globalTemperature", "Temperature multiplier must be between 0.1 and 5.")
	}
	if between(f.GlobalPrecipitation, 0.1, 5) != nil {
		add("globalPrecipitation", "Precipitation multiplier must be between 0.1 and 5.")
	}
	if f.PolarEquatorDistance < 5000 || f.PolarEquatorDistance > 400000 {
		add("polarEquatorDistance", "Polar-equator distance must be between 5k and 400k blocks.")
	}
	if between(f.Forestation, -1, 1) != nil {
		add("forestation", "Forestation must be between -100% and +100%.")
	}
	if !allowedDeposits[f.CopperDeposits] || !allowedDeposits[f.TinDeposits] {
		add("copperDeposits", "Unknown deposit frequency.")
	}
	if between(f.SaplingGrowthRate, 0.1, 20) != nil {
		add("saplingGrowthRate", "Sapling growth rate must be between 0.1 and 20.")
	}
	if between(f.CropGrowthRate, 0.1, 10) != nil {
		add("cropGrowthRate", "Crop growth rate must be between 0.1 and 10.")
	}
	if !allowedStorms[f.TemporalStorms] {
		add("temporalStorms", "Unknown temporal storm frequency.")
	}
	if !allowedRifts[f.TemporalRifts] {
		add("temporalRifts", "Unknown temporal rift setting.")
	}
	if f.TemporalGearRespawnUses < -1 || f.TemporalGearRespawnUses > 20 {
		add("temporalGearRespawnUses", "Gear respawn uses must be -1 (infinite) or 1 to 20.")
	}
	if f.TemporalGearRespawnUses == 0 {
		add("temporalGearRespawnUses", "Use -1 for infinite or a value from 1 to 20.")
	}
	if f.PlayerHealthPoints < 5 || f.PlayerHealthPoints > 100 {
		add("playerHealthPoints", "Player health must be between 5 and 100.")
	}
	if between(f.HungerSpeed, 0.25, 4) != nil {
		add("hungerSpeed", "Hunger speed must be between 0.25x and 4x.")
	}
	if f.BodyTemperatureHardiness < -5 || f.BodyTemperatureHardiness > 10 {
		add("bodyTemperatureHardiness", "Temperature hardiness must be between -5°C and +10°C.")
	}
	if !allowedDeath[f.DeathPunishment] {
		add("deathPunishment", "Unknown death punishment.")
	}
	if f.DroppedItemsTimer < 1 || f.DroppedItemsTimer > 1440 {
		add("droppedItemsTimer", "Despawn timer must be between 1 and 1440 minutes.")
	}
	if !allowedChisel[f.MicroblockChiseling] {
		add("microblockChiseling", "Unknown chiseling mode.")
	}
	if !allowedHostility[f.CreatureHostility] {
		add("creatureHostility", "Unknown creature hostility.")
	}
	if between(f.CreatureStrength, 0.25, 4) != nil {
		add("creatureStrength", "Creature strength must be between 0.25x and 4x.")
	}
	if between(f.CreatureHealth, 0.25, 4) != nil {
		add("creatureHealth", "Creature health must be between 0.25x and 4x.")
	}
	if f.GraceTimer < 0 || f.GraceTimer > 10 {
		add("graceTimer", "Grace timer must be between 0 and 10 days.")
	}
	if between(f.FoodSpoilageRate, 0.25, 4) != nil {
		add("foodSpoilageRate", "Food spoilage must be between 0.25x and 4x.")
	}
	if !allowedCaveIns[f.CaveIns] {
		add("caveIns", "Unknown cave-in mode.")
	}
	return errs
}

// ApplyPreset fills the wizard form with a playstyle template, leaving server
// identity fields (name, description, password, port) untouched.
func ApplyPreset(f *WizardForm, id string) error {
	presets := map[string]bool{}
	for _, p := range Presets() {
		presets[p.ID] = true
	}
	if !presets[id] {
		return fmt.Errorf("unknown playstyle preset %q", id)
	}
	p := DefaultWizardForm()
	p.Preset = id

	switch id {
	case "wildernesssurvival":
		p.HungerSpeed = 1.5
		p.PlayerHealthPoints = 15
		p.CreatureStrength = 1.5
		p.CreatureHealth = 1.5
		p.CaveIns = "sandgravelsoil"
		p.GraceTimer = 0
		p.HarshWinters = true
		p.Landcover = 1
		p.LandcoverScale = 0.8
		p.UpheavalRate = 0.45
		p.GeologicActivity = "uncommon"
		p.TemporalStorms = "often"
		p.GlobalTemperature = 0.8
	case "exploration":
		p.Landcover = 0.7
		p.LandcoverScale = 2
		p.UpheavalRate = 0.4
		p.LandformScale = 1.4
		p.Forestation = 0.2
		p.PolarEquatorDistance = 150000
		p.CopperDeposits = "common"
		p.TinDeposits = "common"
		p.GraceTimer = 5
	case "homosapiens":
		p.Landcover = 0.85
		p.LandcoverScale = 1.5
		p.GlobalTemperature = 0.85
		p.GlobalPrecipitation = 0.85
		p.CopperDeposits = "never"
		p.TinDeposits = "never"
		p.CreatureStrength = 2
		p.CreatureHealth = 1.5
		p.AllowMap = false
		p.AllowCoordinateHUD = false
		p.HarshWinters = true
		p.CaveIns = "sandgravel"
		p.GraceTimer = 0
		p.TemporalStorms = "rare"
	case "creativebuilding":
		p.GameMode = "creative"
		p.Preset = "creativebuilding"
		p.TemporalStability = false
		p.TemporalStorms = "off"
		p.TemporalRifts = "off"
		p.CreatureHostility = "passive"
		p.CreatureStrength = 0.25
		p.PlayerHealthPoints = 100
		p.HungerSpeed = 0.25
		p.GraceTimer = 10
		p.FireSpread = false
		p.LightningFires = false
		p.CaveIns = "off"
		p.Landcover = 1
		p.UpheavalRate = 0.1
		p.Forestation = -0.5
	}

	// Preserve identity/rules from step 1 except game mode.
	f.GameMode = p.GameMode
	f.Seed = p.Seed
	f.MapSizeX, f.MapSizeZ, f.WorldHeight = p.MapSizeX, p.MapSizeZ, p.WorldHeight
	f.StartingClimate = p.StartingClimate
	f.Landcover, f.LandcoverScale = p.Landcover, p.LandcoverScale
	f.UpheavalRate, f.LandformScale = p.UpheavalRate, p.LandformScale
	f.GeologicActivity = p.GeologicActivity
	f.GlobalTemperature, f.GlobalPrecipitation = p.GlobalTemperature, p.GlobalPrecipitation
	f.PolarEquatorDistance = p.PolarEquatorDistance
	f.Forestation = p.Forestation
	f.CopperDeposits, f.TinDeposits = p.CopperDeposits, p.TinDeposits
	f.SaplingGrowthRate, f.CropGrowthRate = p.SaplingGrowthRate, p.CropGrowthRate
	f.TemporalStability = p.TemporalStability
	f.TemporalStorms, f.TemporalRifts = p.TemporalStorms, p.TemporalRifts
	f.TemporalGearRespawnUses = p.TemporalGearRespawnUses
	f.PlayerHealthPoints, f.HungerSpeed = p.PlayerHealthPoints, p.HungerSpeed
	f.BodyTemperatureHardiness = p.BodyTemperatureHardiness
	f.DeathPunishment, f.DroppedItemsTimer = p.DeathPunishment, p.DroppedItemsTimer
	f.MicroblockChiseling = p.MicroblockChiseling
	f.AllowUndergroundFarming, f.AllowCoordinateHUD, f.AllowMap = p.AllowUndergroundFarming, p.AllowCoordinateHUD, p.AllowMap
	f.CreatureHostility, f.CreatureStrength, f.CreatureHealth = p.CreatureHostility, p.CreatureStrength, p.CreatureHealth
	f.GraceTimer, f.HarshWinters = p.GraceTimer, p.HarshWinters
	f.FoodSpoilageRate, f.FireSpread, f.LightningFires = p.FoodSpoilageRate, p.FireSpread, p.LightningFires
	f.CaveIns = p.CaveIns
	f.Preset = id
	return nil
}

// playStyleCodes maps ChronoHost preset IDs to the exact values the game
// writes into WorldConfig. The "standard" preset is called "surviveandbuild"
// in the game, with its own localization key.
func playStyleCodes(preset string) (playStyle, langCode string) {
	switch preset {
	case "wildernesssurvival", "exploration", "homosapiens":
		return preset, "preset-" + preset
	case "creativebuilding":
		return preset, preset
	default:
		return "surviveandbuild", "surviveandbuild-bands"
	}
}

// BuildServerConfig translates a wizard form into a serverconfig.json-compatible
// document. The playstyle preset key is derived from the selected preset.
func BuildServerConfig(f WizardForm) (map[string]any, error) {
	if errs := Validate(f); len(errs) > 0 {
		return nil, fmt.Errorf("invalid configuration: %s (field %s)", errs[0].Message, errs[0].Field)
	}
	seed := strings.TrimSpace(f.Seed)
	if seed == "" {
		seed = fmt.Sprintf("%d", randomSeed())
	}

	password := any(nil)
	if f.Password != "" {
		password = f.Password
	}
	description := any(nil)
	if f.ServerDescription != "" {
		description = f.ServerDescription
	}

	playStyle, langCode := playStyleCodes(f.Preset)

	world := map[string]any{
		"gameMode":                   f.GameMode,
		"startingClimate":            f.StartingClimate,
		"spawnRadius":                "50",
		"graceTimer":                 strconv.Itoa(f.GraceTimer),
		"deathPunishment":            f.DeathPunishment,
		"droppedItemsTimer":          strconv.Itoa(f.DroppedItemsTimer * 60),
		"seasons":                    "enabled",
		"playerlives":                "-1",
		"lungCapacity":               "40000",
		"daysPerMonth":               "9",
		"harshWinters":               boolString(f.HarshWinters),
		"blockGravity":               blockGravity(f.CaveIns),
		"caveIns":                    caveIns(f.CaveIns),
		"allowUndergroundFarming":    f.AllowUndergroundFarming,
		"noLiquidSourceTransport":    "false",
		"bodyTemperatureResistance":  strconv.Itoa(f.BodyTemperatureHardiness),
		"creatureHostility":          f.CreatureHostility,
		"creatureStrength":           formatFloat(f.CreatureStrength),
		"creatureSwimSpeed":          "1",
		"creatureHealth":             formatFloat(f.CreatureHealth),
		"playerHealthPoints":         strconv.Itoa(f.PlayerHealthPoints),
		"playerHungerSpeed":          formatFloat(f.HungerSpeed),
		"playerHealthRegenSpeed":     "1",
		"playerMoveSpeed":            "1.5",
		"foodSpoilSpeed":             formatFloat(f.FoodSpoilageRate),
		"saplingGrowthRate":          formatFloat(f.SaplingGrowthRate),
		"cropGrowthRateMul":          formatFloat(f.CropGrowthRate),
		"toolDurability":             "1",
		"toolMiningSpeed":            "1",
		"propickNodeSearchRadius":    "6",
		"microblockChiseling":        f.MicroblockChiseling,
		"allowCoordinateHud":         f.AllowCoordinateHUD,
		"allowMap":                   f.AllowMap,
		"colorAccurateWorldmap":      false,
		"loreContent":                true,
		"clutterObtainable":          "ifrepaired",
		"lightningFires":             f.LightningFires,
		"allowTimeswitch":            false,
		"temporalStability":          f.TemporalStability,
		"temporalStorms":             f.TemporalStorms,
		"tempstormDurationMul":       "1",
		"temporalRifts":              f.TemporalRifts,
		"temporalGearRespawnUses":    strconv.Itoa(f.TemporalGearRespawnUses),
		"temporalStormSleeping":      "0",
		"worldClimate":               "realistic",
		"landcover":                  formatFloat(f.Landcover),
		"oceanscale":                 formatFloat(f.LandcoverScale),
		"upheavelCommonness":         formatFloat(f.UpheavalRate),
		"geologicActivity":           formatFloat(geologicActivity(f.GeologicActivity)),
		"landformScale":              formatFloat(f.LandformScale),
		"worldWidth":                 strconv.Itoa(f.MapSizeX),
		"worldLength":                strconv.Itoa(f.MapSizeZ),
		"worldEdge":                  "traversable",
		"polarEquatorDistance":       strconv.Itoa(f.PolarEquatorDistance),
		"storyStructuresDistScaling": "1",
		"globalTemperature":          formatFloat(f.GlobalTemperature),
		"globalPrecipitation":        formatFloat(f.GlobalPrecipitation),
		"globalForestation":          formatFloat(f.Forestation),
		"globalDepositSpawnRate":     "1",
		"surfaceCopperDeposits":      formatFloat(depositRate(f.CopperDeposits, false)),
		"surfaceTinDeposits":         formatFloat(depositRate(f.TinDeposits, true)),
		"snowAccum":                  boolString(f.HarshWinters),
		"allowLandClaiming":          true,
		"classExclusiveRecipes":      true,
		"auctionHouse":               true,
	}

	return map[string]any{
		"ConfigVersion":      "1.10",
		"ServerName":         f.ServerName,
		"ServerUrl":          nil,
		"ServerDescription":  description,
		"WelcomeMessage":     f.WelcomeMessage,
		"Ip":                 nil,
		"Port":               f.Port,
		"Upnp":               false,
		"CompressPackets":    true,
		"AdvertiseServer":    f.Advertise,
		"MaxClients":         f.MaxClients,
		"Password":           password,
		"MapSizeX":           f.MapSizeX,
		"MapSizeY":           f.WorldHeight,
		"MapSizeZ":           f.MapSizeZ,
		"ServerLanguage":     "en",
		"MaxChunkRadius":     12,
		"DefaultRoleCode":    defaultRoleCode,
		"Roles":              DefaultRoles(),
		"NextPlayerGroupUid": 10,
		"WhitelistMode":      0,
		"AllowPvP":           true,
		"AllowFireSpread":    f.FireSpread,
		"AllowFallingBlocks": true,
		"HostedMode":         false,
		"ModPaths":           []string{"Mods"},
		"WorldConfig": map[string]any{
			"Seed":                seed,
			"SaveFileLocation":    "Saves/default.vcdbs",
			"WorldName":           f.ServerName,
			"AllowCreativeMode":   true,
			"PlayStyle":           playStyle,
			"PlayStyleLangCode":   langCode,
			"WorldType":           "standard",
			"WorldConfiguration":  world,
			"MapSizeY":            f.WorldHeight,
			"CreatedByPlayerName": "ChronoHost",
			"DisabledMods":        []string{},
			"RepairMode":          false,
		},
	}, nil
}

// MergeConfig merges a wizard-generated config into an existing
// serverconfig.json, preserving game-managed keys such as Roles and
// NextPlayerGroupUid. Keys in next overwrite keys in base.
func MergeConfig(base, next map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(next))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range next {
		if k == "WorldConfig" {
			bw, _ := base["WorldConfig"].(map[string]any)
			nw, _ := v.(map[string]any)
			merged := make(map[string]any, len(bw)+len(nw))
			for bk, bv := range bw {
				merged[bk] = bv
			}
			for nk, nv := range nw {
				merged[nk] = nv
			}
			out[k] = merged
			continue
		}
		out[k] = v
	}
	return out
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func geologicActivity(v string) float64 {
	switch v {
	case "none":
		return 0
	case "uncommon":
		return 0.15
	case "common":
		return 0.4
	default:
		return 0.05
	}
}

func caveIns(v string) string {
	if v == "sandgravelsoil" {
		return "on"
	}
	return "off"
}

func blockGravity(v string) string {
	if v == "sandgravelsoil" {
		return "sandgravelsoil"
	}
	return "sandgravel"
}

func depositRate(v string, tin bool) float64 {
	if tin {
		switch v {
		case "never":
			return 0
		case "veryrare":
			return 0.001
		case "common":
			return 0.05
		case "verycommon":
			return 0.2
		default:
			return 0.007
		}
	}
	switch v {
	case "never":
		return 0
	case "veryrare":
		return 0.04
	case "common":
		return 0.3
	case "verycommon":
		return 1
	default:
		return 0.12
	}
}

func oneOf(v int, allowed ...int) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func between(v, min, max float64) error {
	if v < min || v > max {
		return errors.New("out of range")
	}
	return nil
}

// Seed choices are only used when the administrator leaves the seed blank.
var seedCounter uint64

func randomSeed() uint64 {
	seedCounter++
	return 612031164 + seedCounter*7919
}

// SortedKeys returns the sorted keys of a config map, useful for stable diffs.
func SortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
