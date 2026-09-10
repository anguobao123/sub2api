package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type openClawGatewayRepositoryStub struct {
	service.OpenClawTaskSessionRepository
	begun, finished int
	unknown         bool
	request         *service.OpenClawGatewayRequest
}

func (s *openClawGatewayRepositoryStub) BeginGatewayRequest(_ context.Context, r *service.OpenClawGatewayRequest) error {
	if r.LeaseID != "valid-lease" {
		return service.ErrOpenClawLeaseNotFound
	}
	s.begun++
	s.request = r
	return nil
}
func (s *openClawGatewayRepositoryStub) FinishGatewayRequest(_ context.Context, _ *service.OpenClawGatewayRequest, unknown bool) error {
	s.finished++
	s.unknown = unknown
	return nil
}

func TestOpenClawGatewayRequiresLeaseAndKeepsRequestIdentitySeparate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name, method, path, lease string
		dispatch                  bool
		status                    int
	}{
		{"valid", http.MethodPost, "/v1/responses", "valid-lease", true, 200},
		{"missing", http.MethodPost, "/v1/responses", "", false, 400},
		{"wrong", http.MethodPost, "/v1/responses", "other", false, 404},
		{"websocket", http.MethodGet, "/v1/responses", "valid-lease", false, 403},
		{"other-paid-route", http.MethodPost, "/v1/chat/completions", "valid-lease", false, 403},
		{"local-rejection", http.MethodPost, "/v1/responses", "valid-lease", false, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &openClawGatewayRepositoryStub{}
			cfg := &config.Config{OpenClawBilling: config.OpenClawBillingConfig{Enabled: true, InternalBearer: strings.Repeat("b", 32), IdentityHMACKey: strings.Repeat("h", 32)}}
			billing := service.NewOpenClawBillingService(nil, cfg)
			sessions := service.NewOpenClawTaskSessionService(repo, billing, nil, nil, nil, nil, cfg)
			router := gin.New()
			router.Handle(test.method, test.path, func(c *gin.Context) {
				c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, "unique-request"))
				beginOpenClawGatewayRequest(c, sessions, &service.APIKey{ID: 10, User: &service.User{ID: 20}})
			}, func(c *gin.Context) {
				link := service.OpenClawGatewayRequestFromContext(c.Request.Context())
				require.NotNil(t, link)
				require.Empty(t, c.GetHeader("X-OpenClaw-Lease"))
				link.Dispatched = test.dispatch
				c.Status(test.status)
			})
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(`{"model":"gpt-5.5","input":"hello"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-OpenClaw-Lease", test.lease)
			router.ServeHTTP(recorder, request)
			require.Equal(t, test.status, recorder.Code)
			if test.name == "valid" || test.name == "local-rejection" {
				require.Equal(t, 1, repo.begun)
				require.Equal(t, 1, repo.finished)
				require.Equal(t, test.dispatch, repo.unknown)
				require.Equal(t, "client:unique-request", repo.request.RequestID)
			} else {
				require.Zero(t, repo.begun)
			}
		})
	}
}
