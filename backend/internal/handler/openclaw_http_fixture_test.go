package handler_test

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Opt-in, disposable integration fixture. It exposes only the private bridge,
// with native PostgreSQL repositories. No provider/model route is registered.
func TestOpenClawHTTPFixture(t *testing.T) {
	listen := os.Getenv("OPENCLAW_TEST_HTTP_LISTEN")
	if listen == "" {
		t.Skip("opt-in fixture for Suite RPC integration")
	}
	dsn := os.Getenv("OPENCLAW_TEST_DATABASE_URL")
	bearer := os.Getenv("OPENCLAW_TEST_BRIDGE_BEARER")
	require.NotEmpty(t, dsn)
	require.GreaterOrEqual(t, len(bearer), 32)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Use a fresh disposable database when the migration filename changes.
	require.NoError(t, repository.ApplyMigrations(ctx, db))
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	var groupID int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups(name,platform,subscription_type,status) VALUES($1,'openai','standard','active') RETURNING id`, "OpenClaw HTTP Fixture "+uuid.NewString()).Scan(&groupID))
	cfg := &config.Config{OpenClawBilling: config.OpenClawBillingConfig{Enabled: true, GatewayGroupID: groupID, InternalBearer: bearer, IdentityHMACKey: strings.Repeat("fixture-identity-", 3), DefaultGrantUSD: 50, LeaseTTLSeconds: 900, MaxUsageEventsPerBatch: 100, ExpiryBatchSize: 100}}
	groups := repository.NewGroupRepository(client, db)
	users := repository.NewUserRepository(client, db)
	userSubs := repository.NewUserSubscriptionRepository(client)
	keys := service.NewAPIKeyService(repository.NewAPIKeyRepository(client, db), users, groups, userSubs, nil, nil, cfg)
	subscriptions := service.NewSubscriptionService(groups, userSubs, nil, client, cfg)
	defer subscriptions.Stop()
	billing := service.NewOpenClawBillingService(repository.NewOpenClawBillingRepository(client, db), cfg)
	sessions := service.NewOpenClawTaskSessionService(repository.NewOpenClawTaskSessionRepository(client, db), billing, keys, subscriptions, groups, repository.NewSettingRepository(client), cfg)
	idempotency := service.NewIdempotencyCoordinator(repository.NewIdempotencyRepository(client, db), service.DefaultIdempotencyConfig())
	previous := service.DefaultIdempotencyCoordinator()
	service.SetDefaultIdempotencyCoordinator(idempotency)
	defer service.SetDefaultIdempotencyCoordinator(previous)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(gin.Recovery())
	routes.RegisterOpenClawBillingRoutes(router, handler.ProvideOpenClawBillingHandler(billing, sessions), middleware.NewOpenClawBillingAuth(cfg))
	stop := make(chan struct{}, 1)
	router.POST("/__test/stop", func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer "+bearer {
			c.Status(http.StatusUnauthorized)
			return
		}
		select {
		case stop <- struct{}{}:
		default:
		}
		c.Status(http.StatusNoContent)
	})
	listener, err := net.Listen("tcp", listen)
	require.NoError(t, err)
	server := &http.Server{Handler: router, ReadHeaderTimeout: 10 * time.Second}
	defer server.Close()
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			select {
			case stop <- struct{}{}:
			default:
			}
		}
	}()
	fmt.Printf("OPENCLAW_HTTP_FIXTURE_READY address=%s gatewayGroupId=%d\n", listener.Addr(), groupID)
	select {
	case <-stop:
	case <-time.After(15 * time.Minute):
	}
}
