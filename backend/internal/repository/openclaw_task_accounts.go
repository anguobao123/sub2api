package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
)

func (r *openClawBillingRepository) AdjustBalance(ctx context.Context, account service.OpenClawBillingAccount, operation string, amount decimal.Decimal, key, reason string) (*service.OpenClawBillingAccount, error) {
	var result *service.OpenClawBillingAccount
	err := r.taskTransaction(ctx, func(tx *sql.Tx) error {
		a, err := managedAccount(ctx, tx, account.BillingAccountID, true)
		if err != nil {
			return err
		}
		var oldOperation, oldReason string
		var oldAmount decimal.Decimal
		err = tx.QueryRowContext(ctx, `SELECT operation,amount_usd,reason FROM openclaw_billing_adjustments WHERE billing_account_id=$1 AND idempotency_key=$2`, account.BillingAccountID, key).Scan(&oldOperation, &oldAmount, &oldReason)
		if err == nil {
			if oldOperation != operation || !oldAmount.Equal(amount) || oldReason != reason {
				return service.ErrOpenClawIdempotencyConflict
			}
			result = a
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if operation == "set" {
			a.Balance = amount
		} else {
			a.Balance = a.Balance.Add(amount)
		}
		if _, err = service.NormalizeOpenClawUSDAmount(a.Balance, true); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE users SET balance=$2,updated_at=NOW() WHERE id=$1`, a.BillingUserID, a.Balance); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO openclaw_billing_adjustments(billing_account_id,idempotency_key,operation,amount_usd,reason) VALUES($1,$2,$3,$4,$5)`, a.BillingAccountID, key, operation, amount, reason); err != nil {
			return err
		}
		if err = settleKnownOpenClawDebts(ctx, tx, a); err != nil {
			return err
		}
		result, err = managedAccount(ctx, tx, a.BillingAccountID, false)
		return err
	})
	return result, err
}

func (r *openClawBillingRepository) FindSubscriptionAssignment(ctx context.Context, account, key string, group int64, days int, reason string) (*int64, error) {
	var oldGroup, id int64
	var oldDays int
	var oldReason string
	err := r.db.QueryRowContext(ctx, `SELECT group_id,days,reason,subscription_id FROM openclaw_billing_subscription_assignments WHERE billing_account_id=$1 AND idempotency_key=$2`, account, key).Scan(&oldGroup, &oldDays, &oldReason, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if oldGroup != group || oldDays != days || oldReason != reason {
		return nil, service.ErrOpenClawIdempotencyConflict
	}
	return &id, nil
}

func (r *openClawBillingRepository) RecordSubscriptionAssignment(ctx context.Context, account, key string, group int64, days int, reason string, id int64) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO openclaw_billing_subscription_assignments(billing_account_id,idempotency_key,group_id,days,reason,subscription_id) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(billing_account_id,idempotency_key) DO NOTHING`, account, key, group, days, reason, id)
	return err
}
