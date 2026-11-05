package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// HashSecret is how session ids, recovery codes and API tokens are stored.
func HashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ---------- sessions ----------

type AuthSession struct {
	IDHash    string
	User      string
	MFADone   bool
	CreatedAt int64
	SeenAt    int64
	ExpiresAt int64
	IP        string
	Agent     string
}

func (s *Store) CreateAuthSession(ctx context.Context, idHash, user string, mfaDone bool, ttl time.Duration, ip, agent string) error {
	now := time.Now()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO auth_sessions(id_hash,user,mfa_done,created_at,seen_at,expires_at,ip,agent)
		VALUES(?,?,?,?,?,?,?,?)`, idHash, user, mfaDone, now.Unix(), now.Unix(), now.Add(ttl).Unix(), ns(ip), ns(agent))
	return err
}

func (s *Store) GetAuthSession(ctx context.Context, idHash string) (*AuthSession, error) {
	var a AuthSession
	err := s.DB.QueryRowContext(ctx, `SELECT id_hash,user,mfa_done,created_at,seen_at,expires_at,COALESCE(ip,''),COALESCE(agent,'')
		FROM auth_sessions WHERE id_hash=? AND expires_at > ?`, idHash, time.Now().Unix()).
		Scan(&a.IDHash, &a.User, &a.MFADone, &a.CreatedAt, &a.SeenAt, &a.ExpiresAt, &a.IP, &a.Agent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

// TouchSession records activity and slides the expiry forward.
func (s *Store) TouchAuthSession(ctx context.Context, idHash string, ttl time.Duration) error {
	now := time.Now()
	_, err := s.DB.ExecContext(ctx, `UPDATE auth_sessions SET seen_at=?, expires_at=? WHERE id_hash=?`,
		now.Unix(), now.Add(ttl).Unix(), idHash)
	return err
}

func (s *Store) DeleteAuthSession(ctx context.Context, idHash string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM auth_sessions WHERE id_hash=?`, idHash)
	return err
}

// DeleteSessionsExcept signs out every other session of a user (all of them
// when keep is empty).
func (s *Store) DeleteAuthSessionsExcept(ctx context.Context, user, keep string) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user=? AND id_hash != ?`, user, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) ListAuthSessions(ctx context.Context, user string) ([]AuthSession, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id_hash,user,mfa_done,created_at,seen_at,expires_at,COALESCE(ip,''),COALESCE(agent,'')
		FROM auth_sessions WHERE user=? AND mfa_done=1 AND expires_at > ? ORDER BY seen_at DESC`, user, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuthSession
	for rows.Next() {
		var a AuthSession
		if err := rows.Scan(&a.IDHash, &a.User, &a.MFADone, &a.CreatedAt, &a.SeenAt, &a.ExpiresAt, &a.IP, &a.Agent); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) PruneAuthSessions(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM auth_sessions WHERE expires_at <= ?`, time.Now().Unix())
	return err
}

// ---------- TOTP ----------

type TOTP struct {
	Secret   string
	Enabled  bool
	LastStep int64
}

func (s *Store) GetTOTP(ctx context.Context, user string) (*TOTP, error) {
	var t TOTP
	err := s.DB.QueryRowContext(ctx, `SELECT secret, enabled, last_step FROM auth_totp WHERE user=?`, user).
		Scan(&t.Secret, &t.Enabled, &t.LastStep)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

// SetPendingTOTP stores a new, not yet confirmed secret (replacing any
// earlier unconfirmed one; an enabled one is left alone).
func (s *Store) SetPendingTOTP(ctx context.Context, user, secret string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO auth_totp(user,secret,enabled,last_step,created_at) VALUES(?,?,0,0,?)
		ON CONFLICT(user) DO UPDATE SET secret=excluded.secret, last_step=0, created_at=excluded.created_at
		WHERE auth_totp.enabled=0`, user, secret, time.Now().Unix())
	return err
}

func (s *Store) EnableTOTP(ctx context.Context, user string, step int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE auth_totp SET enabled=1, last_step=? WHERE user=?`, step, user)
	return err
}

// UseTOTPStep records the time step of an accepted code, refusing a step
// that was already used (so a code can't be replayed).
func (s *Store) UseTOTPStep(ctx context.Context, user string, step int64) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE auth_totp SET last_step=? WHERE user=? AND last_step < ?`, step, user, step)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) DeleteTOTP(ctx context.Context, user string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM auth_totp WHERE user=?`, user)
	return err
}

// ---------- recovery codes ----------

func (s *Store) SetRecoveryCodes(ctx context.Context, user string, hashes []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_recovery WHERE user=?`, user); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_recovery(user,code_hash) VALUES(?,?)`, user, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UseRecoveryCode consumes one unused code.
func (s *Store) UseRecoveryCode(ctx context.Context, user, hash string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE auth_recovery SET used_at=? WHERE user=? AND code_hash=? AND used_at IS NULL`,
		time.Now().Unix(), user, hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) RecoveryCodesLeft(ctx context.Context, user string) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_recovery WHERE user=? AND used_at IS NULL`, user).Scan(&n)
	return n, err
}

// ---------- passkeys ----------

type Passkey struct {
	ID         string // base64url credential id
	User       string
	Name       string
	RPID       string
	Credential []byte // go-webauthn Credential as JSON
	CreatedAt  int64
	LastUsed   int64
}

func (s *Store) AddPasskey(ctx context.Context, p Passkey) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO auth_passkeys(id,user,name,rp_id,credential,created_at) VALUES(?,?,?,?,?,?)`,
		p.ID, p.User, p.Name, p.RPID, string(p.Credential), time.Now().Unix())
	return err
}

func (s *Store) Passkeys(ctx context.Context, user string) ([]Passkey, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,user,name,rp_id,credential,created_at,COALESCE(last_used,0)
		FROM auth_passkeys WHERE (?1='' OR user=?1) ORDER BY created_at`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		var p Passkey
		var cred string
		if err := rows.Scan(&p.ID, &p.User, &p.Name, &p.RPID, &cred, &p.CreatedAt, &p.LastUsed); err != nil {
			return nil, err
		}
		p.Credential = []byte(cred)
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePasskeyUse stores the credential after a login (new signature
// counter and flags) and the time it was used.
func (s *Store) UpdatePasskeyUse(ctx context.Context, id string, credential []byte) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE auth_passkeys SET credential=?, last_used=? WHERE id=?`,
		string(credential), time.Now().Unix(), id)
	return err
}

func (s *Store) DeletePasskey(ctx context.Context, user, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM auth_passkeys WHERE user=? AND id=?`, user, id)
	return err
}

// ---------- API tokens ----------

type APIToken struct {
	Hash      string
	User      string
	Name      string
	CreatedAt int64
	LastUsed  int64
}

func (s *Store) AddAPIToken(ctx context.Context, hash, user, name string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO auth_tokens(hash,user,name,created_at) VALUES(?,?,?,?)`,
		hash, user, name, time.Now().Unix())
	return err
}

// CheckAPIToken returns the token's user, or "" if it doesn't exist.
func (s *Store) CheckAPIToken(ctx context.Context, hash string) (string, error) {
	var user string
	var last int64
	err := s.DB.QueryRowContext(ctx, `SELECT user, COALESCE(last_used,0) FROM auth_tokens WHERE hash=?`, hash).Scan(&user, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err == nil && time.Now().Unix()-last > 300 {
		_, _ = s.DB.ExecContext(ctx, `UPDATE auth_tokens SET last_used=? WHERE hash=?`, time.Now().Unix(), hash)
	}
	return user, err
}

func (s *Store) APITokens(ctx context.Context, user string) ([]APIToken, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT hash,user,name,created_at,COALESCE(last_used,0) FROM auth_tokens WHERE user=? ORDER BY created_at`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.Hash, &t.User, &t.Name, &t.CreatedAt, &t.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAPIToken(ctx context.Context, user, hash string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM auth_tokens WHERE user=? AND hash=?`, user, hash)
	return err
}

// ResetMFA removes every second factor and session of a user: the escape
// hatch when an authenticator and all recovery codes are lost (AUTH_RESET).
func (s *Store) ResetMFA(ctx context.Context, user string) error {
	for _, q := range []string{`DELETE FROM auth_totp WHERE user=?`, `DELETE FROM auth_recovery WHERE user=?`,
		`DELETE FROM auth_passkeys WHERE user=?`, `DELETE FROM auth_sessions WHERE user=?`} {
		if _, err := s.DB.ExecContext(ctx, q, user); err != nil {
			return err
		}
	}
	return nil
}
