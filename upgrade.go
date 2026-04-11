package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// objLabels is the complete list of obj_* keys in player_stats that get
// format-upgraded (string value → {objective, timestamp_unix} object).
// Used for player_stats upgrade only.
var objLabels = []string{
	"obj_planted",
	"obj_defused",
	"obj_destroyed",
	"obj_repaired",
	"obj_taken",
	"obj_secured",
	"obj_returned",
	"obj_carrierkilled", // object value: {victim, weapon, objective} — handled specially
	"obj_flagcaptured",
	"obj_misc",
	"obj_escort",
}

// objGamelogLabels is the subset of obj_* player_stats keys that generate
// gamelog events. Matches the ObjectiveLabel TypeScript union in the new format:
//   obj_planted | obj_defused | obj_destroyed | obj_repaired
//   obj_taken   | obj_secured | obj_returned  | obj_carrierkilled
// plus the separate obj_flag_captured event (from obj_flagcaptured).
// obj_misc and obj_escort are player_stats tracking only — no gamelog events.
var objGamelogLabels = []string{
	"obj_planted",
	"obj_defused",
	"obj_destroyed",
	"obj_repaired",
	"obj_taken",
	"obj_secured",
	"obj_returned",
	"obj_carrierkilled",
	"obj_flagcaptured",
}

// upgradeFile reads an old-format JSON file, upgrades it, writes to the
// mirrored path under outputRoot, and restores file mtime from match timestamps.
func upgradeFile(inputPath, inputRoot, outputRoot string) error {
	data, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	// Parse as map to preserve all top-level fields
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse root: %w", err)
	}

	matchRaw, ok := root["match"]
	if !ok {
		return fmt.Errorf("no 'match' key")
	}

	var matchMap map[string]json.RawMessage
	if err := json.Unmarshal(matchRaw, &matchMap); err != nil {
		return fmt.Errorf("parse match: %w", err)
	}

	// Extract timestamps for file mtime restoration
	var endTime, startTime int64
	if v, ok := matchMap["end_time"]; ok {
		json.Unmarshal(v, &endTime) //nolint: errcheck
	}
	if v, ok := matchMap["start_time"]; ok {
		json.Unmarshal(v, &startTime) //nolint: errcheck
	}

	// Extract match_id for gamelog events
	var matchID string
	if v, ok := matchMap["match_id"]; ok {
		json.Unmarshal(v, &matchID) //nolint: errcheck
	}

	// Process rounds
	roundsRaw, ok := matchMap["rounds"]
	if !ok {
		return fmt.Errorf("no 'rounds' key")
	}

	var rounds []map[string]json.RawMessage
	if err := json.Unmarshal(roundsRaw, &rounds); err != nil {
		return fmt.Errorf("parse rounds: %w", err)
	}

	anyUpgraded := false
	for i, round := range rounds {
		rdRaw, ok := round["round_data"]
		if !ok {
			continue
		}

		var rd map[string]json.RawMessage
		if err := json.Unmarshal(rdRaw, &rd); err != nil {
			continue
		}

		// Skip rounds already containing a gamelog
		if _, has := rd["gamelog"]; has {
			continue
		}

		if err := upgradeRoundData(rd, matchID, i+1); err != nil {
			return fmt.Errorf("round %d: %w", i+1, err)
		}

		rdBytes, err := json.Marshal(rd)
		if err != nil {
			return fmt.Errorf("marshal round_data %d: %w", i+1, err)
		}
		round["round_data"] = rdBytes
		rounds[i] = round
		anyUpgraded = true
	}

	// Inject metadata.scores for upgraded files only.
	// Pass-through files (anyUpgraded == false) already have authoritative scores
	// from the native Lua module and must not be overwritten.
	// The mismatch warning from applyScoresToFile is intentionally discarded here:
	// match.alpha_score/beta_score/winner in old 1.2.4 files were written by the
	// old scoring system and are not comparable to our new algorithm's output.
	if anyUpgraded {
		rounds, _ = applyScoresToFile(rounds, matchMap)
	}

	roundsBytes, err := json.Marshal(rounds)
	if err != nil {
		return fmt.Errorf("marshal rounds: %w", err)
	}
	matchMap["rounds"] = roundsBytes

	matchBytes, err := json.Marshal(matchMap)
	if err != nil {
		return fmt.Errorf("marshal match: %w", err)
	}
	root["match"] = matchBytes

	outBytes, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal output: %w", err)
	}

	// Determine output path, mirroring directory structure
	rel, err := filepath.Rel(inputRoot, inputPath)
	if err != nil {
		return fmt.Errorf("rel path: %w", err)
	}
	outPath := filepath.Join(outputRoot, rel)

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(outPath, outBytes, 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	// Restore file timestamps: prefer end_time, fall back to start_time
	ts := endTime
	if ts == 0 {
		ts = startTime
	}
	if ts != 0 {
		t := time.Unix(ts, 0)
		os.Chtimes(outPath, t, t) //nolint: errcheck
	}

	return nil
}

// upgradeRoundData modifies a round_data map in-place, adding gamelog and
// metadata, upgrading round_info and player_stats.
func upgradeRoundData(rd map[string]json.RawMessage, matchID string, roundID int) error {
	// --- Parse round_info ---
	riRaw, ok := rd["round_info"]
	if !ok {
		return fmt.Errorf("no round_info")
	}

	// Full map for pass-through modification
	var riMap map[string]json.RawMessage
	if err := json.Unmarshal(riRaw, &riMap); err != nil {
		return fmt.Errorf("parse round_info map: %w", err)
	}

	// Typed struct for easy field access
	var ri OldRoundInfo
	if err := json.Unmarshal(riRaw, &ri); err != nil {
		return fmt.Errorf("parse round_info struct: %w", err)
	}

	// Guard: already upgraded if stats_version present
	if ri.StatsVersion != "" {
		return nil
	}

	// --- Parse player_stats ---
	psRaw, ok := rd["player_stats"]
	if !ok {
		// No player_stats: still add metadata + empty gamelog
		psRaw = json.RawMessage("{}")
	}

	// Some very old files serialise an empty player_stats as [] instead of {}.
	// Treat any JSON array as an empty map.
	trimmed := bytes.TrimSpace(psRaw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		psRaw = json.RawMessage("{}")
	}

	var psRawMap map[string]json.RawMessage
	if err := json.Unmarshal(psRaw, &psRawMap); err != nil {
		return fmt.Errorf("parse player_stats: %w", err)
	}

	// Build per-player context maps for gamelog + player_stats upgrade
	playerCtxs := make(map[string]*playerContext, len(psRawMap))
	playerMaps := make(map[string]map[string]json.RawMessage, len(psRawMap))

	for guid, raw := range psRawMap {
		pm := make(map[string]json.RawMessage)
		if err := json.Unmarshal(raw, &pm); err != nil {
			continue
		}
		playerMaps[guid] = pm

		ctx := &playerContext{}

		// Team
		if v, ok := pm["team"]; ok {
			json.Unmarshal(v, &ctx.Team) //nolint: errcheck
		}

		// class_switches
		if v, ok := pm["class_switches"]; ok {
			json.Unmarshal(v, &ctx.ClassSwitches) //nolint: errcheck
		}

		// shoves_given (old format: leveltime_str → guid string)
		if v, ok := pm["shoves_given"]; ok {
			ctx.ShovesGiven = extractOldShoveMap(v)
		}

		// obj_* events: only labels that produce gamelog events
		for _, label := range objGamelogLabels {
			if v, ok := pm[label]; ok {
				entries := extractObjEntries(label, v)
				ctx.ObjEvents = append(ctx.ObjEvents, entries...)
			}
		}

		playerCtxs[guid] = ctx
	}

	// --- Build gamelog ---
	gamelog := buildGamelog(ri, playerCtxs, matchID, roundID)

	glBytes, err := json.Marshal(gamelog)
	if err != nil {
		return fmt.Errorf("marshal gamelog: %w", err)
	}
	rd["gamelog"] = glBytes

	// --- Build metadata ---
	meta := buildMetadata(ri)
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	rd["metadata"] = metaBytes

	// --- Upgrade round_info ---
	// Add version fields
	riMap["stats_version"], _ = json.Marshal(OldStatsVersion)
	riMap["et_version"], _ = json.Marshal(OldETVersion)
	riMap["mod_version"], _ = json.Marshal(OldModVersion)
	// Remove collections now in gamelog
	delete(riMap, "messages")
	delete(riMap, "obituaries")
	delete(riMap, "damageStats")

	newRIBytes, err := json.Marshal(riMap)
	if err != nil {
		return fmt.Errorf("marshal round_info: %w", err)
	}
	rd["round_info"] = newRIBytes

	// --- Upgrade player_stats ---
	upgradeAllPlayerStats(playerMaps, ri)

	newPSBytes, err := json.Marshal(playerMaps)
	if err != nil {
		return fmt.Errorf("marshal player_stats: %w", err)
	}
	rd["player_stats"] = newPSBytes

	return nil
}

// buildGamelog constructs the gamelog event array from old data sources.
func buildGamelog(ri OldRoundInfo, playerCtxs map[string]*playerContext, matchID string, roundID int) []GamelogEvent {
	// Use original_match_id as match_id for gamelog (matches new format convention)
	gmID := ri.OriginalMatchID
	if gmID == "" {
		gmID = ri.MatchID
	}

	var events []GamelogEvent

	// Helper: infer unix timestamp in milliseconds from a leveltime value.
	// Returns nil if round_start_unix is unavailable.
	unixMs := func(leveltime int64) *int64 {
		if ri.RoundStartUnix == nil {
			return nil
		}
		v := *ri.RoundStartUnix*1000 + (leveltime - ri.RoundStart)
		return &v
	}

	base := func(group, label string, lt int64) GamelogEvent {
		return GamelogEvent{
			Group:     group,
			Label:     label,
			MatchID:   gmID,
			RoundID:   roundID,
			Leveltime: lt,
			Unixtime:  unixMs(lt),
		}
	}

	// Synthetic boundary events — generated separately, bookend the sorted inner events.
	roundStartEvent := base("server", "round_start", ri.RoundStart)
	roundEndEvent := base("server", "round_end", ri.RoundEnd)

	// 2. Messages → message events
	for _, msg := range ri.Messages {
		e := base("player", "message", msg.Timestamp)
		e.Player = msg.GUID
		e.Command = msg.Command
		e.Message = msg.Message
		events = append(events, e)
	}

	// 3. Obituaries → kill / suicide / teamkill events
	// Build GUID→team map for disambiguation
	guidTeam := make(map[string]string, len(playerCtxs))
	for guid, ctx := range playerCtxs {
		guidTeam[guid] = ctx.Team
	}

	for _, obit := range ri.Obituaries {
		lt := obit.Timestamp
		weaponVal := obit.MeansOfDeath

		if obit.Attacker == "" || obit.Attacker == obit.Target {
			// Suicide: player, weapon only (no reinf times in new format suicide spec)
			e := base("player", "suicide", lt)
			e.Player = string(obit.Target)
			e.Weapon = &weaponVal
			events = append(events, e)
		} else {
			attackerTeam := guidTeam[string(obit.Attacker)]
			targetTeam := guidTeam[string(obit.Target)]

			label := "kill"
			if attackerTeam != "" && targetTeam != "" && attackerTeam == targetTeam {
				label = "teamkill"
			}

			e := base("player", label, lt)
			e.Killer = string(obit.Attacker)
			e.Victim = string(obit.Target)
			e.Weapon = &weaponVal
			// killer_reinf / victim_reinf only on kill events, not teamkill (per new format spec)
			if label == "kill" {
				if obit.VictimRespawnTime > 0 {
					e.VictimReinf = &obit.VictimRespawnTime
				}
				if obit.AttackerRespawnTime > 0 {
					e.KillerReinf = &obit.AttackerRespawnTime
				}
			}
			events = append(events, e)
		}
	}

	// 4. DamageStats → damage events
	for _, ds := range ri.DamageStats {
		e := base("player", "damage", ds.Timestamp)
		e.Killer = string(ds.Attacker)
		e.Victim = string(ds.Target)
		dmg := ds.Damage
		e.Damage = &dmg
		flags := ds.DamageFlags
		e.DamageFlags = &flags
		w := ds.MeansOfDeath
		e.Weapon = &w
		e.HitRegion = ds.HitRegion
		events = append(events, e)
	}

	// 5. Per-player shoves_given → shove events
	// Only use shoves_given to avoid duplicates with shoves_received
	for guid, ctx := range playerCtxs {
		for ltStr, victimGUID := range ctx.ShovesGiven {
			lt, err := strconv.ParseInt(ltStr, 10, 64)
			if err != nil {
				continue
			}
			e := base("player", "shove", lt)
			e.Player = guid
			e.Victim = victimGUID
			events = append(events, e)
		}
	}

	// 6. Per-player class_switches → class_change events
	for guid, ctx := range playerCtxs {
		for _, cs := range ctx.ClassSwitches {
			e := base("player", "class_change", cs.Timestamp)
			e.Player = guid
			e.Class = cs.ToClass
			events = append(events, e)
		}
	}

	// 7. Per-player obj_* → objective events
	for guid, ctx := range playerCtxs {
		for _, oe := range ctx.ObjEvents {
			lt, err := strconv.ParseInt(oe.LevelTimeStr, 10, 64)
			if err != nil {
				continue
			}
			// Normalize label: obj_flagcaptured → obj_flag_captured
			label := oe.Label
			if label == "obj_flagcaptured" {
				label = "obj_flag_captured"
			}
			e := base("player", label, lt)
			e.Player = guid
			// obj_flag_captured uses "flag" field name; all other obj events use "objective"
			if label == "obj_flag_captured" {
				e.Flag = oe.ObjName
			} else {
				e.Objective = oe.ObjName
			}
			events = append(events, e)
		}
	}

	// Sort inner events by leveltime. round_start/round_end are always first/last
	// regardless of their leveltime values, since player-side events (class_changes
	// at round setup, etc.) can have leveltimes outside the nominal round window.
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Leveltime < events[j].Leveltime
	})

	result := make([]GamelogEvent, 0, len(events)+2)
	result = append(result, roundStartEvent)
	result = append(result, events...)
	result = append(result, roundEndEvent)

	return result
}

// buildMetadata constructs the new metadata section from old round_info.
func buildMetadata(ri OldRoundInfo) Metadata {
	return Metadata{
		Servername:   ri.Servername,
		Config:       ri.Config,
		MatchID:      ri.OriginalMatchID,
		ServerIP:     ri.ServerIP,
		ServerPort:   ri.ServerPort,
		StatsVersion: OldStatsVersion,
		ETVersion:    OldETVersion,
		ModVersion:   OldModVersion,
		Features:     nil,
		Scores:       nil,
	}
}

// upgradeAllPlayerStats upgrades shoves and obj_* in each player map, and
// removes class_switches (which has moved exclusively to the gamelog).
func upgradeAllPlayerStats(playerMaps map[string]map[string]json.RawMessage, ri OldRoundInfo) {
	for _, pm := range playerMaps {
		// class_switches moves to gamelog as class_change events; remove from player_stats.
		delete(pm, "class_switches")

		// Upgrade shoves_given and shoves_received: string value → {objective, timestamp_unix}
		for _, key := range []string{"shoves_given", "shoves_received"} {
			if raw, ok := pm[key]; ok {
				if upgraded := upgradeShoveMap(raw, ri); upgraded != nil {
					pm[key] = upgraded
				}
			}
		}

		// Upgrade obj_* fields (simple string value → {objective, timestamp_unix})
		for _, label := range objLabels {
			if raw, ok := pm[label]; ok {
				if upgraded := upgradeObjMap(raw, ri); upgraded != nil {
					pm[label] = upgraded
				}
			}
		}

		// Note: obj_flagcaptured key is intentionally kept as-is in player_stats —
		// both old and new formats use this name in player_stats. Only the gamelog
		// event label is normalised to "obj_flag_captured".
	}
}


// upgradeShoveMap converts old shove format {lt: guid_string} to new {lt: {objective, timestamp_unix}}.
// Returns nil if no changes needed.
func upgradeShoveMap(raw json.RawMessage, ri OldRoundInfo) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return nil
	}

	changed := false
	result := make(map[string]json.RawMessage, len(m))

	for lt, val := range m {
		if len(val) > 0 && val[0] == '"' {
			// Old format: plain string GUID
			var guid string
			if err := json.Unmarshal(val, &guid); err == nil {
				lti, _ := strconv.ParseInt(lt, 10, 64)
				entry := ShoveEntry{
					Objective:     guid,
					TimestampUnix: inferUnixSeconds(ri.RoundStartUnix, ri.RoundStart, lti),
				}
				b, _ := json.Marshal(entry)
				result[lt] = b
				changed = true
				continue
			}
		}
		result[lt] = val
	}

	if !changed {
		return nil
	}
	b, _ := json.Marshal(result)
	return b
}

// upgradeObjMap converts old obj_* format {lt: obj_name_string} to new {lt: {objective, timestamp_unix}}.
// Returns nil if no changes needed.
func upgradeObjMap(raw json.RawMessage, ri OldRoundInfo) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return nil
	}

	changed := false
	result := make(map[string]json.RawMessage, len(m))

	for lt, val := range m {
		if len(val) > 0 && val[0] == '"' {
			// Old format: plain string objective name
			var objName string
			if err := json.Unmarshal(val, &objName); err == nil {
				lti, _ := strconv.ParseInt(lt, 10, 64)
				entry := ObjEntry{
					Objective:     objName,
					TimestampUnix: inferUnixSeconds(ri.RoundStartUnix, ri.RoundStart, lti),
				}
				b, _ := json.Marshal(entry)
				result[lt] = b
				changed = true
				continue
			}
		}
		// Already object format — add timestamp_unix if missing
		if len(val) > 0 && val[0] == '{' {
			upgraded := ensureTimestampUnix(val, lt, ri)
			if string(upgraded) != string(val) {
				changed = true
			}
			result[lt] = upgraded
			continue
		}
		result[lt] = val
	}

	if !changed {
		return nil
	}
	b, _ := json.Marshal(result)
	return b
}

// ensureTimestampUnix adds timestamp_unix to an object-valued map entry if absent.
func ensureTimestampUnix(val json.RawMessage, ltStr string, ri OldRoundInfo) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(val, &m); err != nil {
		return val
	}
	if _, ok := m["timestamp_unix"]; ok {
		return val // already has it
	}
	lti, _ := strconv.ParseInt(ltStr, 10, 64)
	ts := inferUnixSeconds(ri.RoundStartUnix, ri.RoundStart, lti)
	b, _ := json.Marshal(ts)
	m["timestamp_unix"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return val
	}
	return out
}

// extractOldShoveMap parses {lt_str: guid_string} from a raw shoves_given value.
// Returns nil if the value is not in old string format.
func extractOldShoveMap(raw json.RawMessage) map[string]string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return nil
	}
	result := make(map[string]string, len(m))
	for lt, val := range m {
		if len(val) > 0 && val[0] == '"' {
			var guid string
			if err := json.Unmarshal(val, &guid); err == nil {
				result[lt] = guid
			}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// extractObjEntries parses objective event entries from an obj_* raw value for
// gamelog construction. Handles two value formats:
//
//   String value (most obj types):
//     { "leveltime": "gold_crate" }
//
//   Object value (obj_carrierkilled):
//     { "leveltime": { "victim": "...", "weapon": 8, "objective": "gold_crate" } }
//     The gamelog ObjectiveEvent only exposes player + objective; victim/weapon
//     remain in player_stats only.
func extractObjEntries(label string, raw json.RawMessage) []objEventEntry {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return nil
	}
	var result []objEventEntry
	for lt, val := range m {
		if len(val) == 0 {
			continue
		}
		switch val[0] {
		case '"':
			// Plain string objective name
			var name string
			if err := json.Unmarshal(val, &name); err == nil {
				result = append(result, objEventEntry{
					Label:        label,
					LevelTimeStr: lt,
					ObjName:      name,
				})
			}
		case '{':
			// Object value — extract "objective" field (used by obj_carrierkilled)
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(val, &obj); err == nil {
				if objNameRaw, ok := obj["objective"]; ok {
					var name string
					if json.Unmarshal(objNameRaw, &name) == nil {
						result = append(result, objEventEntry{
							Label:        label,
							LevelTimeStr: lt,
							ObjName:      name,
						})
					}
				}
			}
		}
	}
	return result
}

// inferUnixSeconds computes the unix timestamp in seconds for a given leveltime.
// Returns 0 if roundStartUnix is unknown.
func inferUnixSeconds(roundStartUnix *int64, roundStart, leveltime int64) int64 {
	if roundStartUnix == nil {
		return 0
	}
	deltaSeconds := (leveltime - roundStart) / 1000
	return *roundStartUnix + deltaSeconds
}
