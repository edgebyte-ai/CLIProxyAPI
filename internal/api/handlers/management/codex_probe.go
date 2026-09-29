package management

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// ProbeCodexCooldown verifies one credential without selecting another account.
func (h *Handler) ProbeCodexCooldown(c *gin.Context) {
	var req struct {
		AuthIndex string `json:"auth_index"`
	}
	if errBind := c.ShouldBindJSON(&req); errBind != nil || strings.TrimSpace(req.AuthIndex) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	auth := h.authByIndex(strings.TrimSpace(req.AuthIndex))
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	report, errProbe := h.authManager.ProbeCodexCooldown(c.Request.Context(), auth.ID)
	if errProbe != nil {
		status := http.StatusBadRequest
		var authErr *coreauth.Error
		if errors.As(errProbe, &authErr) && authErr.HTTPStatus != 0 {
			status = authErr.HTTPStatus
		}
		c.JSON(status, gin.H{"error": errProbe.Error()})
		return
	}
	c.JSON(http.StatusOK, report)
}
