package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
)

func (r *openClawBillingRepository) ManagedGatewayAccount(ctx context.Context, userID int64) (*service.OpenClawBillingAccount, error) {
	a := &service.OpenClawBillingAccount{}
	err := r.db.QueryRowContext(ctx, `SELECT a.billing_account_id,a.billing_user_id,u.balance,COALESCE(u.frozen_balance,0),a.initial_grant_usd FROM openclaw_billing_accounts a JOIN users u ON u.id=a.billing_user_id WHERE a.billing_user_id=$1`, userID).Scan(&a.BillingAccountID, &a.BillingUserID, &a.Balance, &a.FrozenBalance, &a.InitialGrantUSD)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

func (r *openClawBillingRepository) BeginGatewayRequest(ctx context.Context, request *service.OpenClawGatewayRequest) error {
	return r.taskTransaction(ctx, func(tx *sql.Tx) error {
		var accountID string
		if err := tx.QueryRowContext(ctx, `SELECT billing_account_id FROM openclaw_billing_accounts WHERE billing_user_id=$1 AND gateway_key_id=$2`, request.UserID, request.APIKeyID).Scan(&accountID); err != nil {
			return service.ErrOpenClawAccountNotFound
		}
		a, err := managedAccount(ctx, tx, accountID, true)
		if err != nil {
			return err
		}
		if err = rejectUnsettledAccount(ctx, tx, a.BillingUserID); err != nil {
			return err
		}
		var allowed bool
		var remaining decimal.Decimal
		err = tx.QueryRowContext(ctx, `SELECT (allowed_models ? $5),reserved_usd-captured_usd-released_usd FROM openclaw_task_budget_leases WHERE lease_id=$1 AND billing_account_id=$2 AND gateway_api_key_id=$3 AND billing_user_id=$4 AND status='active' AND expires_at>NOW() FOR UPDATE`, request.LeaseID, accountID, request.APIKeyID, request.UserID, request.Model).Scan(&allowed, &remaining)
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrOpenClawLeaseExpired
		}
		if err != nil {
			return err
		}
		if !allowed {
			return service.ErrOpenClawBillingInvalid
		}
		var subscription bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM api_keys k JOIN groups g ON g.id=k.group_id JOIN user_subscriptions s ON s.group_id=g.id AND s.user_id=k.user_id WHERE k.id=$1 AND g.subscription_type='subscription' AND s.status='active' AND s.expires_at>NOW() AND s.deleted_at IS NULL)`, request.APIKeyID).Scan(&subscription); err != nil {
			return err
		}
		if !subscription && !remaining.Add(a.Balance).IsPositive() {
			return service.ErrOpenClawInsufficientBalance
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO openclaw_gateway_requests(request_id,api_key_id,lease_id,billing_user_id,model,status) VALUES($1,$2,$3,$4,$5,'inflight')`, request.RequestID, request.APIKeyID, request.LeaseID, request.UserID, request.Model)
		return err
	})
}

func (r *openClawBillingRepository) FinishGatewayRequest(ctx context.Context, request *service.OpenClawGatewayRequest, unknown bool) error {
	return r.taskTransaction(ctx, func(tx *sql.Tx) error {
		var accountID string
		if err := tx.QueryRowContext(ctx, `SELECT billing_account_id FROM openclaw_task_budget_leases WHERE lease_id=$1`, request.LeaseID).Scan(&accountID); err != nil {
			return err
		}
		if _, err := managedAccount(ctx, tx, accountID, true); err != nil {
			return err
		}
		var err error
		if unknown {
			_, err = tx.ExecContext(ctx, `UPDATE openclaw_gateway_requests SET status='unsettled' WHERE request_id=$1 AND api_key_id=$2 AND status='inflight'`, request.RequestID, request.APIKeyID)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE openclaw_gateway_requests SET status='settled',actual_cost_usd=0,funding_source='none',settled_at=NOW() WHERE request_id=$1 AND api_key_id=$2 AND status='inflight'`, request.RequestID, request.APIKeyID)
		}
		if err != nil {
			return err
		}
		return finishClosingManagedSession(ctx, tx, accountID, request.LeaseID)
	})
}

func finishClosingManagedSession(ctx context.Context, tx *sql.Tx, accountID, leaseID string) error {
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM openclaw_task_budget_leases WHERE lease_id=$1`, leaseID).Scan(&status); err != nil {
		return err
	}
	if status == "closing" || status == "unsettled" {
		_, err := closeManagedSession(ctx, tx, accountID, leaseID)
		return err
	}
	return nil
}

// Runs after the native request/key dedup claim and inside its transaction.
// Monetary capture replaces the wallet branch; native subscription and provider
// quota effects remain in the same transaction.
func applyOpenClawGatewayUsage(ctx context.Context, tx *sql.Tx, cmd *service.UsageBillingCommand, result *service.UsageBillingApplyResult) error {
	var accountID string
	if err := tx.QueryRowContext(ctx, `SELECT billing_account_id FROM openclaw_billing_accounts WHERE billing_user_id=$1 AND gateway_key_id=$2`, cmd.UserID, cmd.APIKeyID).Scan(&accountID); err != nil {
		return service.ErrOpenClawAccountNotFound
	}
	a, err := managedAccount(ctx, tx, accountID, true)
	if err != nil {
		return err
	}
	var leaseID, state string
	err = tx.QueryRowContext(ctx, `SELECT lease_id,status FROM openclaw_gateway_requests WHERE request_id=$1 AND api_key_id=$2 AND billing_user_id=$3 FOR UPDATE`, cmd.OpenClawRequestID, cmd.APIKeyID, cmd.UserID).Scan(&leaseID, &state)
	if err != nil {
		return err
	}
	if leaseID != cmd.OpenClawLeaseID || state == "settled" {
		return service.ErrOpenClawIdempotencyConflict
	}
	amount := decimal.NewFromFloat(cmd.BalanceCost)
	funding := "wallet"
	if cmd.SubscriptionID != nil && cmd.BillingType == service.BillingTypeSubscription {
		amount = decimal.NewFromFloat(cmd.SubscriptionCost)
		funding = "subscription"
	}
	status := "unsettled"
	var actual any
	if cmd.OpenClawPricingKnown {
		actual = amount
		settled := true
		if funding == "wallet" {
			settled, err = settleManagedWallet(ctx, tx, a, leaseID, amount)
			if err != nil {
				return err
			}
		}
		if settled {
			status = "settled"
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE openclaw_gateway_requests SET model=$3,input_tokens=$4,output_tokens=$5,cache_read_tokens=$6,cache_creation_tokens=$7,actual_cost_usd=$8,funding_source=$9,status=$10::text,settled_at=CASE WHEN $10::text='settled' THEN NOW() ELSE NULL END WHERE request_id=$1 AND api_key_id=$2`, cmd.OpenClawRequestID, cmd.APIKeyID, cmd.Model, cmd.InputTokens, cmd.OutputTokens, cmd.CacheReadTokens, cmd.CacheCreationTokens, actual, funding, status)
	if err != nil {
		return err
	}
	result.OpenClawUnsettled = status != "settled"
	if err = finishClosingManagedSession(ctx, tx, accountID, leaseID); err != nil {
		return err
	}
	a, err = managedAccount(ctx, tx, accountID, false)
	if err != nil {
		return err
	}
	balance, _ := a.Balance.Float64()
	result.NewBalance = &balance
	return nil
}

func settleManagedWallet(ctx context.Context, tx *sql.Tx, account *service.OpenClawBillingAccount, leaseID string, amount decimal.Decimal) (bool, error) {
	var remaining decimal.Decimal
	if err := tx.QueryRowContext(ctx, `SELECT reserved_usd-captured_usd-released_usd FROM openclaw_task_budget_leases WHERE lease_id=$1 AND billing_account_id=$2 AND gateway_api_key_id IS NOT NULL FOR UPDATE`, leaseID, account.BillingAccountID).Scan(&remaining); err != nil {
		return false, err
	}
	fromFrozen := decimal.Min(remaining, amount)
	extra := amount.Sub(fromFrozen)
	if account.Balance.LessThan(extra) {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET balance=balance-$2,frozen_balance=frozen_balance-$3,updated_at=NOW() WHERE id=$1`, account.BillingUserID, extra, fromFrozen); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE openclaw_task_budget_leases SET reserved_usd=reserved_usd+$2,captured_usd=captured_usd+$3,updated_at=NOW() WHERE lease_id=$1`, leaseID, extra, amount); err != nil {
		return false, err
	}
	account.Balance = account.Balance.Sub(extra)
	account.FrozenBalance = account.FrozenBalance.Sub(fromFrozen)
	return true, nil
}

func settleKnownOpenClawDebts(ctx context.Context, tx *sql.Tx, account *service.OpenClawBillingAccount) error {
	rows, err := tx.QueryContext(ctx, `SELECT request_id,api_key_id,lease_id,actual_cost_usd FROM openclaw_gateway_requests WHERE billing_user_id=$1 AND status='unsettled' AND actual_cost_usd IS NOT NULL AND funding_source='wallet' ORDER BY created_at,request_id FOR UPDATE`, account.BillingUserID)
	if err != nil {
		return err
	}
	type debt struct {
		request string
		key     int64
		lease   string
		cost    decimal.Decimal
	}
	debts := []debt{}
	for rows.Next() {
		var d debt
		if err = rows.Scan(&d.request, &d.key, &d.lease, &d.cost); err != nil {
			rows.Close()
			return err
		}
		debts = append(debts, d)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, d := range debts {
		settled, err := settleManagedWallet(ctx, tx, account, d.lease, d.cost)
		if err != nil {
			return err
		}
		if !settled {
			break
		}
		if _, err = tx.ExecContext(ctx, `UPDATE openclaw_gateway_requests SET status='settled',settled_at=NOW() WHERE request_id=$1 AND api_key_id=$2`, d.request, d.key); err != nil {
			return err
		}
		if err = finishClosingManagedSession(ctx, tx, account.BillingAccountID, d.lease); err != nil {
			return err
		}
		updated, err := managedAccount(ctx, tx, account.BillingAccountID, false)
		if err != nil {
			return err
		}
		*account = *updated
	}
	return nil
}

func (r *openClawBillingRepository) Transactions(ctx context.Context, accountID string) ([]map[string]any, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT r.request_id,l.task_id,r.model,r.input_tokens,r.output_tokens,r.cache_read_tokens,r.cache_creation_tokens,r.actual_cost_usd,r.funding_source,r.status,r.created_at FROM openclaw_gateway_requests r JOIN openclaw_task_budget_leases l ON l.lease_id=r.lease_id WHERE l.billing_account_id=$1 ORDER BY r.created_at DESC LIMIT 100`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var request, task, model, status string
		var input, output, cache, creation sql.NullInt64
		var amount decimal.NullDecimal
		var funding sql.NullString
		var created time.Time
		if err = rows.Scan(&request, &task, &model, &input, &output, &cache, &creation, &amount, &funding, &status, &created); err != nil {
			return nil, err
		}
		entry := map[string]any{"requestId": request, "taskId": task, "model": nil, "inputTokens": nullableOpenClawCounter(input), "outputTokens": nullableOpenClawCounter(output), "cachedInputTokens": nullableOpenClawCounter(cache), "cacheReadTokens": nullableOpenClawCounter(cache), "cacheCreationTokens": nullableOpenClawCounter(creation), "chargedUsd": nil, "actualCostUsd": nil, "fundingSource": nil, "status": status, "createdAt": created}
		if model != "" {
			entry["model"] = model
		}
		if amount.Valid {
			entry["actualCostUsd"] = service.OpenClawMoneyFromDecimal(amount.Decimal)
			if status == "settled" {
				entry["chargedUsd"] = service.OpenClawMoneyFromDecimal(amount.Decimal)
			}
		}
		if funding.Valid && funding.String != "none" {
			entry["fundingSource"] = funding.String
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

func nullableOpenClawCounter(value sql.NullInt64) any {
	if value.Valid {
		return value.Int64
	}
	return nil
}
