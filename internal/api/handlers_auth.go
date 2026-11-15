package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/auth"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/store"
)

type registerReq struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Password    string `json:"password"`
	InviteCode  string `json:"inviteCode"`
}

type authResp struct {
	Token     string     `json:"token"`
	ExpiresAt int64      `json:"expiresAt"`
	User      store.User `json:"user"`
}

var displayRe = regexp.MustCompile(`^[\p{L}\p{N} _.\-]{1,32}$`)

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSON[registerReq](w, r)
	if !ok {
		return
	}
	username := store.NormalizeUsername(req.Username)
	if err := store.ValidateUsername(username); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_username", err.Error())
		return
	}
	if !displayRe.MatchString(strings.TrimSpace(req.DisplayName)) && req.DisplayName != "" {
		writeError(w, http.StatusBadRequest, "invalid_display_name", "昵称只允许 1..32 个字符")
		return
	}

	ctx := r.Context()
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		mapErr(w, err)
		return
	}
	// 首个用户自动成为管理员；其余用户若配置了邀请码需校验。
	// 判定与插入放在同一事务里，避免并发注册双双拿到管理员。
	u, isFirst, err := s.svc.Store().CreateUserAtomic(
		ctx, username, strings.TrimSpace(req.DisplayName), hash, true)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "username_taken", "用户名已被占用")
			return
		}
		mapErr(w, err)
		return
	}
	if !isFirst && s.cfg.InviteCode != "" && subtle.ConstantTimeCompare(
		[]byte(req.InviteCode), []byte(s.cfg.InviteCode)) != 1 {
		// 邀请码不对：撤销刚建的账号，避免脏数据。
		_ = s.svc.Store().DeleteUser(ctx, u.ID)
		writeError(w, http.StatusForbidden, "invite_required", "邀请码不正确")
		return
	}
	s.svc.Store().Audit(ctx, u.ID, "auth.register", u.Username, clientIP(ctx))
	s.issue(w, u)
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSON[loginReq](w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	u, err := s.svc.Store().Authenticate(ctx, req.Username, req.Password)
	if err != nil {
		s.svc.Store().Audit(ctx, 0, "auth.login_failed", store.NormalizeUsername(req.Username), clientIP(ctx))
		writeError(w, http.StatusUnauthorized, "unauthorized", "用户名或密码错误")
		return
	}
	s.svc.Store().Audit(ctx, u.ID, "auth.login", u.Username, clientIP(ctx))
	s.issue(w, u)
}

func (s *Server) issue(w http.ResponseWriter, u store.User) {
	token := s.issuer.Issue(u.ID)
	tok, _ := s.issuer.Parse(token)
	writeData(w, http.StatusOK, authResp{Token: token, ExpiresAt: tok.ExpiresAt.UnixMilli(), User: u})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	writeData(w, http.StatusOK, u)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	// 令牌无状态，客户端丢弃即可；服务端仅记录审计。
	s.svc.Store().Audit(r.Context(), u.ID, "auth.logout", u.Username, clientIP(r.Context()))
	writeData(w, http.StatusOK, map[string]bool{"ok": true})
}
