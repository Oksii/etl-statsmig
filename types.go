package main

import (
	"encoding/json"
	"fmt"
)

// flexString unmarshals a JSON string or JSON number into a Go string.
// Very old files used numeric client slot IDs instead of GUID strings for
// attacker/target fields in obituaries and damageStats.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	// Treat any non-string JSON value as its raw text representation.
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("flexString: expected string or number, got %s", b)
	}
	*f = flexString(n.String())
	return nil
}

// Hardcoded version constants for old (1.2.4) files
const (
	OldStatsVersion = "1.2.4"
	OldETVersion    = "ET 2.60b linux-x86_64 May  8 2006"
	OldModVersion   = "v2.83.1-34-ga127043"
)

// OldRoundInfo is used for reading key fields from old round_info.
// The actual round_info map is manipulated via map[string]json.RawMessage
// to preserve all fields we don't explicitly handle.
type OldRoundInfo struct {
	Round           int    `json:"round"`
	Config          string `json:"config"`
	Mapname         string `json:"mapname"`
	MatchID         string `json:"matchID"`
	RoundEnd        int64  `json:"round_end"`
	ServerIP        string `json:"server_ip"`
	ServerPort      string `json:"server_port"`
	Timelimit       string `json:"timelimit"`
	Servername      string `json:"servername"`
	Winnerteam      int    `json:"winnerteam"`
	Defenderteam    int    `json:"defenderteam"`
	RoundStart      int64  `json:"round_start"`
	NextTimeLimit   string `json:"nextTimeLimit"`
	RoundEndUnix    *int64 `json:"round_end_unix"`
	RoundStartUnix  *int64 `json:"round_start_unix"`
	OriginalMatchID string `json:"original_match_id"`
	StatsVersion    string `json:"stats_version"` // present only in already-upgraded files
	// Collections that move to gamelog in new format
	Messages    []OldMessage    `json:"messages"`
	Obituaries  []OldObituary   `json:"obituaries"`
	DamageStats []OldDamageStat `json:"damageStats"`
}

// OldMessage is a chat message from old round_info.messages
type OldMessage struct {
	GUID      string `json:"guid"`
	Command   string `json:"command"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
}

// OldObituary is a death event from old round_info.obituaries
type OldObituary struct {
	Target              flexString `json:"target"`
	Attacker            flexString `json:"attacker"`
	Timestamp           int64      `json:"timestamp"`
	MeansOfDeath        int        `json:"meansOfDeath"`
	VictimRespawnTime   float64    `json:"victimRespawnTime"`
	AttackerRespawnTime float64    `json:"attackerRespawnTime"`
}

// OldDamageStat is a damage event from old round_info.damageStats
type OldDamageStat struct {
	Damage       int        `json:"damage"`
	Target       flexString `json:"target"`
	Attacker     flexString `json:"attacker"`
	HitRegion    string     `json:"hitRegion"`
	Timestamp    int64      `json:"timestamp"`
	DamageFlags  int        `json:"damageFlags"`
	MeansOfDeath int        `json:"meansOfDeath"`
}

// OldClassSwitch is an entry in old player_stats[].class_switches
type OldClassSwitch struct {
	ToClass   string `json:"toClass"`
	FromClass string `json:"fromClass"`
	Timestamp int64  `json:"timestamp"`
}


// ShoveEntry is the new shoves_given/shoves_received value format
type ShoveEntry struct {
	Objective     string `json:"objective"`
	TimestampUnix int64  `json:"timestamp_unix"`
}

// ObjEntry is the new obj_* value format
type ObjEntry struct {
	Objective     string `json:"objective"`
	TimestampUnix int64  `json:"timestamp_unix"`
}

// Metadata is the new metadata section added by the upgrade
type Metadata struct {
	Servername   string      `json:"servername"`
	Config       string      `json:"config"`
	MatchID      string      `json:"matchID"`
	ServerIP     string      `json:"server_ip"`
	ServerPort   string      `json:"server_port,omitempty"`
	StatsVersion string      `json:"stats_version"`
	ETVersion    string      `json:"et_version"`
	ModVersion   string      `json:"mod_version"`
	Features     interface{} `json:"features"`
	Scores       interface{} `json:"scores"`
}

// GamelogEvent is a single event in the gamelog array.
// All optional fields use omitempty so absent fields don't appear in JSON.
type GamelogEvent struct {
	Group     string  `json:"group"`
	Label     string  `json:"label"`
	MatchID   string  `json:"match_id"`
	RoundID   int     `json:"round_id"`
	Leveltime int64   `json:"leveltime"`
	Unixtime  *int64  `json:"unixtime,omitempty"`

	// Used by: kill, teamkill — attacker side
	Killer string `json:"killer,omitempty"`
	// Used by: kill, teamkill, suicide, shove — victim/target
	Victim string `json:"victim,omitempty"`
	// Used by: kill, teamkill, suicide, damage
	Weapon *int `json:"weapon,omitempty"`

	// Used by: message, shove, class_change, obj_*, suicide
	Player string `json:"player,omitempty"`

	// message fields
	Command string `json:"command,omitempty"`
	Message string `json:"message,omitempty"`

	// damage fields
	Damage      *int   `json:"damage,omitempty"`
	HitRegion   string `json:"hit_region,omitempty"`
	DamageFlags *int   `json:"damage_flags,omitempty"`

	// class_change
	Class string `json:"class,omitempty"`

	// objective events (obj_planted, obj_destroyed, obj_taken, etc.)
	Objective string `json:"objective,omitempty"`
	// obj_flag_captured events use "flag" (e.g. "allies_flag", "axis_flag")
	Flag string `json:"flag,omitempty"`

	// kill/teamkill — reinf times from obituary data
	VictimReinf  *float64 `json:"victim_reinf,omitempty"`
	KillerReinf  *float64 `json:"killer_reinf,omitempty"`
}

// playerContext holds per-player data needed for gamelog construction
type playerContext struct {
	Team          string
	ClassSwitches []OldClassSwitch
	// shoves_given: leveltime_str → victim GUID (only if string format = old)
	ShovesGiven map[string]string
	// obj events: label → [{leveltimeStr, objName}]
	ObjEvents []objEventEntry
}

type objEventEntry struct {
	Label         string
	LevelTimeStr  string
	ObjName       string
}
