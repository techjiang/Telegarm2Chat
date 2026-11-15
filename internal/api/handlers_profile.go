package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/realtime"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/store"
)

// handleUpdateMe 修改昵称。用户名不可改（它是 @提及与登录凭据）。
func (s *Server) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	req, ok := decodeJSON[updateMeReq](w, r)
	if !ok {
		return
	}
	if req.DisplayName == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "没有需要更新的字段")
		return
	}
	name := strings.TrimSpace(*req.DisplayName)
	if name == "" || utf8.RuneCountInString(name) > store.MaxDisplayLen {
		writeError(w, http.StatusBadRequest, "invalid_display_name",
			"昵称需为 1.."+strconv.Itoa(store.MaxDisplayLen)+" 个字符")
		return
	}
	if !displayRe.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_display_name", "昵称只允许文字、数字、空格与 _ . -")
		return
	}
	updated, err := s.svc.Store().SetDisplayName(r.Context(), u.ID, name)
	if err != nil {
		mapErr(w, err)
		return
	}
	s.svc.Store().Audit(r.Context(), u.ID, "profile.update", name, clientIP(r.Context()))
	s.svc.Hub().ToUsers([]int64{u.ID}, realtime.Event{Type: realtime.EventProfile, Data: updated, TS: realtime.Now()})
	writeData(w, http.StatusOK, updated)
}

type updateMeReq struct {
	DisplayName *string `json:"displayName"`
}

// handleUploadAvatar 接收原始图片字节（非 multipart）：前端已把图缩到 256px 内，
// 用裸 body 传输比构造 multipart 更省事，也不用解析边界。
func (s *Server) handleUploadAvatar(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	// 单独收紧体积上限，别让 256KB 的图去蹭全局的 256KB 之外的额度。
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxAvatarBytes+1024)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "avatar_too_large", "头像不能超过 256 KB")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "读取头像失败")
		return
	}
	av, err := s.svc.Store().SetAvatar(r.Context(), u.ID, raw)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrAvatarTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "avatar_too_large", "头像不能超过 256 KB")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "用户不存在")
		default:
			writeError(w, http.StatusBadRequest, "invalid_avatar", err.Error())
		}
		return
	}
	s.svc.Store().Audit(r.Context(), u.ID, "profile.avatar", av.MIME, clientIP(r.Context()))
	u.AvatarVersion = av.UpdatedAt
	s.svc.Hub().ToUsers([]int64{u.ID}, realtime.Event{Type: realtime.EventProfile, Data: u, TS: realtime.Now()})
	writeData(w, http.StatusOK, map[string]any{"avatarVersion": av.UpdatedAt, "user": u})
}

func (s *Server) handleDeleteAvatar(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	if err := s.svc.Store().DeleteAvatar(r.Context(), u.ID); err != nil {
		mapErr(w, err)
		return
	}
	u.AvatarVersion = 0
	s.svc.Hub().ToUsers([]int64{u.ID}, realtime.Event{Type: realtime.EventProfile, Data: u, TS: realtime.Now()})
	writeData(w, http.StatusOK, map[string]any{"user": u})
}

// handleAvatar 输出头像图片。任何人都能读——头像本来就在每条消息旁展示，
// 加鉴权只会让 <img> 拿不到图。响应带强 ETag，浏览器缓存一天。
func (s *Server) handleAvatar(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "用户 ID 非法")
		return
	}
	av, err := s.svc.Store().AvatarByUserID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 没有头像不是错误，返回 404 让前端回落到字母头像。
			http.Error(w, "no avatar", http.StatusNotFound)
			return
		}
		mapErr(w, err)
		return
	}
	etag := `"` + strconv.FormatInt(id, 10) + "-" + strconv.FormatInt(av.UpdatedAt, 10) + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "public, max-age=86400")
	h.Set("Content-Type", av.MIME)
	h.Set("Content-Length", strconv.Itoa(len(av.Bytes)))
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(av.Bytes)
}
