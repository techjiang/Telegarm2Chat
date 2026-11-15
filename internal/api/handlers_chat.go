package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/chat"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/store"
)

func pathID(r *http.Request, key string) (int64, bool) {
	v := r.PathValue(key)
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func queryInt(r *http.Request, key string, def int64) int64 {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	chats, err := s.svc.ListChats(r.Context(), u)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeData(w, http.StatusOK, map[string]any{"chats": chats})
}

type createChatReq struct {
	Title string `json:"title"`
	Topic string `json:"topic"`
}

func (s *Server) handleCreateChat(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	req, ok := decodeJSON[createChatReq](w, r)
	if !ok {
		return
	}
	c, err := s.svc.CreateChannel(r.Context(), u, req.Title, req.Topic)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeData(w, http.StatusCreated, c)
}

type joinReq struct {
	Slug string `json:"slug"`
}

func (s *Server) handleJoinChat(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	req, ok := decodeJSON[joinReq](w, r)
	if !ok {
		return
	}
	req.Slug = strings.TrimSpace(strings.TrimPrefix(req.Slug, "#"))
	if req.Slug == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "缺少频道标识")
		return
	}
	c, err := s.svc.JoinChannel(r.Context(), u, req.Slug)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeData(w, http.StatusOK, c)
}

func (s *Server) handleLeaveChat(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "会话 ID 非法")
		return
	}
	if err := s.svc.LeaveChannel(r.Context(), u, id); err != nil {
		mapErr(w, err)
		return
	}
	writeData(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "会话 ID 非法")
		return
	}
	limit := int(queryInt(r, "limit", 40))
	before := queryInt(r, "before", 0)
	after := queryInt(r, "after", 0)
	msgs, err := s.svc.Messages(r.Context(), u, id, before, after, limit)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeData(w, http.StatusOK, map[string]any{"messages": msgs})
}

type postMessageReq struct {
	Body     string `json:"body"`
	ClientID string `json:"clientId"`
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "会话 ID 非法")
		return
	}
	req, ok := decodeJSON[postMessageReq](w, r)
	if !ok {
		return
	}
	res, err := s.svc.Send(r.Context(), u, chat.SendInput{ChatID: id, Body: req.Body, ClientID: req.ClientID})
	if err != nil {
		mapErr(w, err)
		return
	}
	writeData(w, http.StatusCreated, map[string]any{
		"message":   res.Message,
		"mentions":  res.Mentions,
		"duplicate": res.Duplicate,
	})
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	chatID, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "会话 ID 非法")
		return
	}
	mid, ok := pathID(r, "mid")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "消息 ID 非法")
		return
	}
	if err := s.svc.DeleteMessage(r.Context(), u, chatID, mid); err != nil {
		mapErr(w, err)
		return
	}
	writeData(w, http.StatusOK, map[string]bool{"ok": true})
}

type readReq struct {
	MessageID int64 `json:"messageId"`
}

func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "会话 ID 非法")
		return
	}
	req, ok := decodeJSON[readReq](w, r)
	if !ok {
		return
	}
	if err := s.svc.MarkRead(r.Context(), u, id, req.MessageID); err != nil {
		mapErr(w, err)
		return
	}
	writeData(w, http.StatusOK, map[string]bool{"ok": true})
}

type inviteReq struct {
	Username string `json:"username"`
}

// handleInviteMember 把用户拉进频道（POST /chats/{id}/members）。
func (s *Server) handleInviteMember(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "会话 ID 非法")
		return
	}
	req, ok := decodeJSON[inviteReq](w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "缺少用户名")
		return
	}
	target, err := s.svc.InviteMember(r.Context(), u, id, req.Username)
	if err != nil {
		if err.Error() == "不用邀请自己，你已经在频道里" {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		mapErr(w, err)
		return
	}
	s.svc.Store().Audit(r.Context(), u.ID, "chat.invite", target.Username, clientIP(r.Context()))
	writeData(w, http.StatusOK, map[string]any{"member": target})
}

func (s *Server) handleMembers(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "会话 ID 非法")
		return
	}
	members, err := s.svc.Members(r.Context(), u, id)
	if err != nil {
		mapErr(w, err)
		return
	}
	writeData(w, http.StatusOK, map[string]any{"members": members})
}

type directReq struct {
	Username string `json:"username"`
}

func (s *Server) handleDirect(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	req, ok := decodeJSON[directReq](w, r)
	if !ok {
		return
	}
	c, err := s.svc.OpenDirect(r.Context(), u, strings.TrimPrefix(strings.TrimSpace(req.Username), "@"))
	if err != nil {
		mapErr(w, err)
		return
	}
	writeData(w, http.StatusOK, c)
}

func (s *Server) handleUserSearch(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeData(w, http.StatusOK, map[string]any{"users": []store.User{}})
		return
	}
	users, err := s.svc.Store().SearchUsers(r.Context(), q, 10)
	if err != nil {
		mapErr(w, err)
		return
	}
	// 结果里标记自己，避免前端让你给自己开会话。
	type userHit struct {
		store.User
		IsMe bool `json:"isMe"`
	}
	out := make([]userHit, 0, len(users))
	for _, x := range users {
		out = append(out, userHit{User: x, IsMe: x.ID == u.ID})
	}
	writeData(w, http.StatusOK, map[string]any{"users": out})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	hits, err := s.svc.Search(r.Context(), u, q, int(queryInt(r, "limit", 30)))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeData(w, http.StatusOK, map[string]any{"hits": hits})
}

type linkTelegramReq struct {
	TGChatID string `json:"tgChatId"`
}

func (s *Server) handleLinkTelegram(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "会话 ID 非法")
		return
	}
	req, ok := decodeJSON[linkTelegramReq](w, r)
	if !ok {
		return
	}
	if !s.cfg.TelegramEnabled {
		writeError(w, http.StatusBadRequest, "telegram_disabled", "未配置 Telegram Bot，无法绑定")
		return
	}
	member, role, err := s.svc.Store().IsMember(r.Context(), id, u.ID)
	if err != nil {
		mapErr(w, err)
		return
	}
	if !member || (role != store.RoleOwner && !u.IsAdmin) {
		writeError(w, http.StatusForbidden, "forbidden", "只有频道创建者可以绑定 Telegram")
		return
	}
	tg := strings.TrimSpace(req.TGChatID)
	if tg == "" || len(tg) > 32 {
		writeError(w, http.StatusBadRequest, "invalid_request", "tgChatId 非法")
		return
	}
	for _, ch := range tg {
		if !(ch == '-' || (ch >= '0' && ch <= '9')) {
			writeError(w, http.StatusBadRequest, "invalid_request", "tgChatId 需为数字（群组为负数）")
			return
		}
	}
	if err := s.svc.Store().LinkTelegram(r.Context(), id, tg); err != nil {
		mapErr(w, err)
		return
	}
	s.svc.Store().Audit(r.Context(), u.ID, "telegram.link", tg, clientIP(r.Context()))
	writeData(w, http.StatusOK, map[string]string{"tgChatId": tg})
}

func (s *Server) handleUnlinkTelegram(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "会话 ID 非法")
		return
	}
	member, role, err := s.svc.Store().IsMember(r.Context(), id, u.ID)
	if err != nil {
		mapErr(w, err)
		return
	}
	if !member || (role != store.RoleOwner && !u.IsAdmin) {
		writeError(w, http.StatusForbidden, "forbidden", "只有频道创建者可以解绑")
		return
	}
	if err := s.svc.Store().UnlinkTelegram(r.Context(), id); err != nil {
		mapErr(w, err)
		return
	}
	s.svc.Store().Audit(r.Context(), u.ID, "telegram.unlink", strconv.FormatInt(id, 10), clientIP(r.Context()))
	writeData(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	st, err := s.svc.Store().Snapshot(ctx)
	if err != nil {
		mapErr(w, err)
		return
	}
	connects, delivered, dropped := s.hub.Counters()
	writeData(w, http.StatusOK, map[string]any{
		"store":  st,
		"online": s.hub.OnlineCount(),
		"conns":  s.hub.ConnCount(),
		"realtime": map[string]int64{
			"connects":  connects,
			"delivered": delivered,
			"dropped":   dropped,
		},
		"uptimeSeconds": int64(time.Since(s.startedAt).Seconds()),
	})
}
