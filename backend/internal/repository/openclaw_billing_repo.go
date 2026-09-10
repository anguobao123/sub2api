package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
)

type openClawBillingRepository struct {
	db *sql.DB
}

func NewOpenClawBillingRepository(_ *dbent.Client, sqlDB *sql.DB) service.OpenClawBillingRepository {
	return &openClawBillingRepository{db: sqlDB}
}

func newOpenClawBillingRepositoryWithDB(db *sql.DB) *openClawBillingRepository {
	return &openClawBillingRepository{db: db}
}

func (r *openClawBillingRepository) EnsureAccount(ctx context.Context, provision service.OpenClawAccountProvision) (_ *service.OpenClawBillingAccount, err error) {
	if r == nil || r.db == nil {
		return nil, errors.New("openclaw billing repository db is nil")
	}
	if len(strings.TrimSpace(provision.PlatformUserHMAC)) != 64 || strings.TrimSpace(provision.BillingAccountID) == "" ||
		strings.TrimSpace(provision.SyntheticEmail) == "" || strings.TrimSpace(provision.PasswordHash) == "" || provision.DefaultGrantUSD.IsNegative() {
		return nil, service.ErrOpenClawBillingInvalid
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	if err := lockOpenClawKey(ctx, tx, "identity\x1f"+provision.PlatformUserHMAC); err != nil {
		return nil, err
	}
	account, found, err := findOpenClawAccountByHMAC(ctx, tx, provision.PlatformUserHMAC, true)
	if err != nil {
		return nil, err
	}
	if found {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		tx = nil
		return account, nil
	}

	var billingUserID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO users (
			email, password_hash, role, balance, frozen_balance, concurrency,
			status, username, notes, signup_source
		)
		VALUES ($1, $2, 'user', 0, 0, 0, 'disabled', 'openclaw-internal', 'openclaw internal billing account', 'email')
		RETURNING id
	`, provision.SyntheticEmail, provision.PasswordHash).Scan(&billingUserID)
	if err != nil {
		return nil, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO openclaw_billing_accounts (
			billing_account_id, platform_user_hmac, billing_user_id, initial_grant_usd
		)
		VALUES ($1, $2, $3, $4)
	`, provision.BillingAccountID, provision.PlatformUserHMAC, billingUserID, provision.DefaultGrantUSD)
	if err != nil {
		return nil, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO openclaw_billing_grants (
			billing_account_id, billing_user_id, grant_key, amount_usd, grant_type
		)
		VALUES ($1, $2, 'initial_mapping', $3, 'initial_mapping')
	`, provision.BillingAccountID, billingUserID, provision.DefaultGrantUSD)
	if err != nil {
		return nil, err
	}

	var balance, frozenBalance decimal.Decimal
	err = tx.QueryRowContext(ctx, `
		UPDATE users
		SET balance = balance + $1,
			updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL
		RETURNING balance, frozen_balance
	`, provision.DefaultGrantUSD, billingUserID).Scan(&balance, &frozenBalance)
	if err != nil {
		return nil, err
	}

	account = &service.OpenClawBillingAccount{
		BillingAccountID: provision.BillingAccountID,
		BillingUserID:    billingUserID,
		Balance:          balance,
		FrozenBalance:    frozenBalance,
		InitialGrantUSD:  provision.DefaultGrantUSD,
		Created:          true,
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	tx = nil
	return account, nil
}

func (r *openClawBillingRepository) FindAccount(ctx context.Context, platformUserHMAC string) (_ *service.OpenClawBillingAccount, err error) {
	if r == nil || r.db == nil {
		return nil, errors.New("openclaw billing repository db is nil")
	}
	if len(strings.TrimSpace(platformUserHMAC)) != 64 {
		return nil, service.ErrOpenClawBillingInvalid
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	account, found, err := findOpenClawAccountByHMAC(ctx, tx, platformUserHMAC, false)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, service.ErrOpenClawAccountNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	tx = nil
	return account, nil
}

func (r *openClawBillingRepository) ReserveLease(ctx context.Context, command service.OpenClawReserveLeaseCommand) (_ *service.OpenClawLeaseResult, err error) {
	if r == nil || r.db == nil {
		return nil, errors.New("openclaw billing repository db is nil")
	}
	if err := validateOpenClawReserveCommand(command); err != nil {
		return nil, err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	if err := lockOpenClawKey(ctx, tx, openClawTaskLockKey(command.Account.BillingAccountID, command.TaskID, command.SessionID, command.NodeID)); err != nil {
		return nil, err
	}

	// Lock the idempotency key before creating an account-specific lease. This
	// turns concurrent retries into one winner plus one read-only replay.
	if err := lockOpenClawKey(ctx, tx, openClawReserveKeyLockKey(command.Account.BillingAccountID, command.IdempotencyKey)); err != nil {
		return nil, err
	}
	lease, fingerprint, found, err := findOpenClawLeaseByReserveKey(ctx, tx, command.Account.BillingAccountID, command.IdempotencyKey, true)
	if err != nil {
		return nil, err
	}
	if found {
		if fingerprint != command.RequestFingerprint {
			return nil, service.ErrOpenClawIdempotencyConflict
		}
		balance, frozenBalance, err := openClawBalances(ctx, tx, command.Account.BillingUserID)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		tx = nil
		return &service.OpenClawLeaseResult{
			Applied:       false,
			Lease:         *lease,
			BalanceUSD:    balance,
			FrozenBalance: frozenBalance,
		}, nil
	}

	activeLease, active, err := findActiveOpenClawLeaseForTask(ctx, tx, command.Account.BillingAccountID, command.TaskID, command.SessionID, command.NodeID, true)
	if err != nil {
		return nil, err
	}
	if active {
		if activeLease.ExpiresAt.After(time.Now().UTC()) {
			return nil, service.ErrOpenClawLeaseActive
		}
		if _, err := r.expireOpenClawLeaseLocked(ctx, tx, activeLease); err != nil {
			return nil, err
		}
	}

	var balance, frozenBalance decimal.Decimal
	err = tx.QueryRowContext(ctx, `
		UPDATE users
		SET balance = balance - $1,
			frozen_balance = COALESCE(frozen_balance, 0) + $1,
			updated_at = NOW()
		WHERE id = $2
			AND deleted_at IS NULL
			AND balance >= $1
		RETURNING balance, frozen_balance
	`, command.AmountUSD, command.Account.BillingUserID).Scan(&balance, &frozenBalance)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrOpenClawInsufficientBalance
	}
	if err != nil {
		return nil, err
	}

	lease = &service.OpenClawBudgetLease{
		LeaseID:          command.LeaseID,
		BillingAccountID: command.Account.BillingAccountID,
		BillingUserID:    command.Account.BillingUserID,
		TaskID:           command.TaskID,
		SessionID:        command.SessionID,
		NodeID:           command.NodeID,
		ReservedUSD:      command.AmountUSD,
		Status:           service.OpenClawLeaseStatusActive,
		ExpiresAt:        command.ExpiresAt.UTC(),
		CreatedAt:        time.Now().UTC(),
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO openclaw_task_budget_leases (
			lease_id, billing_account_id, billing_user_id, task_id, session_id, node_id,
			reserved_usd, reserve_idempotency_key, reserve_request_fingerprint, expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, lease.LeaseID, lease.BillingAccountID, lease.BillingUserID, lease.TaskID, lease.SessionID, lease.NodeID,
		lease.ReservedUSD, command.IdempotencyKey, command.RequestFingerprint, lease.ExpiresAt)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	tx = nil
	return &service.OpenClawLeaseResult{
		Applied:       true,
		Lease:         *lease,
		BalanceUSD:    balance,
		FrozenBalance: frozenBalance,
	}, nil
}

func (r *openClawBillingRepository) CaptureLease(ctx context.Context, command service.OpenClawLeaseOperationCommand) (*service.OpenClawLeaseResult, error) {
	return r.applyOpenClawLeaseOperation(ctx, command, "capture")
}

func (r *openClawBillingRepository) ReleaseLease(ctx context.Context, command service.OpenClawLeaseOperationCommand) (*service.OpenClawLeaseResult, error) {
	return r.applyOpenClawLeaseOperation(ctx, command, "release")
}

func (r *openClawBillingRepository) applyOpenClawLeaseOperation(ctx context.Context, command service.OpenClawLeaseOperationCommand, operation string) (_ *service.OpenClawLeaseResult, err error) {
	if r == nil || r.db == nil {
		return nil, errors.New("openclaw billing repository db is nil")
	}
	if err := validateOpenClawLeaseOperationCommand(command); err != nil {
		return nil, err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	// A row-level lock cannot serialize two first attempts for the same key,
	// because neither attempt sees an operation row yet. Serialize the key
	// before the lookup so concurrent caller retries replay the first result.
	if err := lockOpenClawKey(ctx, tx, openClawLeaseOperationLockKey(
		command.Account.BillingAccountID,
		operation,
		command.IdempotencyKey,
	)); err != nil {
		return nil, err
	}
	alreadyApplied, existingLeaseID, err := openClawLeaseOperationAlreadyApplied(ctx, tx, command, operation)
	if err != nil {
		return nil, err
	}
	if alreadyApplied {
		lease, found, err := findOpenClawLeaseByID(ctx, tx, command.Account.BillingAccountID, existingLeaseID, true)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, service.ErrOpenClawLeaseNotFound
		}
		return r.finishOpenClawIdempotentOperation(ctx, tx, lease, command.Account.BillingUserID)
	}

	lease, found, err := findOpenClawLeaseByID(ctx, tx, command.Account.BillingAccountID, command.LeaseID, true)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, service.ErrOpenClawLeaseNotFound
	}
	if lease.BillingUserID != command.Account.BillingUserID {
		return nil, service.ErrOpenClawAccountNotFound
	}

	if lease.Status != service.OpenClawLeaseStatusActive {
		if lease.Status == service.OpenClawLeaseStatusExpired {
			return nil, service.ErrOpenClawLeaseExpired
		}
		return nil, service.ErrOpenClawLeaseFinalized
	}
	if !lease.ExpiresAt.After(time.Now().UTC()) {
		if _, err := r.expireOpenClawLeaseLocked(ctx, tx, lease); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		tx = nil
		return nil, service.ErrOpenClawLeaseExpired
	}

	remaining := openClawLeaseRemaining(lease)
	if command.AmountUSD.GreaterThan(remaining) {
		return nil, service.ErrOpenClawBudgetExceeded
	}

	var balance, frozenBalance decimal.Decimal
	switch operation {
	case "capture":
		balance, frozenBalance, err = applyOpenClawCaptureToUser(ctx, tx, command.Account.BillingUserID, command.AmountUSD)
	case "release":
		balance, frozenBalance, err = applyOpenClawReleaseToUser(ctx, tx, command.Account.BillingUserID, command.AmountUSD)
	default:
		return nil, errors.New("unsupported openclaw lease operation")
	}
	if err != nil {
		return nil, err
	}

	updatedLease, err := updateOpenClawLeaseOperation(ctx, tx, lease, operation, command.AmountUSD)
	if err != nil {
		return nil, err
	}
	if err := insertOpenClawLeaseOperation(ctx, tx, lease.BillingAccountID, lease.LeaseID, operation, command.IdempotencyKey, command.RequestFingerprint, command.AmountUSD); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	tx = nil
	return &service.OpenClawLeaseResult{
		Applied:       true,
		Lease:         *updatedLease,
		BalanceUSD:    balance,
		FrozenBalance: frozenBalance,
	}, nil
}

func (r *openClawBillingRepository) ExpireLeases(ctx context.Context, limit int) (int, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("openclaw billing repository db is nil")
	}
	if limit <= 0 || limit > 1000 {
		return 0, service.ErrOpenClawBillingInvalid
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT lease_id, billing_account_id, billing_user_id, task_id, session_id, node_id,
			reserved_usd, captured_usd, released_usd, status, expires_at, created_at, finalized_at,
			reserve_request_fingerprint
		FROM openclaw_task_budget_leases
		WHERE status = 'active' AND expires_at <= NOW()
		ORDER BY expires_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, limit)
	if err != nil {
		return 0, err
	}
	leases := make([]*service.OpenClawBudgetLease, 0, limit)
	for rows.Next() {
		lease, err := scanOpenClawLease(rows, nil)
		if err != nil {
			_ = rows.Close()
			return 0, err
		}
		leases = append(leases, lease)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, lease := range leases {
		if _, err := r.expireOpenClawLeaseLocked(ctx, tx, lease); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(leases), nil
}

func (r *openClawBillingRepository) IngestUsageEvent(ctx context.Context, command service.OpenClawUsageEventCommand) (_ *service.OpenClawUsageEventResult, err error) {
	if r == nil || r.db == nil {
		return nil, errors.New("openclaw billing repository db is nil")
	}
	if err := validateOpenClawUsageEventCommand(command); err != nil {
		return nil, err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	status := openClawInitialUsageStatus(command)
	priceSnapshot := nullableOpenClawJSON(command.PriceSnapshot)
	var claimedEventID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO openclaw_billing_usage_events (
			event_id, billing_account_id, billing_user_id, task_id, session_id, node_id,
			codex_thread_id, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			observed_at, reported_at, pricing_version, price_snapshot, estimated_usd, status, ingest_fingerprint
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		ON CONFLICT (event_id) DO NOTHING
		RETURNING event_id
	`, command.EventID, command.Account.BillingAccountID, command.Account.BillingUserID,
		command.TaskID, command.SessionID, command.NodeID, command.CodexThreadID, command.Model,
		command.InputTokens, command.OutputTokens, command.CacheReadTokens, command.CacheWriteTokens,
		command.ObservedAt, command.ReportedAt, command.PricingVersion, priceSnapshot, command.EstimatedUSD,
		status, command.IngestFingerprint).Scan(&claimedEventID)
	if errors.Is(err, sql.ErrNoRows) {
		result, existingFingerprint, err := existingOpenClawUsageEvent(ctx, tx, command.EventID)
		if err != nil {
			return nil, err
		}
		if existingFingerprint != command.IngestFingerprint {
			return nil, service.ErrOpenClawUsageEventConflict
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		tx = nil
		result.Applied = false
		return result, nil
	}
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	tx = nil
	return &service.OpenClawUsageEventResult{
		EventID: command.EventID,
		Applied: true,
		Status:  status,
	}, nil
}

func findOpenClawAccountByHMAC(ctx context.Context, tx *sql.Tx, platformUserHMAC string, lock bool) (*service.OpenClawBillingAccount, bool, error) {
	query := `
		SELECT a.billing_account_id, a.billing_user_id, a.initial_grant_usd,
			u.balance, COALESCE(u.frozen_balance, 0)
		FROM openclaw_billing_accounts AS a
		JOIN users AS u ON u.id = a.billing_user_id
		WHERE a.platform_user_hmac = $1 AND u.deleted_at IS NULL
	`
	if lock {
		query += " FOR UPDATE OF a, u"
	}
	account := &service.OpenClawBillingAccount{}
	err := tx.QueryRowContext(ctx, query, platformUserHMAC).Scan(
		&account.BillingAccountID,
		&account.BillingUserID,
		&account.InitialGrantUSD,
		&account.Balance,
		&account.FrozenBalance,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return account, true, nil
}

func findOpenClawLeaseByReserveKey(ctx context.Context, tx *sql.Tx, accountID, idempotencyKey string, lock bool) (*service.OpenClawBudgetLease, string, bool, error) {
	query := openClawLeaseSelect + `
		WHERE billing_account_id = $1 AND reserve_idempotency_key = $2
	`
	if lock {
		query += " FOR UPDATE"
	}
	var fingerprint string
	lease, err := scanOpenClawLease(tx.QueryRowContext(ctx, query, accountID, idempotencyKey), &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	return lease, fingerprint, true, nil
}

func findOpenClawLeaseByID(ctx context.Context, tx *sql.Tx, accountID, leaseID string, lock bool) (*service.OpenClawBudgetLease, bool, error) {
	query := openClawLeaseSelect + `
		WHERE billing_account_id = $1 AND lease_id = $2
	`
	if lock {
		query += " FOR UPDATE"
	}
	lease, err := scanOpenClawLease(tx.QueryRowContext(ctx, query, accountID, leaseID), nil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return lease, true, nil
}

func findActiveOpenClawLeaseForTask(ctx context.Context, tx *sql.Tx, accountID, taskID, sessionID, nodeID string, lock bool) (*service.OpenClawBudgetLease, bool, error) {
	query := openClawLeaseSelect + `
		WHERE billing_account_id = $1
			AND task_id = $2
			AND session_id = $3
			AND node_id = $4
			AND status = 'active'
	`
	if lock {
		query += " FOR UPDATE"
	}
	lease, err := scanOpenClawLease(tx.QueryRowContext(ctx, query, accountID, taskID, sessionID, nodeID), nil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return lease, true, nil
}

const openClawLeaseSelect = `
	SELECT lease_id, billing_account_id, billing_user_id, task_id, session_id, node_id,
		reserved_usd, captured_usd, released_usd, status, expires_at, created_at, finalized_at,
		reserve_request_fingerprint
	FROM openclaw_task_budget_leases
`

type openClawLeaseScanner interface {
	Scan(dest ...any) error
}

func scanOpenClawLease(scanner openClawLeaseScanner, fingerprint *string) (*service.OpenClawBudgetLease, error) {
	lease := &service.OpenClawBudgetLease{}
	var finalizedAt sql.NullTime
	var requestFingerprint string
	values := []any{
		&lease.LeaseID,
		&lease.BillingAccountID,
		&lease.BillingUserID,
		&lease.TaskID,
		&lease.SessionID,
		&lease.NodeID,
		&lease.ReservedUSD,
		&lease.CapturedUSD,
		&lease.ReleasedUSD,
		&lease.Status,
		&lease.ExpiresAt,
		&lease.CreatedAt,
		&finalizedAt,
		&requestFingerprint,
	}
	if err := scanner.Scan(values...); err != nil {
		return nil, err
	}
	lease.ExpiresAt = lease.ExpiresAt.UTC()
	lease.CreatedAt = lease.CreatedAt.UTC()
	if finalizedAt.Valid {
		value := finalizedAt.Time.UTC()
		lease.FinalizedAt = &value
	}
	if fingerprint != nil {
		*fingerprint = requestFingerprint
	}
	return lease, nil
}

func openClawBalances(ctx context.Context, tx *sql.Tx, billingUserID int64) (decimal.Decimal, decimal.Decimal, error) {
	var balance, frozenBalance decimal.Decimal
	err := tx.QueryRowContext(ctx, `
		SELECT balance, COALESCE(frozen_balance, 0)
		FROM users
		WHERE id = $1 AND deleted_at IS NULL
		FOR UPDATE
	`, billingUserID).Scan(&balance, &frozenBalance)
	if errors.Is(err, sql.ErrNoRows) {
		return decimal.Zero, decimal.Zero, service.ErrOpenClawAccountNotFound
	}
	return balance, frozenBalance, err
}

func openClawLeaseOperationAlreadyApplied(ctx context.Context, tx *sql.Tx, command service.OpenClawLeaseOperationCommand, operation string) (bool, string, error) {
	var fingerprint, leaseID string
	err := tx.QueryRowContext(ctx, `
		SELECT lease_id, request_fingerprint
		FROM openclaw_task_budget_lease_operations
		WHERE billing_account_id = $1 AND operation = $2 AND idempotency_key = $3
		FOR UPDATE
	`, command.Account.BillingAccountID, operation, command.IdempotencyKey).Scan(&leaseID, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if fingerprint != command.RequestFingerprint {
		return false, "", service.ErrOpenClawIdempotencyConflict
	}
	return true, leaseID, nil
}

func (r *openClawBillingRepository) finishOpenClawIdempotentOperation(ctx context.Context, tx *sql.Tx, lease *service.OpenClawBudgetLease, billingUserID int64) (*service.OpenClawLeaseResult, error) {
	balance, frozenBalance, err := openClawBalances(ctx, tx, billingUserID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &service.OpenClawLeaseResult{
		Applied:       false,
		Lease:         *lease,
		BalanceUSD:    balance,
		FrozenBalance: frozenBalance,
	}, nil
}

func applyOpenClawCaptureToUser(ctx context.Context, tx *sql.Tx, billingUserID int64, amount decimal.Decimal) (decimal.Decimal, decimal.Decimal, error) {
	var balance, frozenBalance decimal.Decimal
	err := tx.QueryRowContext(ctx, `
		UPDATE users
		SET frozen_balance = COALESCE(frozen_balance, 0) - $1,
			updated_at = NOW()
		WHERE id = $2
			AND deleted_at IS NULL
			AND COALESCE(frozen_balance, 0) >= $1
		RETURNING balance, frozen_balance
	`, amount, billingUserID).Scan(&balance, &frozenBalance)
	return balance, frozenBalance, err
}

func applyOpenClawReleaseToUser(ctx context.Context, tx *sql.Tx, billingUserID int64, amount decimal.Decimal) (decimal.Decimal, decimal.Decimal, error) {
	var balance, frozenBalance decimal.Decimal
	err := tx.QueryRowContext(ctx, `
		UPDATE users
		SET balance = balance + $1,
			frozen_balance = COALESCE(frozen_balance, 0) - $1,
			updated_at = NOW()
		WHERE id = $2
			AND deleted_at IS NULL
			AND COALESCE(frozen_balance, 0) >= $1
		RETURNING balance, frozen_balance
	`, amount, billingUserID).Scan(&balance, &frozenBalance)
	return balance, frozenBalance, err
}

func updateOpenClawLeaseOperation(ctx context.Context, tx *sql.Tx, lease *service.OpenClawBudgetLease, operation string, amount decimal.Decimal) (*service.OpenClawBudgetLease, error) {
	if lease == nil {
		return nil, service.ErrOpenClawLeaseNotFound
	}
	capturedDelta, releasedDelta := decimal.Zero, decimal.Zero
	if operation == "capture" {
		capturedDelta = amount
	} else if operation == "release" {
		releasedDelta = amount
	} else {
		return nil, fmt.Errorf("unsupported openclaw lease operation %q", operation)
	}
	var status string
	var finalizedAt sql.NullTime
	err := tx.QueryRowContext(ctx, `
		UPDATE openclaw_task_budget_leases
		SET captured_usd = captured_usd + $1,
			released_usd = released_usd + $2,
			status = CASE
				WHEN captured_usd + released_usd + $1 + $2 >= reserved_usd THEN
					CASE WHEN captured_usd + $1 >= reserved_usd THEN 'captured' ELSE 'released' END
				ELSE 'active'
			END,
			finalized_at = CASE
				WHEN captured_usd + released_usd + $1 + $2 >= reserved_usd THEN NOW()
				ELSE finalized_at
			END,
			updated_at = NOW()
		WHERE lease_id = $3 AND billing_account_id = $4 AND status = 'active'
		RETURNING captured_usd, released_usd, status, finalized_at
	`, capturedDelta, releasedDelta, lease.LeaseID, lease.BillingAccountID).Scan(
		&lease.CapturedUSD,
		&lease.ReleasedUSD,
		&status,
		&finalizedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrOpenClawLeaseFinalized
	}
	if err != nil {
		return nil, err
	}
	lease.Status = status
	if finalizedAt.Valid {
		value := finalizedAt.Time.UTC()
		lease.FinalizedAt = &value
	}
	return lease, nil
}

func insertOpenClawLeaseOperation(ctx context.Context, tx *sql.Tx, accountID, leaseID, operation, idempotencyKey, fingerprint string, amount decimal.Decimal) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO openclaw_task_budget_lease_operations (
			billing_account_id, lease_id, operation, idempotency_key, request_fingerprint, amount_usd
		)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, accountID, leaseID, operation, idempotencyKey, fingerprint, amount)
	return err
}

func (r *openClawBillingRepository) expireOpenClawLeaseLocked(ctx context.Context, tx *sql.Tx, lease *service.OpenClawBudgetLease) (*service.OpenClawLeaseResult, error) {
	if lease == nil || lease.Status != service.OpenClawLeaseStatusActive {
		return nil, service.ErrOpenClawLeaseFinalized
	}
	remaining := openClawLeaseRemaining(lease)
	var balance, frozenBalance decimal.Decimal
	var err error
	if remaining.GreaterThan(decimal.Zero) {
		balance, frozenBalance, err = applyOpenClawReleaseToUser(ctx, tx, lease.BillingUserID, remaining)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("openclaw frozen balance invariant violated while expiring lease")
		}
		if err != nil {
			return nil, err
		}
	} else {
		balance, frozenBalance, err = openClawBalances(ctx, tx, lease.BillingUserID)
		if err != nil {
			return nil, err
		}
	}

	var finalizedAt time.Time
	err = tx.QueryRowContext(ctx, `
		UPDATE openclaw_task_budget_leases
		SET released_usd = released_usd + $1,
			status = 'expired',
			finalized_at = NOW(),
			updated_at = NOW()
		WHERE lease_id = $2 AND billing_account_id = $3 AND status = 'active'
		RETURNING released_usd, finalized_at
	`, remaining, lease.LeaseID, lease.BillingAccountID).Scan(&lease.ReleasedUSD, &finalizedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrOpenClawLeaseFinalized
	}
	if err != nil {
		return nil, err
	}
	lease.Status = service.OpenClawLeaseStatusExpired
	finalizedAt = finalizedAt.UTC()
	lease.FinalizedAt = &finalizedAt
	if err := insertOpenClawLeaseOperation(
		ctx,
		tx,
		lease.BillingAccountID,
		lease.LeaseID,
		"expiry",
		"expiry:"+lease.LeaseID,
		openClawExpiryFingerprint(lease.LeaseID),
		remaining,
	); err != nil {
		return nil, err
	}
	return &service.OpenClawLeaseResult{
		Applied:       true,
		Lease:         *lease,
		BalanceUSD:    balance,
		FrozenBalance: frozenBalance,
	}, nil
}

func existingOpenClawUsageEvent(ctx context.Context, tx *sql.Tx, eventID string) (*service.OpenClawUsageEventResult, string, error) {
	var result service.OpenClawUsageEventResult
	var fingerprint string
	err := tx.QueryRowContext(ctx, `
		SELECT event_id, status, ingest_fingerprint
		FROM openclaw_billing_usage_events
		WHERE event_id = $1
	`, eventID).Scan(&result.EventID, &result.Status, &fingerprint)
	if err != nil {
		return nil, "", err
	}
	return &result, fingerprint, nil
}

func openClawInitialUsageStatus(command service.OpenClawUsageEventCommand) string {
	if !command.PricingComplete {
		return service.OpenClawUsageStatusPendingPricing
	}
	return service.OpenClawUsageStatusRecorded
}

func nullableOpenClawJSON(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func lockOpenClawKey(ctx context.Context, tx *sql.Tx, key string) error {
	_, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", key)
	return err
}

func openClawTaskLockKey(accountID, taskID, sessionID, nodeID string) string {
	return "task\x1f" + accountID + "\x1f" + taskID + "\x1f" + sessionID + "\x1f" + nodeID
}

func openClawReserveKeyLockKey(accountID, idempotencyKey string) string {
	return "reserve\x1f" + accountID + "\x1f" + idempotencyKey
}

func openClawLeaseOperationLockKey(accountID, operation, idempotencyKey string) string {
	return "lease-operation\x1f" + accountID + "\x1f" + operation + "\x1f" + idempotencyKey
}

func openClawLeaseRemaining(lease *service.OpenClawBudgetLease) decimal.Decimal {
	if lease == nil {
		return decimal.Zero
	}
	return lease.ReservedUSD.Sub(lease.CapturedUSD).Sub(lease.ReleasedUSD)
}

func openClawExpiryFingerprint(leaseID string) string {
	return service.HashUsageRequestPayload([]byte("openclaw-expiry\x1f" + leaseID))
}

func validateOpenClawReserveCommand(command service.OpenClawReserveLeaseCommand) error {
	if command.Account.BillingAccountID == "" || command.Account.BillingUserID <= 0 || command.LeaseID == "" ||
		command.TaskID == "" || command.SessionID == "" || command.NodeID == "" || command.IdempotencyKey == "" ||
		command.RequestFingerprint == "" || !command.AmountUSD.GreaterThan(decimal.Zero) || command.ExpiresAt.IsZero() {
		return service.ErrOpenClawBillingInvalid
	}
	return nil
}

func validateOpenClawLeaseOperationCommand(command service.OpenClawLeaseOperationCommand) error {
	if command.Account.BillingAccountID == "" || command.Account.BillingUserID <= 0 || command.LeaseID == "" ||
		command.IdempotencyKey == "" || command.RequestFingerprint == "" || !command.AmountUSD.GreaterThan(decimal.Zero) {
		return service.ErrOpenClawBillingInvalid
	}
	return nil
}

func validateOpenClawUsageEventCommand(command service.OpenClawUsageEventCommand) error {
	if command.Account.BillingAccountID == "" || command.Account.BillingUserID <= 0 || command.EventID == "" ||
		command.TaskID == "" || command.SessionID == "" || command.NodeID == "" || command.Model == "" ||
		command.ObservedAt.IsZero() || command.ReportedAt.IsZero() ||
		command.IngestFingerprint == "" {
		return service.ErrOpenClawBillingInvalid
	}
	if !validOpenClawTokenCount(command.InputTokens) ||
		!validOpenClawTokenCount(command.OutputTokens) ||
		!validOpenClawTokenCount(command.CacheReadTokens) ||
		!validOpenClawTokenCount(command.CacheWriteTokens) {
		return service.ErrOpenClawBillingInvalid
	}
	if command.EstimatedUSD != nil && command.EstimatedUSD.IsNegative() {
		return service.ErrOpenClawBillingInvalid
	}
	return nil
}

func validOpenClawTokenCount(value *int64) bool {
	return value == nil || *value >= 0
}
