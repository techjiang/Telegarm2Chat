package api

import (
	"net/http"
	"time"
)

// Version 由构建时注入。
var Version = "dev"

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeData(w, http.StatusOK, map[string]string{"status": "ok", "version": Version})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 2*time.Second)
	defer cancel()
	if err := s.svc.Store().DB().PingContext(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "数据库不可用")
		return
	}
	writeData(w, http.StatusOK, map[string]string{"status": "ready"})
}

// handleMeta 暴露前端引导信息：是否首个用户、是否开启 Telegram、邀请码策略。
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 2*time.Second)
	defer cancel()
	count, err := s.svc.Store().CountUsers(ctx)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeData(w, http.StatusOK, map[string]any{
		"version":         Version,
		"firstRun":        count == 0,
		"telegramEnabled": s.cfg.TelegramEnabled,
		"inviteRequired":  count > 0 && s.cfg.InviteCode != "",
		"maxMessageLen":   4000,
	})
}
