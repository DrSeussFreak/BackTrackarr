package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// ---------- Steam ----------

func (e *Engine) syncSteam(ctx context.Context, report func(string)) (int, error) {
	c := e.cfg.Shelf
	q := url.Values{"key": {c.SteamKey}, "steamid": {c.SteamID}, "include_appinfo": {"1"}, "include_played_free_games": {"1"}, "format": {"json"}}
	var res struct {
		Response struct {
			Games []json.RawMessage `json:"games"`
		} `json:"response"`
	}
	if err := shelfGetJSON(ctx, c.SteamURL+"/IPlayerService/GetOwnedGames/v1/?"+q.Encode(), nil, &res); err != nil {
		return 0, err
	}
	// A private profile answers 200 with an empty body. Nothing is removed.
	if len(res.Response.Games) == 0 {
		return 0, fmt.Errorf("Steam returned no games: set Game details to Public in your profile's privacy settings, or check STEAM_ID")
	}
	user := e.shelfUser(ctx)
	added := 0
	if err := e.st.InTx(ctx, func(ctx context.Context) error {
		for _, raw := range res.Response.Games {
			var g struct {
				AppID      int64  `json:"appid"`
				Name       string `json:"name"`
				Playtime   int64  `json:"playtime_forever"`
				LastPlayed int64  `json:"rtime_last_played"`
			}
			if err := json.Unmarshal(raw, &g); err != nil || g.AppID == 0 {
				continue
			}
			if g.Name == "" {
				g.Name = "Steam app " + strconv.FormatInt(g.AppID, 10)
			}
			it := &store.ShelfItem{MediaType: "game", Source: "steam", ExtKey: "app:" + strconv.FormatInt(g.AppID, 10), Title: g.Name,
				Platform: "Steam", PlaytimeS: g.Playtime * 60, LastActivity: g.LastPlayed, Raw: raw}
			prev, err := e.st.UpsertShelfItem(ctx, it)
			if err != nil {
				return err
			}
			if err := e.applyPlaytime(ctx, it, prev, user, &added); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return added, err
	}
	report(fmt.Sprintf("Steam: %d games", len(res.Response.Games)))
	return added, nil
}

// ---------- RetroAchievements ----------

func (e *Engine) syncRetro(ctx context.Context, report func(string)) (int, error) {
	c := e.cfg.Shelf
	user := e.shelfUser(ctx)
	added := 0
	call := func(endpoint string, count, offset int, out any) error {
		q := url.Values{"y": {c.RetroKey}, "u": {c.RetroUser}, "c": {strconv.Itoa(count)}, "o": {strconv.Itoa(offset)}}
		return shelfGetJSON(ctx, c.RetroURL+"/API/"+endpoint+".php?"+q.Encode(), nil, out)
	}
	for offset := 0; ; {
		var res struct {
			Total   int               `json:"Total"`
			Results []json.RawMessage `json:"Results"`
		}
		if err := call("API_GetUserCompletionProgress", 500, offset, &res); err != nil {
			return added, fmt.Errorf("completion progress: %w", err)
		}
		if err := e.st.InTx(ctx, func(ctx context.Context) error {
			for _, raw := range res.Results {
				var g struct {
					GameID           int64   `json:"GameID"`
					Title            string  `json:"Title"`
					ConsoleName      string  `json:"ConsoleName"`
					MaxPossible      float64 `json:"MaxPossible"`
					NumAwarded       float64 `json:"NumAwarded"`
					MostRecent       string  `json:"MostRecentAwardedDate"`
					HighestAwardKind string  `json:"HighestAwardKind"`
					HighestAwardDate string  `json:"HighestAwardDate"`
				}
				if err := json.Unmarshal(raw, &g); err != nil || g.GameID == 0 || g.Title == "" {
					continue
				}
				status := g.HighestAwardKind
				if status == "" {
					status = "in-progress"
				}
				it := &store.ShelfItem{MediaType: "game", Source: "retroachievements", ExtKey: "game:" + strconv.FormatInt(g.GameID, 10), Title: g.Title,
					Platform: g.ConsoleName, Status: status, Progress: shelfFraction(g.NumAwarded, g.MaxPossible), LastActivity: shelfTime(g.MostRecent), Raw: raw}
				if _, err := e.st.UpsertShelfItem(ctx, it); err != nil {
					return err
				}
				if at := shelfTime(g.HighestAwardDate); g.HighestAwardKind != "" && at > 0 {
					if err := e.addFinishOnce(ctx, it, user, "award:"+g.HighestAwardKind, at, &added); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return added, err
		}
		offset += len(res.Results)
		report(fmt.Sprintf("RetroAchievements: %d games", offset))
		if len(res.Results) == 0 || offset >= res.Total {
			break
		}
	}
	var recent []json.RawMessage
	if err := call("API_GetUserRecentlyPlayedGames", 50, 0, &recent); err != nil {
		return added, fmt.Errorf("recently played: %w", err)
	}
	if err := e.st.InTx(ctx, func(ctx context.Context) error {
		for _, raw := range recent {
			var g struct {
				GameID      int64  `json:"GameID"`
				Title       string `json:"Title"`
				ConsoleName string `json:"ConsoleName"`
				LastPlayed  string `json:"LastPlayed"`
			}
			if err := json.Unmarshal(raw, &g); err != nil || g.GameID == 0 || g.Title == "" {
				continue
			}
			at := shelfTime(g.LastPlayed)
			key := "game:" + strconv.FormatInt(g.GameID, 10)
			it := &store.ShelfItem{MediaType: "game", Source: "retroachievements", ExtKey: key, Title: g.Title, Platform: g.ConsoleName, LastActivity: at}
			// The completion row is the fuller raw record, so keep it if present.
			if id, err := e.st.ShelfItemID(ctx, "retroachievements", key); err != nil {
				return err
			} else if id == 0 {
				it.Raw = raw
			}
			if _, err := e.st.UpsertShelfItem(ctx, it); err != nil {
				return err
			}
			if at > 0 {
				ev := &store.ShelfEvent{ItemID: it.ID, User: user, Kind: "play", ExtID: "played:" + g.LastPlayed, At: at, Source: "retroachievements", Raw: raw}
				if err := e.addEvent(ctx, ev, &added); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return added, err
	}
	return added, nil
}

// ---------- RomM ----------

func (e *Engine) syncRomm(ctx context.Context, report func(string)) (int, error) {
	c := e.cfg.Shelf
	hdr := map[string]string{"Authorization": "Bearer " + c.RommToken}
	user := e.shelfUser(ctx)
	added := 0
	const size = 200
	for offset := 0; ; offset += size {
		var res struct {
			Items []json.RawMessage `json:"items"`
			Total int               `json:"total"`
		}
		u := fmt.Sprintf("%s/api/roms?limit=%d&offset=%d&last_played=true", c.RommURL, size, offset)
		if err := shelfGetJSON(ctx, u, hdr, &res); err != nil {
			return added, err
		}
		if err := e.st.InTx(ctx, func(ctx context.Context) error {
			for _, raw := range res.Items {
				var g struct {
					ID       int64  `json:"id"`
					Name     string `json:"name"`
					FSName   string `json:"fs_name"`
					Platform string `json:"platform_display_name"`
					RomUser  struct {
						LastPlayed string  `json:"last_played"`
						UpdatedAt  string  `json:"updated_at"`
						Rating     float64 `json:"rating"`
						Completion float64 `json:"completion"`
						Status     string  `json:"status"`
					} `json:"rom_user"`
				}
				if err := json.Unmarshal(raw, &g); err != nil || g.ID == 0 {
					continue
				}
				title := g.Name
				if title == "" {
					title = g.FSName
				}
				at := shelfTime(g.RomUser.LastPlayed)
				if title == "" || at == 0 {
					continue
				}
				it := &store.ShelfItem{MediaType: "game", Source: "romm", ExtKey: "rom:" + strconv.FormatInt(g.ID, 10), Title: title, Platform: g.Platform,
					Status: g.RomUser.Status, Rating: g.RomUser.Rating, Progress: g.RomUser.Completion / 100, LastActivity: at, Raw: raw}
				if _, err := e.st.UpsertShelfItem(ctx, it); err != nil {
					return err
				}
				ev := &store.ShelfEvent{ItemID: it.ID, User: user, Kind: "play", ExtID: "at:" + g.RomUser.LastPlayed, At: at, Source: "romm"}
				if err := e.addEvent(ctx, ev, &added); err != nil {
					return err
				}
				if g.RomUser.Status == "finished" || g.RomUser.Status == "completed_100" {
					when := shelfTime(g.RomUser.UpdatedAt)
					if when == 0 {
						when = at
					}
					if err := e.addFinishOnce(ctx, it, user, "status:"+g.RomUser.Status, when, &added); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return added, err
		}
		report(fmt.Sprintf("RomM: %d games", offset+len(res.Items)))
		if len(res.Items) < size || (res.Total > 0 && offset+size >= res.Total) {
			return added, nil
		}
	}
}

// ---------- Playnite and other game exports ----------

// Playnite is the practical route to GOG, PlayStation, Xbox, Epic and others:
// its library plugins pull those accounts in, and its export lands here.
var gameColumns = map[string][]string{
	"title":    {"name", "title", "game", "gamename"},
	"id":       {"gameid", "appid", "id"},
	"source":   {"source", "library", "store"},
	"platform": {"platforms", "platform", "console", "system"},
	"playtime": {"playtime", "timeplayed", "playtimeseconds", "playtimeminutes", "playtimehours", "hoursplayed", "minutesplayed", "hours"},
	"count":    {"playcount", "plays"},
	"last":     {"lastactivity", "lastplayed", "lastplayeddate"},
	"status":   {"completionstatus", "status"},
	"rating":   {"userscore", "myrating", "rating"},
	"creator":  {"developers", "developer", "publishers", "publisher"},
	"year":     {"releaseyear", "releasedate", "year"},
}

// gameValue flattens Playnite's nested values: {"Name": "PC"} and lists of them.
func gameValue(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case map[string]any:
		for _, k := range []string{"Name", "name"} {
			if s, ok := t[k].(string); ok {
				return s
			}
		}
	case []any:
		var parts []string
		for _, el := range t {
			if s := gameValue(el); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

// gamePlaytime reads seconds by default. A column named for hours or minutes
// is scaled, and "12h 30m" or "12:30:00" are understood.
func gamePlaytime(v, header string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	h := normHeader(header)
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		switch {
		case strings.Contains(h, "hour"):
			return int64(f * 3600)
		case strings.Contains(h, "minute"):
			return int64(f * 60)
		}
		return int64(f)
	}
	return int64(parseDuration(v, h) / time.Second)
}

// gameStream yields one game at a time, so a large library export never sits
// in memory all at once.
func gameStream(r io.Reader) (func() (map[string]any, error), error) {
	br := bufio.NewReader(r)
	if b, _ := br.Peek(3); bytes.Equal(b, []byte("\xef\xbb\xbf")) {
		br.Discard(3)
	}
	head, _ := br.Peek(4096)
	t := bytes.TrimSpace(head)
	if len(t) > 0 && t[0] == '[' {
		dec := json.NewDecoder(br)
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return func() (map[string]any, error) {
			for dec.More() {
				var v any
				if err := dec.Decode(&v); err != nil {
					return nil, err
				}
				if m, ok := v.(map[string]any); ok {
					return m, nil
				}
			}
			return nil, io.EOF
		}, nil
	}
	if len(t) > 0 && t[0] == '{' { // a wrapper such as {"games": [...]}
		var v map[string]any
		if err := json.NewDecoder(br).Decode(&v); err != nil {
			return nil, err
		}
		var list []any
		for _, val := range v {
			if l, ok := val.([]any); ok && len(l) > len(list) {
				list = l
			}
		}
		i := 0
		return func() (map[string]any, error) {
			for i < len(list) {
				i++
				if m, ok := list[i-1].(map[string]any); ok {
					return m, nil
				}
			}
			return nil, io.EOF
		}, nil
	}
	header, next, err := shelfRows(br)
	if err != nil {
		return nil, err
	}
	return func() (map[string]any, error) {
		for {
			rec, err := next()
			if err != nil {
				return nil, err
			}
			m := map[string]any{}
			for i, h := range header {
				if i < len(rec) && strings.TrimSpace(rec[i]) != "" {
					m[strings.TrimSpace(h)] = rec[i]
				}
			}
			if len(m) > 0 {
				return m, nil
			}
		}
	}, nil
}

// ImportGames reads a Playnite JSON or CSV export, or any game list with a
// name column. Re-importing turns added playtime into new session rows.
func (e *Engine) ImportGames(ctx context.Context, r io.Reader) (*ShelfImportResult, error) {
	next, err := gameStream(r)
	if err != nil {
		return nil, err
	}
	user := e.shelfUser(ctx)
	res := &ShelfImportResult{Source: "games-csv"}
	for done := false; !done; {
		if err := e.st.InTx(ctx, func(ctx context.Context) error {
			for n := 0; n < 500; n++ {
				m, err := next()
				if err == io.EOF {
					done = true
					return nil
				}
				if err != nil {
					return fmt.Errorf("game %d: %w", res.Rows+1, err)
				}
				fields := map[string]string{}
				headers := map[string]string{}
				for k, v := range m {
					n := normHeader(k)
					for field, opts := range gameColumns {
						for rank, o := range opts {
							if n != o {
								continue
							}
							// Earlier names in the list win, whatever the key order.
							if cur, ok := headers[field]; ok && gameRank(gameColumns[field], normHeader(cur)) <= rank {
								continue
							}
							if s := gameValue(v); s != "" {
								fields[field], headers[field] = s, k
							}
						}
					}
					if n == "pluginid" || n == "completionstatus" {
						res.Source = "playnite"
					}
				}
				title := fields["title"]
				if title == "" {
					continue
				}
				res.Rows++
				platform := fields["platform"]
				if platform == "" {
					platform = fields["source"]
				}
				key := hashKey(title, platform)
				if fields["id"] != "" {
					key = "id:" + fields["source"] + ":" + fields["id"]
				}
				it := &store.ShelfItem{MediaType: "game", Source: res.Source, ExtKey: key, Title: title, Platform: platform, Creator: fields["creator"],
					Status: strings.ToLower(fields["status"]), Year: shelfYear(fields["year"]), PlaytimeS: gamePlaytime(fields["playtime"], headers["playtime"]),
					LastActivity: shelfTime(fields["last"]), Raw: shelfRaw(m)}
				it.Rating, _ = strconv.ParseFloat(fields["rating"], 64)
				prev, err := e.st.UpsertShelfItem(ctx, it)
				if err != nil {
					return err
				}
				if prev.New {
					res.New++
				} else {
					res.Updated++
				}
				if err := e.applyPlaytime(ctx, it, prev, user, &res.Events); err != nil {
					return err
				}
				switch it.Status {
				case "beaten", "completed", "finished", "100%", "mastered":
					if err := e.addFinishOnce(ctx, it, user, "status:"+it.Status, it.LastActivity, &res.Events); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return res, err
		}
	}
	if res.Rows == 0 {
		return nil, fmt.Errorf("no games found in the file")
	}
	return res, nil
}

func gameRank(opts []string, n string) int {
	for i, o := range opts {
		if o == n {
			return i
		}
	}
	return len(opts)
}
