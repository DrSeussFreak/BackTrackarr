package plex

import (
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

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

// "Sign in with Plex": the user approves BackTrackarr on Plex's own page, so
// nobody has to find or paste a token.

// Pin is one pending sign-in.
type Pin struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
}

// Server is a Plex server the signed-in account owns.
type Server struct {
	Name        string `json:"name"`
	MachineID   string `json:"clientIdentifier"`
	Provides    string `json:"provides"`
	Owned       bool   `json:"owned"`
	AccessToken string `json:"accessToken"`
	Connections []struct {
		Protocol string `json:"protocol"`
		Address  string `json:"address"`
		Port     int    `json:"port"`
		URI      string `json:"uri"`
		Local    bool   `json:"local"`
		Relay    bool   `json:"relay"`
	} `json:"connections"`
}

func tvRequest(ctx context.Context, method, u, clientID, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Client-Identifier", clientID)
	req.Header.Set("X-Plex-Product", "BackTrackarr")
	req.Header.Set("X-Plex-Version", model.Version)
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	c := &http.Client{Timeout: 15 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err // the error text must not repeat a URL
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("plex.tv answered HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

// CreatePin starts a sign-in.
func CreatePin(ctx context.Context, tv, clientID string) (*Pin, error) {
	var p Pin
	if err := tvRequest(ctx, http.MethodPost, tv+"/api/v2/pins?strong=true", clientID, "", &p); err != nil {
		return nil, err
	}
	if p.ID == 0 || p.Code == "" {
		return nil, errors.New("plex.tv didn't start a sign-in")
	}
	return &p, nil
}

// AuthURL is Plex's approval page for a pin.
func AuthURL(clientID, code string) string {
	q := url.Values{"clientID": {clientID}, "code": {code}, "context[device][product]": {"BackTrackarr"}}
	return "https://app.plex.tv/auth#?" + q.Encode()
}

// CheckPin returns the account token once the user has approved.
func CheckPin(ctx context.Context, tv, clientID string, p *Pin) (string, error) {
	var res struct {
		AuthToken string `json:"authToken"`
	}
	u := tv + "/api/v2/pins/" + strconv.FormatInt(p.ID, 10) + "?code=" + url.QueryEscape(p.Code)
	if err := tvRequest(ctx, http.MethodGet, u, clientID, "", &res); err != nil {
		return "", err
	}
	return res.AuthToken, nil
}

// OwnedServers lists the Plex servers this account owns.
func OwnedServers(ctx context.Context, tv, clientID, token string) ([]Server, error) {
	var all []Server
	if err := tvRequest(ctx, http.MethodGet, tv+"/api/v2/resources?includeHttps=1&includeRelay=0", clientID, token, &all); err != nil {
		return nil, err
	}
	var out []Server
	for _, s := range all {
		if s.Owned && strings.Contains(s.Provides, "server") {
			if s.AccessToken == "" {
				s.AccessToken = token
			}
			out = append(out, s)
		}
	}
	return out, nil
}

// BestAddress finds an address this app can actually reach, preferring your
// local network and never Plex's relay. The server must prove it's the right
// one by its machine id.
func BestAddress(ctx context.Context, s Server) (string, bool) {
	var local, remote []string
	for _, c := range s.Connections {
		if c.Relay {
			continue
		}
		if c.Local {
			if c.Address != "" && c.Port > 0 {
				local = append(local, fmt.Sprintf("http://%s:%d", c.Address, c.Port))
			}
			local = append(local, c.URI)
		} else {
			remote = append(remote, c.URI)
		}
	}
	for _, u := range append(local, remote...) {
		if u == "" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		id, err := NewClient(u, s.AccessToken).Identity(cctx)
		cancel()
		if err == nil && id == s.MachineID {
			return strings.TrimRight(u, "/"), true
		}
	}
	return "", false
}
