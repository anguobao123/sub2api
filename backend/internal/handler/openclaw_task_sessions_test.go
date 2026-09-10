package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenClawTaskAmountsCannotBeOmitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOpenClawBillingHandler(nil)
	for _, test := range []struct {
		path, body string
		handle     gin.HandlerFunc
	}{
		{"/settings", `{}`, h.SetBillingSettings},
		{"/adjust", `{"platformUserId":"user","operation":"set","idempotencyKey":"once","reason":"test"}`, h.AdjustBalance},
	} {
		router := gin.New()
		router.POST(test.path, test.handle)
		out := httptest.NewRecorder()
		router.ServeHTTP(out, httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body)))
		require.Equal(t, http.StatusBadRequest, out.Code)
	}
}

func TestOpenClawTaskUsageContextSurvivesDetachedWorker(t *testing.T) {
	link := &service.OpenClawGatewayRequest{LeaseID: "lease", RequestID: "client:request", APIKeyID: 1, UserID: 2}
	parent, cancel := context.WithCancel(service.WithOpenClawGatewayRequest(context.Background(), link))
	cancel()
	worker := usageRecordContext(parent, context.Background())
	require.NoError(t, worker.Err())
	require.Same(t, link, service.OpenClawGatewayRequestFromContext(worker))
}
