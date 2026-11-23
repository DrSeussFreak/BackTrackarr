package core

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Backups go to the owner's Google Drive only after they're encrypted here, so
// Google never holds a key. The drive.file scope lets this app see only files
// it created, and old backups go to Drive's trash rather than being erased.

const (
	driveScope     = "https://www.googleapis.com/auth/drive.file"
	driveTokenKey  = "gdrive|token"
	driveFolderKey = "gdrive|folder"
	driveStatusKey = "gdrive|status"
	driveChunk     = 4 << 20 // a multiple of 256 KiB, as Google requires
	driveMark      = "backtrackarr"
)

// Replaced in tests.
var (
	googleOAuth  = "https://oauth2.googleapis.com"
	driveAPI     = "https://www.googleapis.com/drive/v3"
	driveUpload  = "https://www.googleapis.com/upload/drive/v3"
	driveBackoff = 2 * time.Second
)

type driveToken struct {
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	Expires int64  `json:"expires"`
}

// DriveAuth is a sign-in waiting for the owner.
type DriveAuth struct {
	UserCode string
	URL      string
	Err      string
}

// DriveStatus is the last upload.
type DriveStatus struct {
	At   int64  `json:"at"`
	File string `json:"file"`
	Err  string `json:"err,omitempty"`
}

type driveState struct {
	mu   sync.Mutex
	auth *DriveAuth
}

// DriveConfigured reports whether a Google client has been entered.
func (e *Engine) DriveConfigured() bool {
	return e.cfg.DriveClientID != "" && e.cfg.DriveClientSecret != ""
}

// DriveConnected reports whether Drive is signed in.
func (e *Engine) DriveConnected(ctx context.Context) bool {
	v, _ := e.st.GetKV(ctx, driveTokenKey)
	return v != ""
}

// DrivePending returns the sign-in in progress, if any.
func (e *Engine) DrivePending() *DriveAuth {
	e.drive.mu.Lock()
	defer e.drive.mu.Unlock()
	if e.drive.auth == nil {
		return nil
	}
	a := *e.drive.auth
	return &a
}

// DriveLastUpload returns the most recent upload's result.
func (e *Engine) DriveLastUpload(ctx context.Context) DriveStatus {
	var s DriveStatus
	if v, _ := e.st.GetKV(ctx, driveStatusKey); v != "" {
		_ = json.Unmarshal([]byte(v), &s)
	}
	return s
}

func oauthPost(ctx context.Context, path string, form url.Values, out any) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleOAuth+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", errors.New("couldn't reach Google")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		return resp.StatusCode, e.Error, nil
	}
	return resp.StatusCode, "", json.Unmarshal(b, out)
}

// DriveConnect starts Google's device sign-in. Backups are never sent
// unencrypted, so this needs encryption to be on.
func (e *Engine) DriveConnect(ctx context.Context) (*DriveAuth, error) {
	if !e.st.Encrypted() {
		return nil, errors.New("turn on encryption first, so nothing readable reaches Google")
	}
	if !e.DriveConfigured() {
		return nil, errors.New("enter the Google client ID and secret in Settings first")
	}
	var dc struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		URL        string `json:"verification_url"`
		ExpiresIn  int    `json:"expires_in"`
		Interval   int    `json:"interval"`
	}
	status, code, err := oauthPost(ctx, "/device/code", url.Values{"client_id": {e.cfg.DriveClientID}, "scope": {driveScope}}, &dc)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("Google refused the sign-in (%s); check the client is the \"TVs and Limited Input\" type", code)
	}
	a := &DriveAuth{UserCode: dc.UserCode, URL: dc.URL}
	e.drive.mu.Lock()
	e.drive.auth = a
	e.drive.mu.Unlock()
	go e.drivePoll(e.ctx, dc.DeviceCode, max(dc.Interval, 1), time.Duration(max(dc.ExpiresIn, 60))*time.Second)
	cp := *a
	return &cp, nil
}

func (e *Engine) driveAuthDone(msg string) {
	e.drive.mu.Lock()
	defer e.drive.mu.Unlock()
	if msg == "" {
		e.drive.auth = nil
	} else if e.drive.auth != nil {
		e.drive.auth.Err = msg
	}
}

func (e *Engine) drivePoll(ctx context.Context, device string, interval int, life time.Duration) {
	deadline := time.Now().Add(life)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(interval) * time.Second):
		}
		var tk struct {
			Access    string `json:"access_token"`
			Refresh   string `json:"refresh_token"`
			ExpiresIn int64  `json:"expires_in"`
		}
		status, code, err := oauthPost(ctx, "/token", url.Values{"client_id": {e.cfg.DriveClientID}, "client_secret": {e.cfg.DriveClientSecret},
			"device_code": {device}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}, &tk)
		switch {
		case err != nil || code == "authorization_pending":
			continue
		case code == "slow_down":
			interval += 5
			continue
		case code == "access_denied":
			e.driveAuthDone("You declined access in Google.")
			return
		case status >= 300:
			e.driveAuthDone("Google didn't complete the sign-in. Try again.")
			return
		}
		if tk.Refresh == "" {
			e.driveAuthDone("Google didn't return a lasting sign-in. Try again.")
			return
		}
		if err := e.saveDriveToken(ctx, driveToken{Access: tk.Access, Refresh: tk.Refresh, Expires: time.Now().Unix() + tk.ExpiresIn}); err != nil {
			e.driveAuthDone("Couldn't save the sign-in.")
			return
		}
		e.driveAuthDone("")
		return
	}
	e.driveAuthDone("The code expired. Try again.")
}

func (e *Engine) saveDriveToken(ctx context.Context, t driveToken) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return e.st.SetKV(ctx, driveTokenKey, string(b))
}

// DriveDisconnect signs out. Backups already in Drive stay there.
func (e *Engine) DriveDisconnect(ctx context.Context) error {
	if v, _ := e.st.GetKV(ctx, driveTokenKey); v != "" {
		var t driveToken
		if json.Unmarshal([]byte(v), &t) == nil && t.Refresh != "" {
			var ignored map[string]any
			_, _, _ = oauthPost(ctx, "/revoke", url.Values{"token": {t.Refresh}}, &ignored)
		}
	}
	for _, k := range []string{driveTokenKey, driveFolderKey, driveStatusKey} {
		if err := e.st.SetKV(ctx, k, ""); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) driveAccess(ctx context.Context) (string, error) {
	v, err := e.st.GetKV(ctx, driveTokenKey)
	if err != nil {
		return "", err
	}
	var t driveToken
	if v == "" || json.Unmarshal([]byte(v), &t) != nil || t.Refresh == "" {
		return "", errors.New("Google Drive isn't connected")
	}
	if t.Access != "" && t.Expires > time.Now().Unix()+120 {
		return t.Access, nil
	}
	var tk struct {
		Access    string `json:"access_token"`
		ExpiresIn int64  `json:"expires_in"`
	}
	status, code, err := oauthPost(ctx, "/token", url.Values{"client_id": {e.cfg.DriveClientID}, "client_secret": {e.cfg.DriveClientSecret},
		"refresh_token": {t.Refresh}, "grant_type": {"refresh_token"}}, &tk)
	if err != nil {
		return "", err
	}
	if status >= 300 {
		if code == "invalid_grant" {
			return "", errors.New("Google ended this sign-in; reconnect Drive (if the Google app is in Testing, publish it so sign-ins last)")
		}
		return "", fmt.Errorf("Google refused the refresh (%s)", code)
	}
	t.Access, t.Expires = tk.Access, time.Now().Unix()+tk.ExpiresIn
	return t.Access, e.saveDriveToken(ctx, t)
}

func (e *Engine) driveJSON(ctx context.Context, method, target string, in, out any) error {
	token, err := e.driveAccess(ctx)
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.New("couldn't reach Google Drive")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("Google Drive answered HTTP %d", resp.StatusCode)
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

func driveQuote(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}

func (e *Engine) driveFolder(ctx context.Context) (string, error) {
	if id, _ := e.st.GetKV(ctx, driveFolderKey); id != "" {
		var f struct {
			ID      string `json:"id"`
			Trashed bool   `json:"trashed"`
		}
		err := e.driveJSON(ctx, http.MethodGet, driveAPI+"/files/"+url.PathEscape(id)+"?fields=id,trashed", nil, &f)
		if err == nil && !f.Trashed {
			return id, nil
		}
	}
	name := e.cfg.DriveFolder
	if name == "" {
		name = "BackTrackarr Backups"
	}
	q := fmt.Sprintf("name='%s' and mimeType='application/vnd.google-apps.folder' and trashed=false and appProperties has { key='%s' and value='folder' }", driveQuote(name), driveMark)
	var list struct {
		Files []struct {
			ID string `json:"id"`
		} `json:"files"`
	}
	if err := e.driveJSON(ctx, http.MethodGet, driveAPI+"/files?fields=files(id)&pageSize=1&q="+url.QueryEscape(q), nil, &list); err != nil {
		return "", err
	}
	var id string
	if len(list.Files) > 0 {
		id = list.Files[0].ID
	} else {
		var made struct {
			ID string `json:"id"`
		}
		err := e.driveJSON(ctx, http.MethodPost, driveAPI+"/files?fields=id", map[string]any{"name": name,
			"mimeType": "application/vnd.google-apps.folder", "appProperties": map[string]string{driveMark: "folder"}}, &made)
		if err != nil {
			return "", err
		}
		id = made.ID
	}
	return id, e.st.SetKV(ctx, driveFolderKey, id)
}

type driveResult struct {
	ID  string `json:"id"`
	MD5 string `json:"md5Checksum"`
}

// driveSend PUTs one range of a resumable upload. It returns the next offset,
// or the finished file when Google reports the upload complete.
func (e *Engine) driveSend(ctx context.Context, session string, body []byte, offset, size int64) (int64, *driveResult, error) {
	token, err := e.driveAccess(ctx)
	if err != nil {
		return offset, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, session, bytes.NewReader(body))
	if err != nil {
		return offset, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if len(body) > 0 {
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+int64(len(body))-1, size))
	} else {
		req.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return offset, nil, errors.New("connection to Google Drive was lost")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		var r driveResult
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&r); err != nil {
			return offset, nil, err
		}
		return size, &r, nil
	case resp.StatusCode == http.StatusPermanentRedirect:
		next := int64(0)
		if v := resp.Header.Get("Range"); v != "" {
			if i := strings.LastIndexByte(v, '-'); i >= 0 {
				if n, err := strconv.ParseInt(v[i+1:], 10, 64); err == nil {
					next = n + 1
				}
			}
		}
		return next, nil, nil
	}
	return offset, nil, fmt.Errorf("Google Drive answered HTTP %d", resp.StatusCode)
}

// UploadBackup sends an encrypted backup to Drive, checks what arrived, then
// moves backups past the keep count to Drive's trash.
func (e *Engine) UploadBackup(ctx context.Context, name string) (err error) {
	defer func() {
		st := DriveStatus{At: time.Now().Unix(), File: filepath.Base(name)}
		if err != nil {
			st.Err = err.Error()
		}
		if b, merr := json.Marshal(st); merr == nil {
			_ = e.st.SetKV(context.WithoutCancel(ctx), driveStatusKey, string(b))
		}
	}()
	if !strings.HasSuffix(name, ".btbak") || !e.st.Encrypted() {
		return errors.New("only encrypted backups can be sent to Google Drive")
	}
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	folder, err := e.driveFolder(ctx)
	if err != nil {
		return err
	}
	token, err := e.driveAccess(ctx)
	if err != nil {
		return err
	}
	meta, _ := json.Marshal(map[string]any{"name": filepath.Base(name), "parents": []string{folder},
		"mimeType": "application/octet-stream", "appProperties": map[string]string{driveMark: "backup"}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, driveUpload+"/files?uploadType=resumable&fields=id,md5Checksum", bytes.NewReader(meta))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-Upload-Content-Type", "application/octet-stream")
	req.Header.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.New("couldn't reach Google Drive")
	}
	resp.Body.Close()
	session := resp.Header.Get("Location")
	if resp.StatusCode >= 300 || session == "" {
		return fmt.Errorf("Google Drive refused the upload (HTTP %d)", resp.StatusCode)
	}
	buf := make([]byte, driveChunk)
	var done *driveResult
	var offset int64
	for tries := 0; done == nil; {
		n, rerr := f.ReadAt(buf, offset)
		if rerr != nil && rerr != io.EOF {
			return rerr
		}
		next, r, serr := e.driveSend(ctx, session, buf[:n], offset, size)
		if serr == nil {
			done = r
			if next > offset {
				tries = 0
			}
			offset = next
			continue
		}
		if tries++; tries > 5 {
			return serr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(driveBackoff * time.Duration(tries)):
		}
		// Ask how much actually arrived before sending more.
		if next, r, qerr := e.driveSend(ctx, session, nil, offset, size); qerr == nil {
			done, offset = r, next
		}
	}
	if done.MD5 != sum {
		// A bad copy must never count toward the keep limit.
		_ = e.driveJSON(ctx, http.MethodPatch, driveAPI+"/files/"+url.PathEscape(done.ID), map[string]any{"trashed": true}, nil)
		return errors.New("the copy in Google Drive didn't match, so it was discarded")
	}
	return e.pruneDrive(ctx, folder)
}

func (e *Engine) pruneDrive(ctx context.Context, folder string) error {
	keep := e.cfg.DriveKeep
	if keep <= 0 {
		return nil
	}
	q := fmt.Sprintf("'%s' in parents and trashed=false and appProperties has { key='%s' and value='backup' }", driveQuote(folder), driveMark)
	var ids []string
	for page := ""; ; {
		var list struct {
			Next  string `json:"nextPageToken"`
			Files []struct {
				ID string `json:"id"`
			} `json:"files"`
		}
		u := driveAPI + "/files?fields=nextPageToken,files(id)&orderBy=createdTime+desc&pageSize=200&q=" + url.QueryEscape(q)
		if page != "" {
			u += "&pageToken=" + url.QueryEscape(page)
		}
		if err := e.driveJSON(ctx, http.MethodGet, u, nil, &list); err != nil {
			return err
		}
		for _, f := range list.Files {
			ids = append(ids, f.ID)
		}
		if page = list.Next; page == "" {
			break
		}
	}
	for i := keep; i < len(ids); i++ {
		if err := e.driveJSON(ctx, http.MethodPatch, driveAPI+"/files/"+url.PathEscape(ids[i]), map[string]any{"trashed": true}, nil); err != nil {
			return err
		}
	}
	return nil
}

// BackupNow writes a local backup, then sends it to Drive if connected. A
// failed upload is reported but never harms the local copy.
func (e *Engine) BackupNow(ctx context.Context, report func(string)) error {
	name, err := e.st.Backup(ctx, e.cfg.BackupKeep)
	if err != nil {
		return err
	}
	report("wrote " + filepath.Base(name))
	if err := e.st.SetKV(ctx, backupLastKey, strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		e.log.Warn("backup time not saved", "err", err)
	}
	if !e.DriveConnected(ctx) {
		return nil
	}
	report("sending to Google Drive")
	if err := e.UploadBackup(ctx, name); err != nil {
		return fmt.Errorf("saved here, but not sent to Google Drive: %w", err)
	}
	report("saved here and in Google Drive: " + filepath.Base(name))
	return nil
}
