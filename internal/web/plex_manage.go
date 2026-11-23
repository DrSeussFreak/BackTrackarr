package web

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
)

// BackTrackarr looks after its own Plex connection: it checks the saved token
// and address, and when they stop working it asks plex.tv for the current
// ones. A new address must prove it's the same server and accept the token
// before it's saved. If only a person can fix it, the app says so.

const (
	plexMachineKey    = "plex_machine_id"
	plexCheckEvery    = 15 * time.Minute
	plexFailsToRepair = 2 // one failed check may just be a Plex restart
)

const (
	plexStateOff      = "off"
	plexStateOK       = "ok"
	plexStateOffline  = "offline"
	plexStateSignIn   = "signin"
	plexStateRepaired = "repaired"
)

type plexHealth struct {
	State   string `json:"state"`
	Message string `json:"message"`
	Checked int64  `json:"checked"`
}

type plexManager struct {
	mu       sync.Mutex // one check at a time
	failures int

	stateMu sync.Mutex
	health  plexHealth
}

func (s *Server) plexStatus() plexHealth {
	s.plexMgr.stateMu.Lock()
	defer s.plexMgr.stateMu.Unlock()
	return s.plexMgr.health
}

// RunPlexManager checks the Plex connection shortly after start, then regularly.
func (s *Server) RunPlexManager(ctx context.Context) {
	wait := time.NewTimer(20 * time.Second)
	defer wait.Stop()
	select {
	case <-ctx.Done():
		return
	case <-wait.C:
	}
	s.checkPlex(ctx)
	t := time.NewTicker(plexCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkPlex(ctx)
		}
	}
}

func (s *Server) checkPlex(ctx context.Context) plexHealth {
	s.plexMgr.mu.Lock()
	defer s.plexMgr.mu.Unlock()
	h := s.inspectPlex(ctx)
	h.Checked = time.Now().Unix()
	s.plexMgr.stateMu.Lock()
	changed := s.plexMgr.health.State != h.State
	s.plexMgr.health = h
	s.plexMgr.stateMu.Unlock()
	if changed && h.State != plexStateOK && h.State != plexStateOff {
		s.log.Warn("plex connection", "state", h.State)
	}
	return h
}

func (s *Server) inspectPlex(ctx context.Context) plexHealth {
	if s.cfg.PlexURL == "" || s.cfg.PlexToken == "" {
		return plexHealth{State: plexStateOff, Message: "Plex isn't connected."}
	}
	c := plex.NewClient(s.cfg.PlexURL, s.cfg.PlexToken)
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// /identity answers without a token, so this call is what proves the key works.
	_, err := c.Accounts(cctx)
	if err == nil {
		s.plexMgr.failures = 0
		if id, err := c.Identity(cctx); err == nil {
			if old, _ := s.st.GetKV(ctx, plexMachineKey); old != id {
				_ = s.st.SetKV(ctx, plexMachineKey, id)
			}
		}
		return plexHealth{State: plexStateOK, Message: "Connected. The saved key and address are working."}
	}
	rejected := errors.Is(err, plex.ErrUnauthorized)
	s.plexMgr.failures++
	if !rejected && s.plexMgr.failures < plexFailsToRepair {
		return plexHealth{State: plexStateOffline, Message: "Can't reach Plex right now. Checking again soon."}
	}
	return s.repairPlex(ctx, rejected)
}

// repairPlex asks plex.tv for this server's current key and address.
func (s *Server) repairPlex(ctx context.Context, rejected bool) plexHealth {
	offline := plexHealth{State: plexStateOffline, Message: "Can't reach Plex. The saved key was kept, and it will keep checking."}
	signIn := plexHealth{State: plexStateSignIn, Message: "Plex no longer accepts the saved key. Sign in again to fix it."}
	giveUp := offline
	if rejected {
		giveUp = signIn
	}
	if s.cfg.PlexTVURL == "" {
		return giveUp
	}
	clientID, err := s.plexClientID(ctx)
	if err != nil {
		return giveUp
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	servers, err := plex.OwnedServers(cctx, s.cfg.PlexTVURL, clientID, s.cfg.PlexToken)
	if err != nil {
		return giveUp // a server-only key can't list servers, so that alone proves nothing
	}
	machine, _ := s.st.GetKV(ctx, plexMachineKey)
	var srv *plex.Server
	for i := range servers {
		if servers[i].MachineID == machine {
			srv = &servers[i]
		}
	}
	if srv == nil && machine == "" && len(servers) == 1 {
		srv = &servers[0]
	}
	if srv == nil {
		return giveUp
	}
	addr, ok := plex.BestAddress(cctx, *srv)
	if !ok {
		return giveUp
	}
	if addr == s.cfg.PlexURL && srv.AccessToken == s.cfg.PlexToken {
		return giveUp
	}
	if _, err := plex.NewClient(addr, srv.AccessToken).Accounts(cctx); err != nil {
		return giveUp
	}
	if err := s.saveSettings(ctx, map[string]string{"PLEX_URL": addr, "PLEX_TOKEN": srv.AccessToken}, nil); err != nil {
		return giveUp
	}
	_ = s.st.SetKV(ctx, plexMachineKey, srv.MachineID)
	s.plexMgr.failures = 0
	s.log.Info("plex connection repaired from plex.tv", "server", srv.Name)
	s.restartSoon()
	return plexHealth{State: plexStateRepaired, Message: "Plex's address or key changed, so BackTrackarr updated them automatically."}
}

func (s *Server) apiPlexHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h := s.plexStatus()
	if h.State == "" {
		h = s.checkPlex(r.Context())
	}
	writeJSON(w, h)
}

func (s *Server) setupPlexCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, s.checkPlex(r.Context()))
}
