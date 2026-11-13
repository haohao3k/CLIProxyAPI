package management

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cursorguard"
)

const cursorSpendGuardField = "cursor-spend-guard"

// GetCursorSpendGuard returns the guard settings (`GET /v0/management/cursor-spend-guard`).
func (h *Handler) GetCursorSpendGuard(c *gin.Context) {
	h.mu.Lock()
	settings := h.cfg.CursorSpendGuard
	h.mu.Unlock()
	c.JSON(200, gin.H{cursorSpendGuardField: settings})
}

// PutCursorSpendGuard replaces the guard settings (`PUT /v0/management/cursor-spend-guard`).
func (h *Handler) PutCursorSpendGuard(c *gin.Context) {
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	raw, ok := body[cursorSpendGuardField]
	if !ok {
		c.JSON(400, gin.H{"error": "missing " + cursorSpendGuardField})
		return
	}
	var settings config.CursorSpendGuardConfig
	if err := json.Unmarshal(raw, &settings); err != nil {
		c.JSON(400, gin.H{"error": "invalid " + cursorSpendGuardField})
		return
	}
	settings.Sanitize()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.CursorSpendGuard = settings
	h.persistLocked(c)
}

// GetCursorSpendGuardStatus reports each Cursor credential (`GET /v0/management/cursor-spend-guard/status`).
func (h *Handler) GetCursorSpendGuardStatus(c *gin.Context) {
	h.writeCursorSpendGuardStatus(c, false)
}

// RecheckCursorSpendGuard polls every Cursor credential now and reports them
// (`POST /v0/management/cursor-spend-guard/recheck`).
func (h *Handler) RecheckCursorSpendGuard(c *gin.Context) {
	h.writeCursorSpendGuardStatus(c, true)
}

func (h *Handler) writeCursorSpendGuardStatus(c *gin.Context, recheck bool) {
	h.mu.Lock()
	cfg := h.cfg
	manager := h.authManager
	h.mu.Unlock()
	settings := cfg.CursorSpendGuard
	credentials := []cursorguard.CredentialStatus{}
	if manager != nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
		defer cancel()
		credentials = cursorguard.Default().Status(ctx, cfg, manager.List(), recheck)
	}
	c.JSON(200, gin.H{
		"enabled":       settings.Enabled,
		"cap_usd":       settings.MaxOverageUSD,
		"poll_interval": settings.PollEvery().String(),
		"credentials":   credentials,
	})
}
