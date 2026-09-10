package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
)

func NewOpenClawTaskSessionRepository(_ *dbent.Client, db *sql.DB) service.OpenClawTaskSessionRepository {
	return &openClawBillingRepository{db: db}
}

func (r *openClawBillingRepository) taskTransaction(ctx context.Context, work func(*sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = work(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func managedAccount(ctx context.Context, tx *sql.Tx, accountID string, lock bool) (*service.OpenClawBillingAccount, error) {
	query := `SELECT a.billing_account_id,a.billing_user_id,u.balance,COALESCE(u.frozen_balance,0),a.initial_grant_usd FROM openclaw_billing_accounts a JOIN users u ON u.id=a.billing_user_id WHERE a.billing_account_id=$1 AND u.deleted_at IS NULL`
	if lock {
		query += ` FOR UPDATE OF u,a`
	}
	a := &service.OpenClawBillingAccount{}
	err := tx.QueryRowContext(ctx, query, accountID).Scan(&a.BillingAccountID, &a.BillingUserID, &a.Balance, &a.FrozenBalance, &a.InitialGrantUSD)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrOpenClawAccountNotFound
	}
	return a, err
}

func (r *openClawBillingRepository) EnsureGatewayKey(ctx context.Context, account service.OpenClawBillingAccount, proposed string, groupID int64) (int64, string, error) {
	var keyID int64
	var key string
	err := r.taskTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := managedAccount(ctx, tx, account.BillingAccountID, true); err != nil {
			return err
		}
		var existing sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT gateway_key_id FROM openclaw_billing_accounts WHERE billing_account_id=$1`, account.BillingAccountID).Scan(&existing); err != nil {
			return err
		}
		if existing.Valid {
			keyID = existing.Int64
			var active bool
			if err := tx.QueryRowContext(ctx, `SELECT status='active' FROM users WHERE id=$1`, account.BillingUserID).Scan(&active); err != nil {
				return err
			}
			if !active {
				return service.ErrOpenClawAccountNotFound
			}
			if err := tx.QueryRowContext(ctx, `SELECT key FROM api_keys WHERE id=$1 AND user_id=$2 AND status='active' AND deleted_at IS NULL AND (expires_at IS NULL OR expires_at>NOW())`, keyID, account.BillingUserID).Scan(&key); err != nil {
				return err
			}
		} else {
			if err := tx.QueryRowContext(ctx, `INSERT INTO api_keys(user_id,key,name,group_id,status) VALUES($1,$2,'openclaw-task-gateway',$3,'active') RETURNING id`, account.BillingUserID, proposed, groupID).Scan(&keyID); err != nil {
				return err
			}
			key = proposed
			if _, err := tx.ExecContext(ctx, `UPDATE openclaw_billing_accounts SET gateway_key_id=$2 WHERE billing_account_id=$1`, account.BillingAccountID, keyID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE users SET status='active',concurrency=0,updated_at=NOW() WHERE id=$1`, account.BillingUserID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET group_id=$2,updated_at=NOW() WHERE id=$1`, keyID, groupID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_allowed_groups(user_id,group_id) VALUES($1,$2) ON CONFLICT(user_id,group_id) DO NOTHING`, account.BillingUserID, groupID); err != nil {
			return err
		}
		return nil
	})
	return keyID, key, err
}

func (r *openClawBillingRepository) GatewayGroup(ctx context.Context, accountID string) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `SELECT COALESCE(gateway_group_id,0) FROM openclaw_billing_accounts WHERE billing_account_id=$1`, accountID).Scan(&id)
	return id, err
}

func (r *openClawBillingRepository) SetGatewayGroup(ctx context.Context, accountID string, groupID int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE openclaw_billing_accounts SET gateway_group_id=$2,updated_at=NOW() WHERE billing_account_id=$1`, accountID, groupID)
	return err
}

func (r *openClawBillingRepository) OpenTaskSession(ctx context.Context, c service.OpenClawTaskOpenCommand) (*service.OpenClawTaskOpenResult, error) {
	var result *service.OpenClawTaskOpenResult
	err := r.taskTransaction(ctx, func(tx *sql.Tx) error {
		if err := lockOpenClawKey(ctx, tx, "managed-task\x1f"+c.PlatformUserHMAC+"\x1f"+c.Identity.TaskID); err != nil {
			return err
		}
		var closed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM openclaw_task_session_closures WHERE platform_user_hmac=$1 AND task_id=$2)`, c.PlatformUserHMAC, c.Identity.TaskID).Scan(&closed); err != nil {
			return err
		}
		if closed {
			return service.ErrOpenClawLeaseFinalized
		}
		a, err := managedAccount(ctx, tx, c.Account.BillingAccountID, true)
		if err != nil {
			return err
		}
		var id, session, node, status string
		var expires time.Time
		var models []byte
		err = tx.QueryRowContext(ctx, `SELECT lease_id,session_id,node_id,status,expires_at,allowed_models FROM openclaw_task_budget_leases WHERE billing_account_id=$1 AND task_id=$2 AND gateway_api_key_id IS NOT NULL`, a.BillingAccountID, c.Identity.TaskID).Scan(&id, &session, &node, &status, &expires, &models)
		if err == nil {
			var old []string
			if json.Unmarshal(models, &old) != nil {
				return service.ErrOpenClawBillingInvalid
			}
			nowModels, _ := json.Marshal(c.AllowedModels)
			oldModels, _ := json.Marshal(old)
			if session != c.Identity.SessionID || node != c.Identity.NodeID || string(nowModels) != string(oldModels) {
				return service.ErrOpenClawIdempotencyConflict
			}
			if status != "active" || !expires.After(time.Now()) {
				return service.ErrOpenClawLeaseFinalized
			}
			result = &service.OpenClawTaskOpenResult{LeaseID: id, ExpiresAt: expires, Account: service.OpenClawAccountViewOf(a)}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err = rejectUnsettledAccount(ctx, tx, a.BillingUserID); err != nil {
			return err
		}
		reserve := decimal.Zero
		if !c.Subscription {
			if !a.Balance.IsPositive() {
				return service.ErrOpenClawInsufficientBalance
			}
			reserve = decimal.Min(decimal.NewFromInt(1), a.Balance)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE users SET balance=balance-$2,frozen_balance=COALESCE(frozen_balance,0)+$2,updated_at=NOW() WHERE id=$1`, a.BillingUserID, reserve); err != nil {
			return err
		}
		models, err = json.Marshal(c.AllowedModels)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO openclaw_task_budget_leases(lease_id,billing_account_id,billing_user_id,task_id,session_id,node_id,reserved_usd,reserve_idempotency_key,reserve_request_fingerprint,expires_at,gateway_api_key_id,allowed_models) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$8,$9,$10,$11)`, c.LeaseID, a.BillingAccountID, a.BillingUserID, c.Identity.TaskID, c.Identity.SessionID, c.Identity.NodeID, reserve, "task-open:"+c.Identity.TaskID, c.ExpiresAt, c.APIKeyID, models)
		if err != nil {
			return err
		}
		a.Balance = a.Balance.Sub(reserve)
		a.FrozenBalance = a.FrozenBalance.Add(reserve)
		result = &service.OpenClawTaskOpenResult{LeaseID: c.LeaseID, ExpiresAt: c.ExpiresAt, Account: service.OpenClawAccountViewOf(a)}
		return nil
	})
	return result, err
}

func rejectUnsettledAccount(ctx context.Context, tx *sql.Tx, userID int64) error {
	var blocked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM openclaw_gateway_requests WHERE billing_user_id=$1 AND status='unsettled')`, userID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return service.ErrOpenClawUnsettled
	}
	return nil
}

func (r *openClawBillingRepository) RenewTaskSession(ctx context.Context, accountID, leaseID, taskID, nodeID string, expires time.Time) (time.Time, error) {
	var result time.Time
	err := r.taskTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := managedAccount(ctx, tx, accountID, true); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `UPDATE openclaw_task_budget_leases SET expires_at=CASE WHEN status='active' THEN $5 ELSE expires_at END,updated_at=NOW() WHERE billing_account_id=$1 AND lease_id=$2 AND task_id=$3 AND node_id=$4 AND gateway_api_key_id IS NOT NULL AND ((status='active' AND expires_at>NOW()) OR status IN ('closing','closed','unsettled')) RETURNING expires_at`, accountID, leaseID, taskID, nodeID, expires).Scan(&result)
	})
	if errors.Is(err, sql.ErrNoRows) {
		err = service.ErrOpenClawLeaseExpired
	}
	return result, err
}

func closeManagedSession(ctx context.Context, tx *sql.Tx, accountID, leaseID string) (*service.OpenClawTaskCloseResult, error) {
	var userID int64
	var status string
	var reserved, captured, released decimal.Decimal
	err := tx.QueryRowContext(ctx, `SELECT billing_user_id,status,reserved_usd,captured_usd,released_usd FROM openclaw_task_budget_leases WHERE billing_account_id=$1 AND lease_id=$2 AND gateway_api_key_id IS NOT NULL FOR UPDATE`, accountID, leaseID).Scan(&userID, &status, &reserved, &captured, &released)
	if err != nil {
		return nil, err
	}
	var inflight, unsettled int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE status='inflight'),count(*) FILTER(WHERE status='unsettled') FROM openclaw_gateway_requests WHERE lease_id=$1`, leaseID).Scan(&inflight, &unsettled); err != nil {
		return nil, err
	}
	result := &service.OpenClawTaskCloseResult{LeaseID: &leaseID}
	if unsettled > 0 {
		result.Status = "unsettled"
	} else if inflight > 0 {
		result.Status = "closing"
	} else {
		result.Status = "closed"
		var charged decimal.Decimal
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(actual_cost_usd),0) FROM openclaw_gateway_requests WHERE lease_id=$1 AND status='settled'`, leaseID).Scan(&charged); err != nil {
			return nil, err
		}
		amount := service.OpenClawMoneyFromDecimal(charged)
		result.ChargedUSD = &amount
	}
	if result.Status == "closed" && status != "closed" {
		remaining := reserved.Sub(captured).Sub(released)
		if remaining.IsPositive() {
			if _, _, err = applyOpenClawReleaseToUser(ctx, tx, userID, remaining); err != nil {
				return nil, err
			}
			released = released.Add(remaining)
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE openclaw_task_budget_leases SET status=$2::text,released_usd=$3,finalized_at=CASE WHEN $2::text='closed' THEN NOW() ELSE NULL END,updated_at=NOW() WHERE lease_id=$1`, leaseID, result.Status, released)
	return result, err
}

func (r *openClawBillingRepository) CloseTaskSession(ctx context.Context, accountID, leaseID, taskID, nodeID, key string) (*service.OpenClawTaskCloseResult, error) {
	var result *service.OpenClawTaskCloseResult
	err := r.taskTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := managedAccount(ctx, tx, accountID, true); err != nil {
			return err
		}
		var matches bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM openclaw_task_budget_leases WHERE billing_account_id=$1 AND lease_id=$2 AND task_id=$3 AND node_id=$4 AND gateway_api_key_id IS NOT NULL)`, accountID, leaseID, taskID, nodeID).Scan(&matches); err != nil {
			return err
		}
		if !matches {
			return service.ErrOpenClawLeaseNotFound
		}
		var err error
		result, err = closeManagedSession(ctx, tx, accountID, leaseID)
		return err
	})
	return result, err
}

func (r *openClawBillingRepository) CloseTaskSessionByTask(ctx context.Context, hmac string, input service.OpenClawTaskIdentity, key string) (*service.OpenClawTaskCloseResult, error) {
	result := &service.OpenClawTaskCloseResult{Status: "closed"}
	err := r.taskTransaction(ctx, func(tx *sql.Tx) error {
		if err := lockOpenClawKey(ctx, tx, "managed-task\x1f"+hmac+"\x1f"+input.TaskID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO openclaw_task_session_closures(platform_user_hmac,task_id,session_id,node_id,idempotency_key) VALUES($1,$2,$3,$4,$5) ON CONFLICT(platform_user_hmac,task_id) DO NOTHING`, hmac, input.TaskID, input.SessionID, input.NodeID, key)
		if err != nil {
			return err
		}
		var session, node string
		if err = tx.QueryRowContext(ctx, `SELECT session_id,node_id FROM openclaw_task_session_closures WHERE platform_user_hmac=$1 AND task_id=$2`, hmac, input.TaskID).Scan(&session, &node); err != nil {
			return err
		}
		if session != input.SessionID || node != input.NodeID {
			return service.ErrOpenClawIdempotencyConflict
		}
		var accountID string
		err = tx.QueryRowContext(ctx, `SELECT billing_account_id FROM openclaw_billing_accounts WHERE platform_user_hmac=$1`, hmac).Scan(&accountID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err = managedAccount(ctx, tx, accountID, true); err != nil {
			return err
		}
		var leaseID string
		err = tx.QueryRowContext(ctx, `SELECT lease_id FROM openclaw_task_budget_leases WHERE billing_account_id=$1 AND task_id=$2 AND session_id=$3 AND node_id=$4 AND gateway_api_key_id IS NOT NULL`, accountID, input.TaskID, input.SessionID, input.NodeID).Scan(&leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		result, err = closeManagedSession(ctx, tx, accountID, leaseID)
		return err
	})
	return result, err
}

func (r *openClawBillingRepository) ExpireTaskSessions(ctx context.Context, limit int) (int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT billing_account_id,lease_id,task_id,node_id FROM openclaw_task_budget_leases WHERE gateway_api_key_id IS NOT NULL AND status='active' AND expires_at<=NOW() ORDER BY expires_at LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type expired struct{ account, lease, task, node string }
	items := []expired{}
	for rows.Next() {
		var item expired
		if err = rows.Scan(&item.account, &item.lease, &item.task, &item.node); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for i, item := range items {
		if _, err = r.CloseTaskSession(ctx, item.account, item.lease, item.task, item.node, "expiry:"+item.lease); err != nil {
			return i, err
		}
	}
	return len(items), nil
}
