package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestOpenClawTaskPostgres(t *testing.T) {
	dsn := os.Getenv("OPENCLAW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires the dedicated disposable PostgreSQL test database")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(16)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	require.NoError(t, ApplyMigrations(ctx, db))
	repo := &openClawBillingRepository{db: db}
	native := &usageBillingRepository{db: db}
	var groupID int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,subscription_type,status) VALUES($1,'openai','standard','active') RETURNING id`, "OpenClaw Test "+uuid.NewString()).Scan(&groupID))
	type fixture struct {
		account *service.OpenClawBillingAccount
		hmac    string
		keyID   int64
	}
	newAccount := func(t *testing.T, grant string) fixture {
		bytes := make([]byte, 32)
		_, err := rand.Read(bytes)
		require.NoError(t, err)
		hmac := hex.EncodeToString(bytes)
		id := "ocba_" + uuid.NewString()
		a, err := repo.EnsureAccount(ctx, service.OpenClawAccountProvision{PlatformUserHMAC: hmac, BillingAccountID: id, SyntheticEmail: id + "@test.invalid", PasswordHash: "!no-login!", DefaultGrantUSD: decimal.RequireFromString(grant)})
		require.NoError(t, err)
		keyID, _, err := repo.EnsureGatewayKey(ctx, *a, "test-"+uuid.NewString(), groupID)
		require.NoError(t, err)
		return fixture{a, hmac, keyID}
	}
	open := func(t *testing.T, f fixture, name string, subscription bool) *service.OpenClawTaskOpenResult {
		result, err := repo.OpenTaskSession(ctx, service.OpenClawTaskOpenCommand{Account: *f.account, PlatformUserHMAC: f.hmac, Identity: service.OpenClawTaskIdentity{TaskID: name, SessionID: "session", NodeID: "node"}, LeaseID: "lease_" + uuid.NewString(), APIKeyID: f.keyID, AllowedModels: []string{"gpt-5.5"}, Subscription: subscription, ExpiresAt: time.Now().Add(time.Minute)})
		require.NoError(t, err)
		return result
	}
	begin := func(t *testing.T, f fixture, lease *service.OpenClawTaskOpenResult) *service.OpenClawGatewayRequest {
		request := &service.OpenClawGatewayRequest{LeaseID: lease.LeaseID, RequestID: "client:" + uuid.NewString(), APIKeyID: f.keyID, UserID: f.account.BillingUserID, Model: "gpt-5.5"}
		require.NoError(t, repo.BeginGatewayRequest(ctx, request))
		return request
	}
	bill := func(f fixture, request *service.OpenClawGatewayRequest, cost float64) *service.UsageBillingCommand {
		return &service.UsageBillingCommand{RequestID: request.RequestID, APIKeyID: f.keyID, UserID: f.account.BillingUserID, Model: "gpt-5.5", InputTokens: 20, OutputTokens: 5, BalanceCost: cost, OpenClawLeaseID: request.LeaseID, OpenClawRequestID: request.RequestID, OpenClawPricingKnown: true}
	}
	balances := func(t *testing.T, f fixture, available, frozen string) {
		a, err := repo.FindAccount(ctx, f.hmac)
		require.NoError(t, err)
		require.True(t, a.Balance.Equal(decimal.RequireFromString(available)), "available=%s", a.Balance)
		require.True(t, a.FrozenBalance.Equal(decimal.RequireFromString(frozen)), "frozen=%s", a.FrozenBalance)
	}
	closeTask := func(t *testing.T, f fixture, task string) *service.OpenClawTaskCloseResult {
		result, err := repo.CloseTaskSessionByTask(ctx, f.hmac, service.OpenClawTaskIdentity{TaskID: task, SessionID: "session", NodeID: "node"}, "close:"+task)
		require.NoError(t, err)
		return result
	}

	t.Run("wallet exact charge, native dedup and refund", func(t *testing.T) {
		f := newAccount(t, "50")
		lease := open(t, f, "wallet", false)
		balances(t, f, "49", "1")
		again := open(t, f, "wallet", false)
		require.Equal(t, lease.LeaseID, again.LeaseID)
		balances(t, f, "49", "1")
		request := begin(t, f, lease)
		command := bill(f, request, .3)
		result, err := native.Apply(ctx, command)
		require.NoError(t, err)
		require.True(t, result.Applied)
		balances(t, f, "49", "0.7")
		result, err = native.Apply(ctx, command)
		require.NoError(t, err)
		require.False(t, result.Applied)
		balances(t, f, "49", "0.7")
		closed := closeTask(t, f, "wallet")
		require.Equal(t, "closed", closed.Status)
		require.Equal(t, "30000000", closed.ChargedUSD.MinorAmount)
		balances(t, f, "49.7", "0")
		require.Equal(t, "closed", closeTask(t, f, "wallet").Status)
		balances(t, f, "49.7", "0")
		renewed, err := repo.RenewTaskSession(ctx, f.account.BillingAccountID, lease.LeaseID, "wallet", "node", time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.WithinDuration(t, lease.ExpiresAt, renewed, time.Microsecond)
	})
	t.Run("parallel tasks have separate holds without fixed concurrency", func(t *testing.T) {
		f := newAccount(t, "50")
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := repo.OpenTaskSession(ctx, service.OpenClawTaskOpenCommand{Account: *f.account, PlatformUserHMAC: f.hmac, Identity: service.OpenClawTaskIdentity{TaskID: fmt.Sprint("parallel", i), SessionID: "session", NodeID: "node"}, LeaseID: "lease_" + uuid.NewString(), APIKeyID: f.keyID, AllowedModels: []string{"gpt-5.5"}, ExpiresAt: time.Now().Add(time.Minute)})
				errs <- err
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		balances(t, f, "46", "4")
		for i := 0; i < 4; i++ {
			closeTask(t, f, fmt.Sprint("parallel", i))
		}
		balances(t, f, "50", "0")
	})
	t.Run("insufficient settlement never uses another hold and top-up settles once", func(t *testing.T) {
		f := newAccount(t, "2")
		first := open(t, f, "debt", false)
		second := open(t, f, "other", false)
		balances(t, f, "0", "2")
		request := begin(t, f, first)
		result, err := native.Apply(ctx, bill(f, request, 1.5))
		require.NoError(t, err)
		require.True(t, result.OpenClawUnsettled)
		balances(t, f, "0", "2")
		blocked := &service.OpenClawGatewayRequest{LeaseID: second.LeaseID, RequestID: "client:" + uuid.NewString(), APIKeyID: f.keyID, UserID: f.account.BillingUserID, Model: "gpt-5.5"}
		require.ErrorIs(t, repo.BeginGatewayRequest(ctx, blocked), service.ErrOpenClawUnsettled)
		closed := closeTask(t, f, "debt")
		require.Equal(t, "unsettled", closed.Status)
		require.Nil(t, closed.ChargedUSD)
		transactions, err := repo.Transactions(ctx, f.account.BillingAccountID)
		require.NoError(t, err)
		require.Nil(t, transactions[0]["chargedUsd"])
		require.Equal(t, "unsettled", transactions[0]["status"])
		_, err = repo.AdjustBalance(ctx, *f.account, "add", decimal.NewFromInt(1), "credit", "test top-up")
		require.NoError(t, err)
		balances(t, f, "0.5", "1")
		_, err = repo.AdjustBalance(ctx, *f.account, "add", decimal.NewFromInt(1), "credit", "test top-up")
		require.NoError(t, err)
		balances(t, f, "0.5", "1")
		require.Equal(t, "closed", closeTask(t, f, "debt").Status)
		closeTask(t, f, "other")
		balances(t, f, "1.5", "0")
	})
	t.Run("expiry retains inflight funds and permits late real settlement", func(t *testing.T) {
		f := newAccount(t, "50")
		lease := open(t, f, "expired", false)
		request := begin(t, f, lease)
		_, err := db.ExecContext(ctx, `UPDATE openclaw_task_budget_leases SET expires_at=NOW()-INTERVAL '1 second' WHERE lease_id=$1`, lease.LeaseID)
		require.NoError(t, err)
		_, err = repo.ExpireTaskSessions(ctx, 100)
		require.NoError(t, err)
		balances(t, f, "49", "1")
		require.Equal(t, "closing", closeTask(t, f, "expired").Status)
		result, err := native.Apply(ctx, bill(f, request, .2))
		require.NoError(t, err)
		require.False(t, result.OpenClawUnsettled)
		balances(t, f, "49.8", "0")
		require.Equal(t, "closed", closeTask(t, f, "expired").Status)
	})
	t.Run("close-before-open and foreign lease are rejected", func(t *testing.T) {
		f := newAccount(t, "50")
		require.Nil(t, closeTask(t, f, "cancelled-before-open").LeaseID)
		_, err := repo.OpenTaskSession(ctx, service.OpenClawTaskOpenCommand{Account: *f.account, PlatformUserHMAC: f.hmac, Identity: service.OpenClawTaskIdentity{TaskID: "cancelled-before-open", SessionID: "session", NodeID: "node"}, LeaseID: "lease_" + uuid.NewString(), APIKeyID: f.keyID, AllowedModels: []string{"gpt-5.5"}, ExpiresAt: time.Now().Add(time.Minute)})
		require.ErrorIs(t, err, service.ErrOpenClawLeaseFinalized)
		balances(t, f, "50", "0")
		other := newAccount(t, "50")
		lease := open(t, other, "foreign", false)
		require.Error(t, repo.BeginGatewayRequest(ctx, &service.OpenClawGatewayRequest{LeaseID: lease.LeaseID, RequestID: "foreign", APIKeyID: f.keyID, UserID: f.account.BillingUserID, Model: "gpt-5.5"}))
		require.Error(t, repo.BeginGatewayRequest(ctx, &service.OpenClawGatewayRequest{LeaseID: lease.LeaseID, RequestID: "bad-model", APIKeyID: other.keyID, UserID: other.account.BillingUserID, Model: "blocked-model"}))
		closeTask(t, other, "foreign")
	})
	t.Run("native subscription consumes quota without wallet charge", func(t *testing.T) {
		f := newAccount(t, "0")
		var planID, subscriptionID int64
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,subscription_type,status) VALUES($1,'openai','subscription','active') RETURNING id`, "Plan "+uuid.NewString()).Scan(&planID))
		require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO user_subscriptions(user_id,group_id,starts_at,expires_at,status) VALUES($1,$2,NOW(),NOW()+INTERVAL '30 days','active') RETURNING id`, f.account.BillingUserID, planID).Scan(&subscriptionID))
		_, _, err := repo.EnsureGatewayKey(ctx, *f.account, "unused", planID)
		require.NoError(t, err)
		lease := open(t, f, "plan", true)
		balances(t, f, "0", "0")
		request := begin(t, f, lease)
		command := bill(f, request, 0)
		command.SubscriptionID = &subscriptionID
		command.SubscriptionCost = .7
		command.BillingType = service.BillingTypeSubscription
		_, err = native.Apply(ctx, command)
		require.NoError(t, err)
		balances(t, f, "0", "0")
		var used decimal.Decimal
		require.NoError(t, db.QueryRowContext(ctx, `SELECT daily_usage_usd FROM user_subscriptions WHERE id=$1`, subscriptionID).Scan(&used))
		require.True(t, used.Equal(decimal.RequireFromString("0.7")))
		closed := closeTask(t, f, "plan")
		require.Equal(t, "70000000", closed.ChargedUSD.MinorAmount)
	})
}
