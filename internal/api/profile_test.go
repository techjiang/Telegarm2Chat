package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
)

// pngBytes 生成一张 w×h 的纯色 PNG，用来喂头像接口。
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 4), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编码 PNG: %v", err)
	}
	return buf.Bytes()
}

func (h *harness) doRaw(method, path, token, contentType string, body []byte) (int, http.Header, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

func TestAvatarLifecycle(t *testing.T) {
	h := newHarness(t)
	tok, uid := h.register("avataruser", "avatarpass1234")

	// 未设置时 404，前端据此回落到字母头像。
	if code, _, _ := h.doRaw("GET", "/api/v1/users/1/avatar", "", "", nil); code != 404 {
		t.Fatalf("无头像应 404，得到 %d", code)
	}

	img := pngBytes(t, 128, 128)
	code, _, raw := h.doRaw("POST", "/api/v1/me/avatar", tok, "application/octet-stream", img)
	if code != 200 {
		t.Fatalf("上传头像失败: %d %s", code, raw)
	}
	var payload struct {
		Data struct {
			AvatarVersion int64 `json:"avatarVersion"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Data.AvatarVersion == 0 {
		t.Fatalf("响应缺少 avatarVersion: %s", raw)
	}

	// /me 要带上版本号，否则刷新后前端不知道要取图。
	_, out := h.do("GET", "/api/v1/me", tok, nil)
	if dataMap(t, out)["avatarVersion"] == nil {
		t.Fatalf("/me 未返回 avatarVersion: %v", out)
	}

	code, hdr, got := h.doRaw("GET", "/api/v1/users/1/avatar", "", "", nil)
	if code != 200 || !bytes.Equal(got, img) {
		t.Fatalf("读取头像不一致: %d, %d bytes", code, len(got))
	}
	if hdr.Get("Content-Type") != "image/png" {
		t.Fatalf("Content-Type 应为 image/png，得到 %q", hdr.Get("Content-Type"))
	}
	etag := hdr.Get("ETag")
	if etag == "" {
		t.Fatal("缺少 ETag")
	}
	// 二次请求带 If-None-Match 应 304，省流量。
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/users/1/avatar", nil)
	req.Header.Set("If-None-Match", etag)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("条件请求应 304，得到 %d", resp.StatusCode)
	}

	// 同一秒内重复上传版本号必须递增，否则浏览器会命中旧缓存。
	code, _, raw = h.doRaw("POST", "/api/v1/me/avatar", tok, "application/octet-stream", pngBytes(t, 64, 64))
	if code != 200 {
		t.Fatalf("二次上传失败: %d %s", code, raw)
	}
	var second struct {
		Data struct {
			AvatarVersion int64 `json:"avatarVersion"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &second)
	if second.Data.AvatarVersion <= payload.Data.AvatarVersion {
		t.Fatalf("头像版本未递增: %d -> %d", payload.Data.AvatarVersion, second.Data.AvatarVersion)
	}

	if code, _, _ := h.doRaw("DELETE", "/api/v1/me/avatar", tok, "", nil); code != 200 {
		t.Fatalf("删除头像失败: %d", code)
	}
	if code, _, _ := h.doRaw("GET", "/api/v1/users/1/avatar", "", "", nil); code != 404 {
		t.Fatalf("删除后应 404，得到 %d", code)
	}
	_ = uid
}

// 声明成图片但不是图片的内容必须被拒：否则同源就多了一个存储型 XSS / 资源滥用的口子。
func TestAvatarRejectsBadInput(t *testing.T) {
	h := newHarness(t)
	tok, _ := h.register("badavatar", "badavatar1234")

	cases := []struct {
		name string
		body []byte
		want int
	}{
		{"HTML 冒充图片", []byte("<html><script>alert(1)</script></html>"), 400},
		{"空内容", []byte{}, 400},
		{"超大尺寸", pngBytes(t, 600, 600), 400},
		{"超过体积上限", bytes.Repeat([]byte{0xFF, 0xD8, 0xFF}, 100<<10), 413},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, raw := h.doRaw("POST", "/api/v1/me/avatar", tok, "image/png", c.body)
			if code != c.want {
				t.Fatalf("期望 %d，得到 %d: %s", c.want, code, raw)
			}
			if code == 200 {
				t.Fatalf("不应接受 %s", c.name)
			}
		})
	}
}

func TestDisplayNameUpdate(t *testing.T) {
	h := newHarness(t)
	tok, _ := h.register("namer", "namerpass1234")

	code, out := h.do("PATCH", "/api/v1/me", tok, map[string]string{"displayName": "新名字"})
	if code != 200 || dataMap(t, out)["displayName"] != "新名字" {
		t.Fatalf("改昵称失败: %d %v", code, out)
	}
	if code, _ := h.do("PATCH", "/api/v1/me", tok, map[string]string{"displayName": ""}); code != 400 {
		t.Fatalf("空昵称应 400，得到 %d", code)
	}
	if code, _ := h.do("PATCH", "/api/v1/me", tok,
		map[string]string{"displayName": strings.Repeat("字", 40)}); code != 400 {
		t.Fatalf("过长昵称应 400，得到 %d", code)
	}
	// 昵称不能开成注入点：尖括号被拒。
	if code, _ := h.do("PATCH", "/api/v1/me", tok,
		map[string]string{"displayName": "<img src=x>"}); code != 400 {
		t.Fatalf("含尖括号的昵称应 400，得到 %d", code)
	}
	if code, _ := h.do("PATCH", "/api/v1/me", "", map[string]string{"displayName": "x"}); code != 401 {
		t.Fatalf("未登录改昵称应 401，得到 %d", code)
	}
}

// 头像与昵称要出现在消息里，否则聊天气泡旁永远是字母。
func TestMessageCarriesSenderAvatar(t *testing.T) {
	h := newHarness(t)
	tok, _ := h.register("sender", "senderpass1234")
	if code, _, raw := h.doRaw("POST", "/api/v1/me/avatar", tok, "application/octet-stream",
		pngBytes(t, 64, 64)); code != 200 {
		t.Fatalf("上传头像失败: %d %s", code, raw)
	}
	_, out := h.do("POST", "/api/v1/chats", tok, map[string]string{"title": "头像频道"})
	chatID := ftoa(dataMap(t, out)["id"].(float64))

	_, out = h.do("POST", "/api/v1/chats/"+chatID+"/messages", tok, map[string]string{"body": "带头像的消息"})
	msg := dataMap(t, out)["message"].(map[string]any)
	if v, _ := msg["senderAvatar"].(float64); v == 0 {
		t.Fatalf("消息未携带 senderAvatar: %v", msg)
	}

	// 列表接口同样要带上，刷新后仍能画出来。
	_, out = h.do("GET", "/api/v1/chats/"+chatID+"/messages", tok, nil)
	msgs := dataMap(t, out)["messages"].([]any)
	first := msgs[0].(map[string]any)
	if v, _ := first["senderAvatar"].(float64); v == 0 {
		t.Fatalf("消息列表未携带 senderAvatar: %v", first)
	}
}

func TestInviteAndJoin(t *testing.T) {
	h := newHarness(t)
	aliceTok, _ := h.register("alice", "alicepass1234")
	bobTok, _ := h.register("bob", "bobpass1234567")

	_, out := h.do("POST", "/api/v1/chats", aliceTok, map[string]string{"title": "邀请测试"})
	chat := dataMap(t, out)
	chatID := ftoa(chat["id"].(float64))
	slug := chat["slug"].(string)

	// bob 此时还不是成员，读不到消息。
	if code, _ := h.do("GET", "/api/v1/chats/"+chatID+"/messages", bobTok, nil); code != 403 {
		t.Fatalf("非成员应 403，得到 %d", code)
	}
	code, out := h.do("POST", "/api/v1/chats/"+chatID+"/members", aliceTok, map[string]string{"username": "bob"})
	if code != 200 {
		t.Fatalf("邀请失败: %d %v", code, out)
	}
	if code, _ := h.do("GET", "/api/v1/chats/"+chatID+"/messages", bobTok, nil); code != 200 {
		t.Fatalf("被邀请后应能读消息，得到 %d", code)
	}
	// 重复邀请幂等。
	if code, _ := h.do("POST", "/api/v1/chats/"+chatID+"/members", aliceTok, map[string]string{"username": "bob"}); code != 200 {
		t.Fatalf("重复邀请应幂等，得到 %d", code)
	}
	// 邀请自己应被拒。
	if code, _ := h.do("POST", "/api/v1/chats/"+chatID+"/members", aliceTok, map[string]string{"username": "alice"}); code != 400 {
		t.Fatalf("邀请自己应 400，得到 %d", code)
	}
	// 非成员不能邀请人进来。
	eveTok, _ := h.register("eve", "evepass12345")
	if code, _ := h.do("POST", "/api/v1/chats/"+chatID+"/members", eveTok, map[string]string{"username": "eve"}); code != 403 {
		t.Fatalf("非成员邀请应 403，得到 %d", code)
	}

	// 用频道号加入
	carolTok, _ := h.register("carol", "carolpass1234")
	code, out = h.do("POST", "/api/v1/chats/join", carolTok, map[string]string{"slug": slug})
	if code != 200 {
		t.Fatalf("按频道号加入失败: %d %v", code, out)
	}
	if code, _ := h.do("GET", "/api/v1/chats/"+chatID+"/messages", carolTok, nil); code != 200 {
		t.Fatalf("加入后应能读消息，得到 %d", code)
	}
}

// 私聊列表要能拿到对方信息，前端才能显示对方头像而不是「私」字。
func TestDirectChatExposesPeer(t *testing.T) {
	h := newHarness(t)
	aliceTok, _ := h.register("alice", "alicepass1234")
	bobTok, bobID := h.register("bobbb", "bobpass12345")
	if code, _, raw := h.doRaw("POST", "/api/v1/me/avatar", bobTok, "application/octet-stream",
		pngBytes(t, 64, 64)); code != 200 {
		t.Fatalf("bob 上传头像失败: %d %s", code, raw)
	}
	if code, out := h.do("POST", "/api/v1/direct", aliceTok, map[string]string{"username": "bobbb"}); code != 200 {
		t.Fatalf("创建私聊失败: %d %v", code, out)
	}
	_, out := h.do("GET", "/api/v1/chats", aliceTok, nil)
	chats := dataMap(t, out)["chats"].([]any)
	var found bool
	for _, raw := range chats {
		c := raw.(map[string]any)
		if c["kind"] != "direct" {
			continue
		}
		peer, ok := c["peer"].(map[string]any)
		if !ok {
			t.Fatalf("私聊缺少 peer 字段: %v", c)
		}
		found = true
		if int64(peer["id"].(float64)) != bobID {
			t.Fatalf("peer.id 应为 %d，得到 %v", bobID, peer["id"])
		}
		if v, _ := peer["avatarVersion"].(float64); v == 0 {
			t.Fatalf("peer 缺少 avatarVersion: %v", peer)
		}
	}
	if !found {
		t.Fatal("会话列表里没有私聊")
	}
}
