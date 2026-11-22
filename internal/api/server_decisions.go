package api

import (
	"bytes"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	decisionsPluginID = "decisions-proxy"
	decisionsPath     = "/v1/decisions"
)

func (s *Server) decisions(c *gin.Context) {
	cfg := s.getConfig()
	if cfg == nil || cfg.Home.Enabled {
		c.Status(http.StatusNotFound)
		return
	}
	dispatch := s.pluginHost.PluginHTTPHandler(decisionsPluginID)
	if dispatch == nil {
		c.Status(http.StatusNotFound)
		return
	}
	body, errRead := handlers.ReadRequestBody(c)
	if errRead != nil {
		c.JSON(clienterror.HTTPStatusFromErrorOr(errRead, http.StatusBadRequest), gin.H{"error": "Failed to read decisions request"})
		return
	}
	model := gjson.GetBytes(body, "model").String()
	body = helps.ApplyPayloadConfigWithRequestForExecutor(cfg, decisionsPluginID, model, "decisions", "decisions", "", body, body, model, decisionsPath, c.Request.Header)
	ctx := c.Request.Context()
	// Only send the fixed endpoint and finalized payload to the plugin. CPA frontend
	// credentials and client-supplied routing parameters stay on this side.
	req, errBuild := http.NewRequestWithContext(ctx, http.MethodPost, decisionsPath, bytes.NewReader(body))
	if errBuild != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to build decisions request"})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, errDispatch := dispatch(ctx, req)
	if errDispatch != nil {
		status := clienterror.HTTPStatusFromErrorOr(errDispatch, http.StatusBadGateway)
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		c.JSON(status, gin.H{"error": "Decisions upstream request failed"})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close decisions response")
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 599 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Invalid decisions upstream status"})
		return
	}
	for _, name := range []string{"Content-Type", "X-Request-Id", "Retry-After", "X-RateLimit-Limit-Requests", "X-RateLimit-Limit-Tokens", "X-RateLimit-Remaining-Requests", "X-RateLimit-Remaining-Tokens", "X-RateLimit-Reset-Requests", "X-RateLimit-Reset-Tokens"} {
		for _, value := range resp.Header.Values(name) {
			c.Writer.Header().Add(name, value)
		}
	}
	c.Status(resp.StatusCode)
	if _, errCopy := io.Copy(c.Writer, resp.Body); errCopy != nil {
		log.WithError(errCopy).Debug("failed to write decisions response")
	}
}
