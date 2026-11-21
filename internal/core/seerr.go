package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
)

// Jellyseerr and Overseerr share one API. BackTrackarr only sends a request
// when you press Request on a recommendation.

func (e *Engine) seerrDo(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, e.cfg.SeerrURL+"/api/v1"+path, body)
	if err != nil {
		return 0, errors.New("bad Jellyseerr address")
	}
	req.Header.Set("X-Api-Key", e.cfg.SeerrKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := shelfHTTP.Do(req)
	if err != nil {
		return 0, errors.New("couldn't reach Jellyseerr")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
			return resp.StatusCode, errors.New("unreadable reply from Jellyseerr")
		}
	}
	return resp.StatusCode, nil
}

// RequestInSeerr requests a recommendation, finding its TMDB id if needed.
func (e *Engine) RequestInSeerr(ctx context.Context, recID int64) (string, error) {
	if e.cfg.SeerrURL == "" || e.cfg.SeerrKey == "" {
		return "", errors.New("Jellyseerr isn't connected")
	}
	rec, err := e.st.GetRec(ctx, recID)
	if err != nil || rec == nil {
		return "", fmt.Errorf("recommendation %d not found", recID)
	}
	mediaType := "movie"
	if rec.Kind == "show" {
		mediaType = "tv"
	}
	id := rec.TMDB
	if id == 0 {
		var res struct {
			Results []struct {
				ID           int    `json:"id"`
				MediaType    string `json:"mediaType"`
				ReleaseDate  string `json:"releaseDate"`
				FirstAirDate string `json:"firstAirDate"`
			} `json:"results"`
		}
		// Seerr rejects "+" for spaces in its search.
		q := strings.ReplaceAll(url.QueryEscape(rec.Title), "+", "%20")
		code, err := e.seerrDo(ctx, http.MethodGet, "/search?page=1&query="+q, nil, &res)
		if err != nil {
			return "", err
		}
		if code != http.StatusOK {
			return "", fmt.Errorf("Jellyseerr search answered HTTP %d", code)
		}
		for _, r := range res.Results {
			if r.MediaType != mediaType {
				continue
			}
			y := shelfYear(r.ReleaseDate + r.FirstAirDate)
			if rec.Year == 0 || y == 0 || absInt(int(y)-rec.Year) <= 1 {
				id = r.ID
				break
			}
		}
		if id == 0 {
			return "", fmt.Errorf("Jellyseerr couldn't find %q", rec.Title)
		}
	}
	body := map[string]any{"mediaType": mediaType, "mediaId": id}
	if mediaType == "tv" {
		body["seasons"] = "all"
	}
	code, err := e.seerrDo(ctx, http.MethodPost, "/request", body, nil)
	if err != nil {
		return "", err
	}
	switch {
	case code >= 200 && code < 300:
		_ = e.st.MarkRecAdded(ctx, recID, "Jellyseerr")
		return "Requested " + strconv.Quote(rec.Title) + " in Jellyseerr", nil
	case code == http.StatusConflict:
		_ = e.st.MarkRecAdded(ctx, recID, "Jellyseerr")
		return strconv.Quote(rec.Title) + " is already requested in Jellyseerr", nil
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return "", errors.New("Jellyseerr rejected the API key")
	}
	return "", fmt.Errorf("Jellyseerr answered HTTP %d", code)
}

// ArrOptions lists an instance's root folders and quality profiles, so Setup
// can offer them as choices.
func ArrOptions(ctx context.Context, kind, base, key string) (roots, profiles []string, err error) {
	a := arrClient{config.ArrInstance{Kind: kind, Name: kind, URL: strings.TrimRight(base, "/"), APIKey: key}}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var p []struct {
		Name string `json:"name"`
	}
	if err := a.do(cctx, http.MethodGet, "/qualityprofile", nil, nil, &p); err != nil {
		return nil, nil, errors.New("couldn't read its quality profiles")
	}
	var r []struct {
		Path string `json:"path"`
	}
	if err := a.do(cctx, http.MethodGet, "/rootfolder", nil, nil, &r); err != nil {
		return nil, nil, errors.New("couldn't read its root folders")
	}
	for _, v := range r {
		roots = append(roots, v.Path)
	}
	for _, v := range p {
		profiles = append(profiles, v.Name)
	}
	return roots, profiles, nil
}
