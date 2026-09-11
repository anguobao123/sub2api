package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func openClawReadOnlyGatewayPath(c *gin.Context) bool {
	if c.Request.Method != http.MethodGet {
		return false
	}
	path := c.Request.URL.Path
	return path == "/v1/usage" || path == "/v1/usage/requests" || path == "/v1/sub2api/billing" || path == "/v1/models" || path == "/models"
}

func beginOpenClawGatewayRequest(c *gin.Context, sessions *service.OpenClawTaskSessionService, key *service.APIKey) bool {
	path := strings.TrimRight(c.Request.URL.Path, "/")
	allowedPath := false
	for _, root := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		if path == root || path == root+"/compact" {
			allowedPath = true
		}
	}
	if c.Request.Method != http.MethodPost || !allowedPath {
		AbortWithError(c, 403, "OPENCLAW_UNSUPPORTED_TRANSPORT", "Task keys require HTTP Responses transport")
		return false
	}
	models, ok := groupModelAllowlistModelsFromBody(c)
	if !ok {
		return false
	}
	if len(models) != 1 {
		AbortWithError(c, 400, "OPENCLAW_MODEL_REQUIRED", "A single explicit model is required")
		return false
	}
	id, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
	if id == "" {
		AbortWithError(c, 500, "OPENCLAW_REQUEST_ID_MISSING", "Request identity is unavailable")
		return false
	}
	request := &service.OpenClawGatewayRequest{LeaseID: c.GetHeader("X-OpenClaw-Lease"), RequestID: "client:" + id, APIKeyID: key.ID, UserID: key.User.ID, Model: models[0]}
	if err := sessions.BeginRequest(c.Request.Context(), request); err != nil {
		status, e := infraerrors.ToHTTP(err)
		AbortWithError(c, status, e.Reason, e.Message)
		return false
	}
	request.ReportFailure = func(ctx context.Context) error { return sessions.RecordUnknownRequest(ctx, request) }
	c.Request.Header.Del("X-OpenClaw-Lease")
	c.Request = c.Request.WithContext(service.WithOpenClawGatewayRequest(c.Request.Context(), request))
	c.Next()
	finishCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sessions.FinishRequest(finishCtx, request); err != nil {
		logger.L().Error("openclaw gateway request finalization failed")
	}
	return false
}
