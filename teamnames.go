package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const etlAPIBase = "https://api.etl.lol"

var httpClient = &http.Client{Timeout: 10 * time.Second}

// colorCodeRe matches a Quake3/ETL colour code: ^ followed by any non-whitespace,
// non-caret character. The full colour table spans all printable ASCII (!–~) plus
// Latin extended characters (°, Ñ, ÿ, etc.). ^^ is excluded: the engine treats the
// first ^ as a literal because the next char is ^, so the second ^ pairs with whatever
// follows — e.g. ^^7 → ^ (literal) + ^7 stripped.
var colorCodeRe = regexp.MustCompile(`\^[^\s^]`)

// ETLPlayer holds the fields we use from the ETL API player response.
type ETLPlayer struct {
	GUID               string `json:"guid"`
	DiscordID          string `json:"discord_id"`
	UserTagNoSeparator string `json:"user_tag_no_separator"`
	UserTag            string `json:"user_tag"`
}

// stripColors removes ET color codes (^X where X is [0-9a-zA-Z~]) from s.
func stripColors(s string) string {
	return colorCodeRe.ReplaceAllString(s, "")
}

// knownSeparators is the list of separator strings that may appear between a
// tag and the player nick, sorted longest-first so we always strip the longest match.
var knownSeparators = []string{" ~ ", " - ", "' ", " '", " *", " >", " #", ".", "|", "/", "'", " "}

// splitTagAlts splits a comma-separated tag alternatives string.
func splitTagAlts(tag string) []string {
	if tag == "" {
		return nil
	}
	return strings.Split(tag, ",")
}

// trimTrailingSeparator removes one trailing separator (longest first) from s.
func trimTrailingSeparator(s string) string {
	for _, sep := range knownSeparators {
		if strings.HasSuffix(s, sep) {
			return s[:len(s)-len(sep)]
		}
	}
	return s
}

// trimTrailingColorCodes removes zero or more trailing ^X sequences and any
// lone trailing ^ that has no following escape character.
func trimTrailingColorCodes(s string) string {
	for len(s) >= 2 {
		if s[len(s)-2] == '^' && isColorEscapeChar(s[len(s)-1]) {
			s = s[:len(s)-2]
			continue
		}
		break
	}
	// Strip a lone trailing ^ (no color char follows).
	if len(s) > 0 && s[len(s)-1] == '^' {
		s = s[:len(s)-1]
	}
	return s
}

// matchTagAlt checks whether alt matches inGameName.
// Returns the stripped teamname prefix (for use with mapStrippedPrefixToColored)
// and true on match.
// If alt contains {name}, coloredPrefix = alt[:idx], suffix = alt[idx+6:].
// If not, coloredPrefix = alt, suffix = "".
// The returned stripped prefix has trailing color codes and separators removed.
func matchTagAlt(alt, inGameName string) (strippedTeamPrefix string, ok bool) {
	const placeholder = "{name}"
	idx := strings.Index(alt, placeholder)

	var coloredPrefix, suffix string
	if idx >= 0 {
		coloredPrefix = alt[:idx]
		suffix = alt[idx+len(placeholder):]
	} else {
		coloredPrefix = alt
		suffix = ""
	}

	strippedName := stripColors(inGameName)
	// Strip trailing color codes then trailing separator from the tag prefix.
	stripped := trimTrailingColorCodes(coloredPrefix)
	stripped = stripColors(stripped)
	stripped = trimTrailingSeparator(stripped)

	if stripped == "" {
		return "", false
	}
	if !strings.HasPrefix(strippedName, stripped) {
		return "", false
	}
	strippedSuffix := stripColors(suffix)
	if strippedSuffix != "" && !strings.HasSuffix(strippedName, strippedSuffix) {
		return "", false
	}

	return stripped, true
}

// mapStrippedPrefixToColored maps a stripped prefix length back to the colored
// original by walking original, skipping ^X sequences, advancing a stripped
// cursor per plain char, stopping when cursor reaches len(strippedPrefix).
// Trailing color codes are not included in the result.
// isColorEscapeChar returns true if b is a valid ETLegacy color escape char
// (any non-whitespace printable byte after a caret).
func isColorEscapeChar(b byte) bool {
	return b > 0x20 && b != 0x7f
}

func mapStrippedPrefixToColored(original, strippedPrefix string) string {
	if strippedPrefix == "" {
		return ""
	}
	targetLen := len(strippedPrefix)
	strippedCursor := 0
	i := 0
	for i < len(original) && strippedCursor < targetLen {
		if original[i] == '^' && i+1 < len(original) && isColorEscapeChar(original[i+1]) {
			i += 2
			continue
		}
		strippedCursor++
		i++
	}
	return original[:i]
}

// extractCommonPrefix finds the longest prefix shared by a quorum (ceil(2N/3),
// minimum 2) of the given names, using case-insensitive comparison, then maps
// it back to the colored form of the first matching name.
//
// Requiring a quorum rather than unanimity prevents a single player whose
// nick overlaps with the tag from inflating the detected prefix.
func extractCommonPrefix(names []string) string {
	n := len(names)
	if n == 0 {
		return ""
	}
	// k = ceil(2n/3), floor of at least 2.
	k := (2*n + 2) / 3
	if k < 2 {
		k = 2
	}
	if n < k {
		return ""
	}

	type entry struct {
		lower    string
		stripped string
		original string
	}
	entries := make([]entry, n)
	for i, nm := range names {
		s := stripColors(nm)
		entries[i] = entry{strings.ToLower(s), s, nm}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].lower < entries[j].lower })

	pairLCP := func(a, b string) string {
		max := len(a)
		if len(b) < max {
			max = len(b)
		}
		for i := 0; i < max; i++ {
			if a[i] != b[i] {
				return a[:i]
			}
		}
		return a[:max]
	}

	// Slide a window of k over the sorted list; the LCP of the first and last
	// entry in each window is the longest prefix shared by all k names in that window.
	best := ""
	bestEntry := entries[0]
	for i := 0; i+k-1 < n; i++ {
		p := pairLCP(entries[i].lower, entries[i+k-1].lower)
		if len(p) > len(best) {
			best = p
			bestEntry = entries[i]
		}
	}

	if best == "" {
		return ""
	}
	best = trimTrailingSeparator(best)
	if best == "" {
		return ""
	}
	return mapStrippedPrefixToColored(bestEntry.original, bestEntry.stripped[:len(best)])
}

// isGUID returns true if s looks like a plain ET GUID (32 uppercase hex chars).
// Old-format files sometimes store GUIDs instead of Discord snowflake IDs in
// match.alpha_team[*].id / match.beta_team[*].id.
func isGUID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'F') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// fetchPlayersBy issues a GET to endpoint with the given param name repeated for each id.
// Handles both JSON-array and single-object responses.
func fetchPlayersBy(endpoint, param string, ids []string, token, base string) ([]ETLPlayer, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	params := url.Values{}
	for _, id := range ids {
		params.Add(param, id)
	}
	reqURL := base + endpoint + "?" + params.Encode()
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// The API returns 500 wrapping a 404 when none of the queried IDs exist.
		// Treat this as an empty result rather than a real error.
		if bytes.Contains(body, []byte("No players found")) {
			return nil, nil
		}
		return nil, fmt.Errorf("API status %d: %s", resp.StatusCode, body)
	}

	var players []ETLPlayer
	if err := json.Unmarshal(body, &players); err != nil {
		var p ETLPlayer
		if err2 := json.Unmarshal(body, &p); err2 != nil {
			return nil, fmt.Errorf("parse response: %w", err)
		}
		players = []ETLPlayer{p}
	}
	return players, nil
}

// fetchPlayersByDiscordID queries the ETL API for one or more Discord snowflake IDs.
func fetchPlayersByDiscordID(ids []string, token, base string) ([]ETLPlayer, error) {
	return fetchPlayersBy("/api/v2/stats/etl/players/by-id", "discord_id", ids, token, base)
}

// fetchPlayersByGUID queries the ETL API for one or more ET GUIDs.
func fetchPlayersByGUID(ids []string, token, base string) ([]ETLPlayer, error) {
	return fetchPlayersBy("/api/v2/stats/etl/players/by-guid", "guid", ids, token, base)
}

// teamNameFromPlayer tries to find a matching teamname prefix from the player's
// tag alternatives against any in-game name in names.
// On match, the colored prefix is extracted from the in-game name itself
// (via mapStrippedPrefixToColored) so that in-game color codes are preserved
// even when the API tag stores the teamname without color codes.
func teamNameFromPlayer(player ETLPlayer, names []string) string {
	alts := splitTagAlts(player.UserTagNoSeparator)
	if player.UserTag != "" {
		alts = append(alts, splitTagAlts(player.UserTag)...)
	}
	for _, name := range names {
		for _, alt := range alts {
			if strippedPrefix, ok := matchTagAlt(alt, name); ok && strippedPrefix != "" {
				return mapStrippedPrefixToColored(name, strippedPrefix)
			}
		}
	}
	return ""
}

// inferTeamNames infers alpha_teamname and beta_teamname for old-format files
// that lack these fields. Uses the ETL API to fetch the first alpha/beta team
// member's tags and matches against in-game names from round 1. Falls back to
// the longest common prefix of round-1 player names on each ET team.
//
// All errors degrade gracefully; returns ("", "") on total failure.
func inferTeamNames(rounds []map[string]json.RawMessage, matchMap map[string]json.RawMessage, token, base, filename string) (alphaTN, betaTN string) {
	if token == "" {
		return "", ""
	}

	type teamMember struct {
		ID   string `json:"id"`
		Nick string `json:"nick"`
	}

	var alphaTeam, betaTeam []teamMember
	if v, ok := matchMap["alpha_team"]; ok {
		json.Unmarshal(v, &alphaTeam) //nolint: errcheck
	}
	if v, ok := matchMap["beta_team"]; ok {
		json.Unmarshal(v, &betaTeam) //nolint: errcheck
	}

	// alpha plays ET team alphaExpectedSide(1,1) in map1/round1.
	alphaETTeam := fmt.Sprintf("%d", alphaExpectedSide(1, 1)) // "1"
	betaETTeam := fmt.Sprintf("%d", 3-alphaExpectedSide(1, 1)) // "2"

	// Collect in-game names from round 1 by ET team.
	var alphaNamesR1, betaNamesR1 []string
	if len(rounds) > 0 {
		if rdRaw, ok := rounds[0]["round_data"]; ok {
			var rd map[string]json.RawMessage
			if json.Unmarshal(rdRaw, &rd) == nil {
				if psRaw, ok := rd["player_stats"]; ok {
					var psMap map[string]map[string]json.RawMessage
					if json.Unmarshal(psRaw, &psMap) == nil {
						for _, pm := range psMap {
							var name, team string
							if v, ok := pm["name"]; ok {
								json.Unmarshal(v, &name) //nolint: errcheck
							}
							if v, ok := pm["team"]; ok {
								json.Unmarshal(v, &team) //nolint: errcheck
							}
							if name == "" {
								continue
							}
							switch team {
							case alphaETTeam:
								alphaNamesR1 = append(alphaNamesR1, name)
							case betaETTeam:
								betaNamesR1 = append(betaNamesR1, name)
							}
						}
					}
				}
			}
		}
	}

	// Collect all team member IDs, split by type: Discord snowflake vs ET GUID.
	seen := make(map[string]bool)
	var discordIDs, guidIDs []string
	for _, m := range append(alphaTeam, betaTeam...) {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if isGUID(m.ID) {
			guidIDs = append(guidIDs, m.ID)
		} else {
			discordIDs = append(discordIDs, m.ID)
		}
	}

	if len(discordIDs) == 0 && len(guidIDs) == 0 {
		// No team member data — fallback only.
		alphaTN = extractCommonPrefix(alphaNamesR1)
		betaTN = extractCommonPrefix(betaNamesR1)
		return
	}

	// Step 2: query API — one call per ID type, merge results.
	// Index by both discord_id and guid so lookups work regardless of which
	// field the team member record uses.
	playerByDiscord := make(map[string]ETLPlayer)
	playerByGUID := make(map[string]ETLPlayer)

	addPlayers := func(players []ETLPlayer) {
		for _, p := range players {
			if p.DiscordID != "" {
				playerByDiscord[p.DiscordID] = p
			}
			if p.GUID != "" {
				playerByGUID[strings.ToUpper(p.GUID)] = p
			}
		}
	}

	if len(discordIDs) > 0 {
		if ps, err := fetchPlayersByDiscordID(discordIDs, token, base); err != nil {
			log.Printf("inferTeamNames: %s: discord lookup failed for [%s]: %v", filename, strings.Join(discordIDs, ", "), err)
		} else {
			addPlayers(ps)
		}
	}
	if len(guidIDs) > 0 {
		if ps, err := fetchPlayersByGUID(guidIDs, token, base); err != nil {
			log.Printf("inferTeamNames: %s: guid lookup failed for [%s]: %v", filename, strings.Join(guidIDs, ", "), err)
		} else {
			addPlayers(ps)
		}
	}

	// Step 3a: backfill discord_id into alpha_team/beta_team entries that currently
	// store a GUID, so output files have consistent Discord snowflake IDs.
	rewriteTeamIDs := func(key string) {
		v, ok := matchMap[key]
		if !ok {
			return
		}
		var members []map[string]json.RawMessage
		if json.Unmarshal(v, &members) != nil {
			return
		}
		changed := false
		for i, m := range members {
			var id string
			if raw, ok := m["id"]; ok {
				json.Unmarshal(raw, &id) //nolint: errcheck
			}
			if isGUID(id) {
				if p, ok := playerByGUID[strings.ToUpper(id)]; ok && p.DiscordID != "" {
					members[i]["id"], _ = json.Marshal(p.DiscordID)
					changed = true
				}
			}
		}
		if changed {
			matchMap[key], _ = json.Marshal(members)
		}
	}
	rewriteTeamIDs("alpha_team")
	rewriteTeamIDs("beta_team")

	// Step 3b: for each team, try members in order until a tag match is found.
	lookupPlayer := func(id string) (ETLPlayer, bool) {
		if isGUID(id) {
			p, ok := playerByGUID[strings.ToUpper(id)]
			return p, ok
		}
		p, ok := playerByDiscord[id]
		return p, ok
	}

	for _, m := range alphaTeam {
		if p, ok := lookupPlayer(m.ID); ok {
			if tn := teamNameFromPlayer(p, alphaNamesR1); tn != "" {
				alphaTN = tn
				break
			}
		}
	}
	for _, m := range betaTeam {
		if p, ok := lookupPlayer(m.ID); ok {
			if tn := teamNameFromPlayer(p, betaNamesR1); tn != "" {
				betaTN = tn
				break
			}
		}
	}

	// Step 4 fallback: common stripped prefix.
	if alphaTN == "" {
		alphaTN = extractCommonPrefix(alphaNamesR1)
	}
	if betaTN == "" {
		betaTN = extractCommonPrefix(betaNamesR1)
	}

	return
}
