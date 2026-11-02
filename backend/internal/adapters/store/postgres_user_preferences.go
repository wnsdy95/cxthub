//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.UserPreferencesStore = (*PostgresStore)(nil)

func (storage *PostgresStore) UpdateUserPreferences(ctx context.Context, userID string, patch domain.UserPreferencesPatch) (domain.User, error) {
	if err := domain.ValidateExternalID(userID); err != nil {
		return domain.User{}, err
	}
	if prior, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx); ok {
		if prior.owner != storage || prior.readOnly || prior.repo != "" || prior.repository != "" || (!prior.identity && prior.profileUser != userID) {
			return domain.User{}, domain.ErrConflict
		}
		return storage.updateUserPreferences(ctx, userID, patch)
	}
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return domain.User{}, err
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended('cxt-identity-access',0))`); err != nil {
		return domain.User{}, err
	}
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage, profileUser: userID})
	updated, err := storage.updateUserPreferences(bound, userID, patch)
	if err != nil {
		return domain.User{}, err
	}
	if err := mapPGConstraint(transaction.Commit(ctx)); err != nil {
		return domain.User{}, err
	}
	return updated, nil
}

func (storage *PostgresStore) updateUserPreferences(ctx context.Context, userID string, patch domain.UserPreferencesPatch) (domain.User, error) {
	var user domain.User
	err := storage.db(ctx).QueryRow(ctx, `SELECT id, email, name, COALESCE(username,''), COALESCE(nickname,''), COALESCE(load_mode,''), COALESCE(avatar,''), COALESCE(locale,''), created_at FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID).
		Scan(&user.ID, &user.Email, &user.Name, &user.Username, &user.Nickname, &user.LoadMode, &user.Avatar, &user.Locale, &user.CreatedAt)
	if err != nil {
		return domain.User{}, mapNoRows(err)
	}
	if user.ID != userID {
		return domain.User{}, domain.ErrNotFound
	}
	if err := domain.ValidateUserRecord(user); err != nil {
		return domain.User{}, storedIdentityIntegrity(err)
	}
	updated, err := domain.ApplyUserPreferences(user, patch)
	if err != nil {
		return domain.User{}, err
	}
	result, err := storage.db(ctx).Exec(ctx, `UPDATE users SET nickname=$2, load_mode=$3, avatar=$4, locale=$5 WHERE id=$1`, userID, updated.Nickname, updated.LoadMode, updated.Avatar, updated.Locale)
	if err != nil {
		return domain.User{}, mapPGConstraint(err)
	}
	if result.RowsAffected() != 1 {
		return domain.User{}, domain.ErrNotFound
	}
	return updated, nil
}
