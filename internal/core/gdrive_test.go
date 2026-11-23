package core

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// fakeDrive implements the parts of Google's API the app uses.
type fakeDrive struct {
	mu        sync.Mutex
	files     map[string][]byte
	order     []string
	trashed   map[string]bool
	session   []byte
	dropOnce  bool
	corrupt   bool
	sawScopes []string
	next      int
}

func (f *fakeDrive) handler(srv **httptest.Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/device/code":
			r.ParseForm()
			f.sawScopes = append(f.sawScopes, r.Form.Get("scope"))
			json.NewEncoder(w).Encode(map[string]any{"device_code": "d", "user_code": "ABCD-EFGH", "verification_url": "https://google.test/device", "expires_in": 60, "interval": 1})
		case r.URL.Path == "/token":
			json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "refresh_token": "rt", "expires_in": 3600})
		case r.Header.Get("Authorization") != "Bearer at":
			http.Error(w, "no", http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/drive/files":
			out := []map[string]string{}
			if strings.Contains(r.URL.Query().Get("q"), "in parents") {
				for i := len(f.order) - 1; i >= 0; i-- {
					if !f.trashed[f.order[i]] {
						out = append(out, map[string]string{"id": f.order[i]})
					}
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"files": out})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/drive/files/"):
			json.NewEncoder(w).Encode(map[string]any{"id": "folder", "trashed": false})
		case r.Method == http.MethodPost && r.URL.Path == "/drive/files":
			json.NewEncoder(w).Encode(map[string]string{"id": "folder"})
		case r.Method == http.MethodPatch:
			f.trashed[strings.TrimPrefix(r.URL.Path, "/drive/files/")] = true
			w.Write([]byte("{}"))
		case r.Method == http.MethodPost && r.URL.Path == "/upload/files":
			f.session = nil
			w.Header().Set("Location", (*srv).URL+"/session")
		case r.Method == http.MethodPut && r.URL.Path == "/session":
			b, _ := io.ReadAll(r.Body)
			cr := r.Header.Get("Content-Range")
			total, _ := strconv.Atoi(cr[strings.LastIndexByte(cr, '/')+1:])
			if len(b) > 0 && f.dropOnce {
				f.dropOnce = false
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			f.session = append(f.session, b...)
			if len(f.session) < total {
				if len(f.session) > 0 {
					w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(f.session)-1))
				}
				w.WriteHeader(http.StatusPermanentRedirect)
				return
			}
			body := append([]byte(nil), f.session...)
			if f.corrupt {
				body[0] ^= 1
			}
			f.next++
			id := "file" + strconv.Itoa(f.next)
			f.files[id] = body
			f.order = append(f.order, id)
			sum := md5.Sum(body)
			json.NewEncoder(w).Encode(map[string]string{"id": id, "md5Checksum": hex.EncodeToString(sum[:])})
		default:
			http.NotFound(w, r)
		}
	})
}

func driveEngine(t *testing.T) (*Engine, *store.Store, *fakeDrive) {
	t.Helper()
	f := &fakeDrive{files: map[string][]byte{}, trashed: map[string]bool{}}
	var srv *httptest.Server
	srv = httptest.NewServer(f.handler(&srv))
	t.Cleanup(srv.Close)
	oa, api, up, wait := googleOAuth, driveAPI, driveUpload, driveBackoff
	googleOAuth, driveAPI, driveUpload, driveBackoff = srv.URL, srv.URL+"/drive", srv.URL+"/upload", time.Millisecond
	t.Cleanup(func() { googleOAuth, driveAPI, driveUpload, driveBackoff = oa, api, up, wait })

	key := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(key, []byte(strings.Repeat("ab", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	enc, err := store.PrepareEncryption(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenWith(dir, enc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := model.Media{Kind: model.Movie, Title: "SecretTitle", IDs: model.IDs{IMDB: "tt1"}}
	if err := st.UpsertMedia(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{BackupKeep: 5, DriveClientID: "id", DriveClientSecret: "secret", DriveKeep: 2}
	return New(context.Background(), cfg, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))), st, f
}

func connectDrive(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.saveDriveToken(context.Background(), driveToken{Access: "at", Refresh: "rt", Expires: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
}

func TestDriveSignInUsesNarrowScope(t *testing.T) {
	e, _, f := driveEngine(t)
	a, err := e.DriveConnect(context.Background())
	if err != nil || a.UserCode != "ABCD-EFGH" {
		t.Fatalf("%+v %v", a, err)
	}
	for end := time.Now().Add(5 * time.Second); !e.DriveConnected(context.Background()) && time.Now().Before(end); {
		time.Sleep(20 * time.Millisecond)
	}
	if !e.DriveConnected(context.Background()) || e.DrivePending() != nil {
		t.Fatal("sign-in didn't finish")
	}
	if len(f.sawScopes) != 1 || f.sawScopes[0] != driveScope {
		t.Fatalf("scopes %v", f.sawScopes)
	}
}

func TestDriveUploadIsEncryptedRetriesAndPrunes(t *testing.T) {
	e, st, f := driveEngine(t)
	connectDrive(t, e)
	ctx := context.Background()
	f.dropOnce = true
	for i := 0; i < 3; i++ {
		if err := e.BackupNow(ctx, func(string) {}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // backup names have one-second precision
	}
	if len(f.files) != 3 || !f.trashed["file1"] || f.trashed["file2"] || f.trashed["file3"] {
		t.Fatalf("files %d trashed %v", len(f.files), f.trashed)
	}
	local := st.EncryptedBackups()
	want, _ := os.ReadFile(local[len(local)-1])
	if !bytes.Equal(f.files["file3"], want) {
		t.Fatal("Drive's copy differs from the local backup")
	}
	if bytes.Contains(f.files["file3"], []byte("SecretTitle")) || bytes.Contains(f.files["file3"], []byte("SQLite format")) {
		t.Fatal("Google received readable data")
	}
	if s := e.DriveLastUpload(ctx); s.Err != "" || s.File == "" {
		t.Fatalf("%+v", s)
	}
}

func TestDriveRejectsBadCopyAndKeepsLocal(t *testing.T) {
	e, st, f := driveEngine(t)
	connectDrive(t, e)
	f.corrupt = true
	err := e.BackupNow(context.Background(), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "didn't match") {
		t.Fatalf("err %v", err)
	}
	if !f.trashed["file1"] {
		t.Fatal("bad copy stayed in Drive")
	}
	if len(st.EncryptedBackups()) != 1 {
		t.Fatal("local backup missing")
	}
}

func TestDriveRefusesWithoutEncryption(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{DriveClientID: "id", DriveClientSecret: "secret"}
	e := New(context.Background(), cfg, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := e.DriveConnect(context.Background()); err == nil {
		t.Fatal("connected without encryption")
	}
	if err := e.UploadBackup(context.Background(), "plain.db.gz"); err == nil {
		t.Fatal("uploaded an unencrypted backup")
	}
}

func TestBackupSchedule(t *testing.T) {
	e, st, _ := driveEngine(t)
	ctx := context.Background()
	now := time.Unix(1800000000, 0)
	if got := e.nextBackup(ctx, now); got != 5*time.Minute {
		t.Fatalf("first backup in %v", got)
	}
	last := func(ago time.Duration) {
		if err := st.SetKV(ctx, backupLastKey, strconv.FormatInt(now.Add(-ago).Unix(), 10)); err != nil {
			t.Fatal(err)
		}
	}
	e.cfg.BackupEvery = "1 day" // the default
	last(4 * time.Hour)
	if got := e.nextBackup(ctx, now); got != 20*time.Hour {
		t.Fatalf("a restart must keep the schedule: %v", got)
	}
	e.cfg.BackupEvery = "6 hours"
	if got := e.nextBackup(ctx, now); got != 2*time.Hour {
		t.Fatalf("6 hours: %v", got)
	}
	last(30 * 24 * time.Hour)
	if got := e.nextBackup(ctx, now); got != 5*time.Minute {
		t.Fatalf("overdue: %v", got)
	}
	e.cfg.BackupEvery = "nonsense"
	if got := e.cfg.BackupInterval(); got != 24*time.Hour {
		t.Fatalf("unknown value: %v", got)
	}
	e.cfg.BackupEvery = "1 day"
	if err := e.BackupNow(ctx, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if !e.backupSaved(ctx) {
		t.Fatal("backup time not recorded")
	}
}
