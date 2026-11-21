package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AppKind describes a self-hosted app the Setup page can find and connect.
type AppKind struct {
	Kind    string
	Label   string
	Ports   []int
	KeyPath string // where the app shows its key, opened for the user
	KeyHelp string
}

// AppKinds are the apps Setup looks for, on their usual ports.
var AppKinds = []AppKind{
	{"sonarr", "Sonarr", []int{8989, 8990}, "/settings/general", "Settings, General, API Key"},
	{"radarr", "Radarr", []int{7878, 7879}, "/settings/general", "Settings, General, API Key"},
	{"whisparr", "Whisparr", []int{6969}, "/settings/general", "Settings, General, API Key"},
	{"seerr", "Jellyseerr / Overseerr", []int{5055}, "/settings", "Settings, General, API Key"},
	{"tautulli", "Tautulli", []int{8181}, "/settings#tab_tabs-web_interface", "Settings, Web Interface, API key"},
	{"floppy", "Floppy", []int{8000}, "/settings/integrations", "Settings, Integrations, API Token"},
	{"crosswatch", "CrossWatch", []int{8787}, "/", "CrossWatch settings, API tokens"},
	{"plekt", "Plekt", nil, "/settings", "Plekt settings, API key"},
	{"jellyfin", "Jellyfin", []int{8096}, "/web/#/dashboard/keys", "Dashboard, API Keys, +"},
	{"emby", "Emby", []int{8096}, "/web/index.html#!/apikeys", "Settings, Advanced, API Keys"},
	{"audiobookshelf", "Audiobookshelf", []int{13378}, "/config/users", "Settings, Users, your user, API token"},
	{"komga", "Komga", []int{25600}, "/account/api-keys", "Account Settings, API Keys"},
	{"kavita", "Kavita", []int{5000}, "/settings", "Your account, Auth Keys"},
	{"romm", "RomM", []int{8080}, "/administration", "Administration, Client API Tokens"},
}

// FoundApp is one app found on the network.
type FoundApp struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	URL     string `json:"url"`
	KeyURL  string `json:"key_url"`
	KeyHelp string `json:"key_help"`
}

func appKind(kind string) (AppKind, bool) {
	for _, k := range AppKinds {
		if k.Kind == kind {
			return k, true
		}
	}
	return AppKind{}, false
}

var probeHTTP = &http.Client{
	Timeout: 2 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || req.URL.Host != via[0].URL.Host {
			return http.ErrUseLastResponse
		}
		return nil
	},
}

func probeBody(ctx context.Context, u string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	resp, err := probeHTTP.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return strings.ToLower(string(b))
}

// identifyApp asks an address what it is, using only public pages. It never
// sends a key.
func identifyApp(ctx context.Context, base string) string {
	if b := probeBody(ctx, base+"/System/Info/Public"); strings.Contains(b, `"serverid"`) || strings.Contains(b, `"id"`) && strings.Contains(b, `"version"`) {
		if strings.Contains(b, "jellyfin") {
			return "jellyfin"
		}
		return "emby"
	}
	if b := probeBody(ctx, base+"/status"); strings.Contains(b, `"audiobookshelf"`) {
		return "audiobookshelf"
	}
	b := probeBody(ctx, base+"/")
	_, rest, ok := strings.Cut(b, "<title")
	if !ok {
		return ""
	}
	title, _, _ := strings.Cut(rest, "</title")
	for _, k := range []string{"tautulli", "crosswatch", "plekt", "floppy", "seerr", "sonarr", "radarr", "whisparr", "komga", "kavita", "romm"} {
		if strings.Contains(title, k) {
			return k
		}
	}
	return ""
}

// ScanSubnets returns the /24 of each private address among urls.
func ScanSubnets(urls []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		ip := net.ParseIP(u.Hostname()).To4()
		if ip == nil || !ip.IsPrivate() {
			continue
		}
		p := fmt.Sprintf("%d.%d.%d", ip[0], ip[1], ip[2])
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// FindApps checks the usual ports across the given /24 networks. It runs only
// when the user asks, and doesn't send any credentials.
func FindApps(ctx context.Context, subnets []string) []FoundApp {
	ports := map[int]bool{}
	for _, k := range AppKinds {
		for _, p := range k.Ports {
			ports[p] = true
		}
	}
	type target struct {
		host string
		port int
	}
	jobs := make(chan target)
	var mu sync.Mutex
	var found []FoundApp
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := net.Dialer{Timeout: 350 * time.Millisecond}
			for t := range jobs {
				addr := net.JoinHostPort(t.host, strconv.Itoa(t.port))
				c, err := d.DialContext(ctx, "tcp", addr)
				if err != nil {
					continue
				}
				c.Close()
				base := "http://" + addr
				kind := identifyApp(ctx, base)
				k, ok := appKind(kind)
				if !ok {
					continue
				}
				mu.Lock()
				found = append(found, FoundApp{Kind: k.Kind, Label: k.Label, URL: base, KeyURL: base + k.KeyPath, KeyHelp: k.KeyHelp})
				mu.Unlock()
			}
		}()
	}
send:
	for _, s := range subnets {
		for h := 1; h <= 254; h++ {
			for p := range ports {
				select {
				case jobs <- target{fmt.Sprintf("%s.%d", s, h), p}:
				case <-ctx.Done():
					break send
				}
			}
		}
	}
	close(jobs)
	wg.Wait()
	sort.Slice(found, func(a, b int) bool {
		if found[a].Label != found[b].Label {
			return found[a].Label < found[b].Label
		}
		return found[a].URL < found[b].URL
	})
	return found
}

// TestApp checks a key against the app and returns the app's own name for
// itself, if it has one. It only reads.
func TestApp(ctx context.Context, kind, base, key string) (string, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("that address doesn't look right")
	}
	var path, header, prefix string
	switch kind {
	case "sonarr", "radarr", "whisparr":
		path, header = "/api/v3/system/status", "X-Api-Key"
	case "seerr":
		path, header = "/api/v1/auth/me", "X-Api-Key"
	case "tautulli":
		path, header = "/api/v2?"+url.Values{"apikey": {strings.TrimSpace(key)}, "cmd": {"arnold"}}.Encode(), "X-Unused"
	case "floppy":
		path, header, prefix = "/api/v1/media", "Authorization", "Bearer "
	case "crosswatch":
		path, header, prefix = "/api/app-auth/tokens/whoami", "Authorization", "Bearer "
	case "plekt":
		path, header = "/api/backup/export?collection=watchHistory&limit=1", "X-Api-Key"
	case "jellyfin", "emby":
		path, header = "/System/Info", "X-Emby-Token"
	case "audiobookshelf":
		path, header, prefix = "/api/me", "Authorization", "Bearer "
	case "komga":
		path, header = "/api/v1/users/me", "X-API-Key"
	case "kavita":
		path, header = "/api/plugin/authkey-expires", "x-api-key"
	case "romm":
		path, header, prefix = "/api/users/me", "Authorization", "Bearer "
	default:
		return "", errors.New("unknown app")
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+path, nil)
	if err != nil {
		return "", errors.New("that address doesn't look right")
	}
	req.Header.Set(header, prefix+strings.TrimSpace(key))
	req.Header.Set("Accept", "application/json")
	resp, err := shelfHTTP.Do(req)
	if err != nil {
		return "", errors.New("couldn't reach it at that address")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return "", errors.New("it rejected that key")
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return "", fmt.Errorf("it answered HTTP %d", resp.StatusCode)
	}
	var info struct {
		InstanceName string `json:"instanceName"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info)
	return strings.TrimSpace(info.InstanceName), nil
}
