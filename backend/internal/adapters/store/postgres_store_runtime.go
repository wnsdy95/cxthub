//go:build postgres

package store

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"time"
)

func (s *PostgresStore) CreateDevicePairing(ctx context.Context, p domain.DevicePairing) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO device_pairings(code,poll_hash,label,expires_at) VALUES($1,$2,$3,$4) ON CONFLICT(code) DO UPDATE SET poll_hash=EXCLUDED.poll_hash,user_id='',label=EXCLUDED.label,expires_at=EXCLUDED.expires_at WHERE device_pairings.expires_at<=clock_timestamp()`, p.Code, p.PollHash, p.Label, p.ExpiresAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}
func (s *PostgresStore) GetDevicePairing(ctx context.Context, code, poll string, now time.Time) (domain.DevicePairing, error) {
	var p domain.DevicePairing
	err := s.pool.QueryRow(ctx, `SELECT code,poll_hash,user_id,label,expires_at FROM device_pairings WHERE code=$1 AND poll_hash=$2 AND expires_at>$3`, code, poll, now).Scan(&p.Code, &p.PollHash, &p.UserID, &p.Label, &p.ExpiresAt)
	return p, mapNoRows(err)
}
func (s *PostgresStore) ApproveDevicePairing(ctx context.Context, code, user string, now time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE device_pairings SET user_id=$2 WHERE code=$1 AND expires_at>$3 AND (user_id='' OR user_id=$2)`, code, user, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}
func (s *PostgresStore) RedeemDevicePairing(ctx context.Context, code, poll string, sess domain.Session, now time.Time) error {
	if err := domain.ValidateSessionRecord(sess); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `DELETE FROM device_pairings WHERE code=$1 AND poll_hash=$2 AND user_id=$3 AND user_id<>'' AND expires_at>$4`, code, poll, sess.UserID, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	_, err = tx.Exec(ctx, `INSERT INTO sessions(token,user_id,expires_at,hint,kind,label) VALUES($1,$2,$3,$4,$5,$6)`, sess.Token, sess.UserID, sess.ExpiresAt, sess.Hint, sess.Kind, sess.Label)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *PostgresStore) AllowRequest(ctx context.Context, key string, limit int, window time.Duration, now time.Time) (bool, error) {
	if limit < 1 || window <= 0 || window/time.Duration(limit) < time.Microsecond {
		return false, domain.ErrValidation
	}
	var clock any
	if !now.IsZero() {
		clock = now
	}
	// One row update is the shared atomic admission point. Use the database clock
	// in production so clocks on different instances cannot create extra allowance.
	tag, err := s.pool.Exec(ctx, `WITH config AS (SELECT COALESCE($4::timestamptz,clock_timestamp()) AS now,$2::double precision*interval '1 microsecond' AS step,$3::double precision*interval '1 microsecond' AS quota_window)
 INSERT INTO request_allowances(key,available_at,expires_at) SELECT $1,now+step,now+quota_window FROM config
 ON CONFLICT(key) DO UPDATE SET available_at=greatest(request_allowances.available_at,(SELECT now FROM config))+(SELECT step FROM config), expires_at=(SELECT now+quota_window FROM config)
 WHERE request_allowances.available_at<=(SELECT now+quota_window-step FROM config)`, key, (window / time.Duration(limit)).Microseconds(), window.Microseconds(), clock)
	return tag.RowsAffected() == 1, err
}
func (s *PostgresStore) PruneRuntimeState(ctx context.Context, now time.Time) error {
	for _, q := range []string{`DELETE FROM device_pairings WHERE code IN (SELECT code FROM device_pairings WHERE expires_at<=$1 LIMIT 1000)`, `DELETE FROM request_allowances WHERE key IN (SELECT key FROM request_allowances WHERE expires_at<=$1 LIMIT 1000)`} {
		if _, err := s.pool.Exec(ctx, q, now); err != nil {
			return err
		}
	}
	return nil
}
