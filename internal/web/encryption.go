package web

import (
	"fmt"
	"net/http"
)

func (s *Server) routesEncryption(admin *http.ServeMux) {
	admin.HandleFunc("GET /encryption", s.encryptionPage)
	admin.HandleFunc("POST /encryption/recovery-saved", s.encryptionRecoverySaved)
	admin.HandleFunc("POST /encryption/remove-unencrypted", s.encryptionRemovePlain)
	admin.HandleFunc("POST /encryption/drive/connect", s.encryptionDriveConnect)
	admin.HandleFunc("POST /encryption/drive/disconnect", s.encryptionDriveDisconnect)
}

func (s *Server) encryptionPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store") // this page can show the recovery key
	recovery, err := s.st.PendingRecoveryKey(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	pending := s.eng.DrivePending()
	waiting := pending != nil && pending.Err == ""
	s.render(w, "encryption", M{"Title": "Encryption", "Nav": "security", "On": s.st.Encrypted(), "Recovery": recovery,
		"Plain": len(s.st.UnencryptedCopies()), "Backups": len(s.st.EncryptedBackups()),
		"DriveReady": s.eng.DriveConfigured(), "DriveOn": s.eng.DriveConnected(ctx), "DriveAuth": pending,
		"DriveLast": s.eng.DriveLastUpload(ctx), "Cfg": s.cfg, "Jobs": s.eng.Jobs.List(),
		"Refresh": waiting || s.eng.Jobs.AnyRunning(), "Flash": takeFlash(w, r)})
}

func (s *Server) encryptionRecoverySaved(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("saved") != "1" {
		s.flash(w, r, "/encryption", "Tick the box once your recovery key is saved somewhere safe.")
		return
	}
	if err := s.st.ClearRecoveryKey(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	s.flash(w, r, "/encryption", "Recovery key removed from this app. Keep your saved copy safe.")
}

func (s *Server) encryptionRemovePlain(w http.ResponseWriter, r *http.Request) {
	n, err := s.st.RemoveUnencrypted(r.Context())
	if err != nil {
		s.flash(w, r, "/encryption", "Nothing more was removed: "+err.Error())
		return
	}
	s.flash(w, r, "/encryption", fmt.Sprintf("Removed %d unencrypted files.", n))
}

func (s *Server) encryptionDriveConnect(w http.ResponseWriter, r *http.Request) {
	if _, err := s.eng.DriveConnect(r.Context()); err != nil {
		s.flash(w, r, "/encryption", err.Error())
		return
	}
	back(w, r, "/encryption")
}

func (s *Server) encryptionDriveDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.DriveDisconnect(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	s.flash(w, r, "/encryption", "Google Drive disconnected. Backups already there were left alone.")
}
