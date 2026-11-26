package config

import (
	"encoding/json"
	"strings"
)

const defaultRoleCode = "suplayer"

// Stock Vintage Story roles, verified against the 1.22 serverconfig.json schema.
// A config that sets DefaultRoleCode but omits the matching role is fatal on
// startup, so freshly generated configs always include this list.
const defaultRolesJSON = `[
  {
    "Code": "suvisitor",
    "PrivilegeLevel": -1,
    "Name": "Survival Visitor",
    "Description": "Can only visit this world and chat but not use/place/break anything",
    "DefaultSpawn": null,
    "ForcedSpawn": null,
    "Privileges": ["chat"],
    "RuntimePrivileges": [],
    "DefaultGameMode": 1,
    "Color": "Green",
    "LandClaimAllowance": 0,
    "LandClaimMinSize": {"X": 5, "Y": 5, "Z": 5},
    "LandClaimMaxAreas": 3,
    "AutoGrant": false
  },
  {
    "Code": "crvisitor",
    "PrivilegeLevel": -1,
    "Name": "Creative Visitor",
    "Description": "Can only visit this world, chat and fly but not use/place/break anything",
    "DefaultSpawn": null,
    "ForcedSpawn": null,
    "Privileges": ["chat"],
    "RuntimePrivileges": [],
    "DefaultGameMode": 2,
    "Color": "DarkGray",
    "LandClaimAllowance": 0,
    "LandClaimMinSize": {"X": 5, "Y": 5, "Z": 5},
    "LandClaimMaxAreas": 3,
    "AutoGrant": false
  },
  {
    "Code": "limitedsuplayer",
    "PrivilegeLevel": -1,
    "Name": "Limited Survival Player",
    "Description": "Can use/place/break blocks only in permitted areas (priv level -1), create/manage player groups and chat",
    "DefaultSpawn": null,
    "ForcedSpawn": null,
    "Privileges": ["controlplayergroups", "manageplayergroups", "chat", "build", "useblock", "attackcreatures", "attackplayers", "selfkill"],
    "RuntimePrivileges": [],
    "DefaultGameMode": 1,
    "Color": "White",
    "LandClaimAllowance": 0,
    "LandClaimMinSize": {"X": 5, "Y": 5, "Z": 5},
    "LandClaimMaxAreas": 3,
    "AutoGrant": false
  },
  {
    "Code": "limitedcrplayer",
    "PrivilegeLevel": -1,
    "Name": "Limited Creative Player",
    "Description": "Can use/place/break blocks in only in permitted areas (priv level -1), create/manage player groups, chat, fly and set his own game mode (= allows fly and change of move speed)",
    "DefaultSpawn": null,
    "ForcedSpawn": null,
    "Privileges": ["controlplayergroups", "manageplayergroups", "chat", "build", "useblock", "gamemode", "freemove", "attackcreatures", "attackplayers", "selfkill"],
    "RuntimePrivileges": [],
    "DefaultGameMode": 2,
    "Color": "LightGreen",
    "LandClaimAllowance": 0,
    "LandClaimMinSize": {"X": 5, "Y": 5, "Z": 5},
    "LandClaimMaxAreas": 3,
    "AutoGrant": false
  },
  {
    "Code": "suplayer",
    "PrivilegeLevel": 0,
    "Name": "Survival Player",
    "Description": "Can use/place/break blocks in unprotected areas (priv level 0), create/manage player groups and chat. Can claim an area of up to 8 chunks.",
    "DefaultSpawn": null,
    "ForcedSpawn": null,
    "Privileges": ["controlplayergroups", "manageplayergroups", "chat", "areamodify", "build", "useblock", "attackcreatures", "attackplayers", "selfkill"],
    "RuntimePrivileges": [],
    "DefaultGameMode": 1,
    "Color": "White",
    "LandClaimAllowance": 262144,
    "LandClaimMinSize": {"X": 5, "Y": 5, "Z": 5},
    "LandClaimMaxAreas": 3,
    "AutoGrant": false
  },
  {
    "Code": "crplayer",
    "PrivilegeLevel": 100,
    "Name": "Creative Player",
    "Description": "Can use/place/break blocks in all areas (priv level 100), create/manage player groups, chat, fly and set his own game mode (= allows fly and change of move speed). Can claim an area of up to 40 chunks.",
    "DefaultSpawn": null,
    "ForcedSpawn": null,
    "Privileges": ["controlplayergroups", "manageplayergroups", "chat", "areamodify", "build", "useblock", "gamemode", "freemove", "attackcreatures", "attackplayers", "selfkill"],
    "RuntimePrivileges": [],
    "DefaultGameMode": 2,
    "Color": "LightGreen",
    "LandClaimAllowance": 1310720,
    "LandClaimMinSize": {"X": 5, "Y": 5, "Z": 5},
    "LandClaimMaxAreas": 6,
    "AutoGrant": false
  },
  {
    "Code": "sumod",
    "PrivilegeLevel": 200,
    "Name": "Survival Moderator",
    "Description": "Can use/place/break blocks everywhere (priv level 200), create/manage own/other player groups, chat, kick/ban players and do serverwide announcements. Can claim an area of up to 4 chunks.",
    "DefaultSpawn": null,
    "ForcedSpawn": null,
    "Privileges": ["controlplayergroups", "manageplayergroups", "manageotherplayergroups", "chat", "areamodify", "build", "useblock", "buildblockseverywhere", "useblockseverywhere", "kick", "ban", "announce", "readlists", "attackcreatures", "attackplayers", "selfkill"],
    "RuntimePrivileges": [],
    "DefaultGameMode": 1,
    "Color": "Cyan",
    "LandClaimAllowance": 1310720,
    "LandClaimMinSize": {"X": 5, "Y": 5, "Z": 5},
    "LandClaimMaxAreas": 60,
    "AutoGrant": false
  },
  {
    "Code": "crmod",
    "PrivilegeLevel": 500,
    "Name": "Creative Moderator",
    "Description": "Can use/place/break blocks everywhere (priv level 500), create/manage own/other player groups, chat, kick/ban players, fly and set his own or other players game modes (= allows fly and change of move speed). Can claim an area of up to 40 chunks.",
    "DefaultSpawn": null,
    "ForcedSpawn": null,
    "Privileges": ["controlplayergroups", "manageplayergroups", "manageotherplayergroups", "chat", "areamodify", "build", "useblock", "buildblockseverywhere", "useblockseverywhere", "kick", "ban", "gamemode", "freemove", "commandplayer", "announce", "readlists", "attackcreatures", "attackplayers", "selfkill"],
    "RuntimePrivileges": [],
    "DefaultGameMode": 2,
    "Color": "Cyan",
    "LandClaimAllowance": 1310720,
    "LandClaimMinSize": {"X": 5, "Y": 5, "Z": 5},
    "LandClaimMaxAreas": 60,
    "AutoGrant": false
  },
  {
    "Code": "admin",
    "PrivilegeLevel": 99999,
    "Name": "Admin",
    "Description": "Has all privileges, including giving other players admin status.",
    "DefaultSpawn": null,
    "ForcedSpawn": null,
    "Privileges": ["build", "useblock", "buildblockseverywhere", "useblockseverywhere", "attackplayers", "attackcreatures", "freemove", "gamemode", "pickingrange", "chat", "kick", "ban", "whitelist", "setwelcome", "announce", "readlists", "give", "areamodify", "setspawn", "controlserver", "tp", "time", "grantrevoke", "root", "commandplayer", "controlplayergroups", "manageplayergroups", "selfkill", "manageotherplayergroups"],
    "RuntimePrivileges": [],
    "DefaultGameMode": 1,
    "Color": "LightBlue",
    "LandClaimAllowance": 2147483647,
    "LandClaimMinSize": {"X": 5, "Y": 5, "Z": 5},
    "LandClaimMaxAreas": 99999,
    "AutoGrant": true
  }
]`

// DefaultRoles returns an independent copy of the stock role definitions.
func DefaultRoles() []any {
	var roles []any
	if err := json.Unmarshal([]byte(defaultRolesJSON), &roles); err != nil {
		panic("config: invalid built-in role definitions: " + err.Error())
	}
	return roles
}

func roleCode(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	code, _ := m["Code"].(string)
	return strings.TrimSpace(code)
}

func roleSlice(v any) ([]any, bool) {
	switch roles := v.(type) {
	case []any:
		return roles, true
	case []map[string]any:
		out := make([]any, 0, len(roles))
		for _, role := range roles {
			out = append(out, role)
		}
		return out, true
	default:
		return nil, false
	}
}

// EnsureRoles makes sure DefaultRoleCode resolves to a defined role. Existing
// roles are preserved: only the missing stock role is appended, or the full
// stock set is installed when Roles is absent or empty.
func EnsureRoles(cfg map[string]any) bool {
	if cfg == nil {
		return false
	}
	code, _ := cfg["DefaultRoleCode"].(string)
	if strings.TrimSpace(code) == "" {
		code = defaultRoleCode
	}
	roles, ok := roleSlice(cfg["Roles"])
	if !ok || len(roles) == 0 {
		cfg["Roles"] = DefaultRoles()
		return true
	}
	for _, role := range roles {
		if strings.EqualFold(roleCode(role), code) {
			return false
		}
	}
	for _, role := range DefaultRoles() {
		if strings.EqualFold(roleCode(role), code) {
			cfg["Roles"] = append(roles, role)
			return true
		}
	}
	return false
}
