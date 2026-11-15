// Package telegram 实现 Telegram Bot API 的最小客户端与双向桥接。
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// APIError 携带 Telegram 返回的 status 与描述，便于区分可重试错误。
type APIError struct {
	Status      int
	Description string
	RetryAfter  time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram: %d %s", e.Status, e.Description)
}

// Retryable 表示是否值得重试。
func (e *APIError) Retryable() bool {
	return e.Status == 429 || e.Status >= 500
}

// Client 是 Bot API 客户端。
type Client struct {
	token string
	base  string
	http  *http.Client
}

// NewClient 创建客户端；token 为空时调用方应禁用桥接。
func NewClient(token, base string) *Client {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 70 * time.Second,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
	}
	return &Client{
		token: token,
		base:  strings.TrimRight(base, "/"),
		http:  &http.Client{Transport: transport, Timeout: 80 * time.Second},
	}
}

// Update 是 getUpdates 返回的单个更新。
type Update struct {
	UpdateID int64      `json:"update_id"`
	Message  *TGMessage `json:"message"`
}

// TGMessage 是 Telegram 消息的最小字段集。
type TGMessage struct {
	MessageID int64  `json:"message_id"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
	Caption   string `json:"caption"`
	Chat      struct {
		ID    int64  `json:"id"`
		Type  string `json:"type"`
		Title string `json:"title"`
	} `json:"chat"`
	From *struct {
		ID        int64  `json:"id"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Username  string `json:"username"`
		IsBot     bool   `json:"is_bot"`
	} `json:"from"`
}

// DisplayName 组装发送者展示名。
func (m *TGMessage) DisplayName() string {
	if m.From == nil {
		return "Telegram"
	}
	name := strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
	if name == "" {
		name = m.From.Username
	}
	if name == "" {
		name = fmt.Sprintf("tg-%d", m.From.ID)
	}
	return name + " (TG)"
}

// Text_ 取正文（文本或媒体说明）。
func (m *TGMessage) Text_() string {
	if m.Text != "" {
		return m.Text
	}
	return m.Caption
}

type apiResp[T any] struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
	Result T `json:"result"`
}

// call 发起 POST 请求并把响应解析到 out。
func call[T any](ctx context.Context, c *Client, method string, payload any, out *T) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("telegram: 编码请求: %w", err)
		}
		body = bytes.NewReader(b)
	}
	url := c.base + "/bot" + c.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("telegram: 构造请求: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// 错误信息里可能带 URL（含 token），统一脱敏。
		return fmt.Errorf("telegram: 请求 %s 失败: %w", method, redactErr(err, c.token))
	}
	defer resp.Body.Close()

	// 限制读取体积，避免被超大响应拖垮内存。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("telegram: 读取 %s 响应: %w", method, redactErr(err, c.token))
	}
	var parsed apiResp[T]
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("telegram: 解析 %s 响应: %w", method, err)
	}
	if !parsed.OK {
		code := parsed.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		e := &APIError{Status: code, Description: redact(parsed.Description, c.token)}
		if parsed.Parameters != nil && parsed.Parameters.RetryAfter > 0 {
			e.RetryAfter = time.Duration(parsed.Parameters.RetryAfter) * time.Second
		}
		return e
	}
	*out = parsed.Result
	return nil
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "<redacted>")
}

func redactErr(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	return errors.New(redact(err.Error(), secret))
}

// GetUpdates 长轮询拉取更新。
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	payload := map[string]any{
		"offset":          offset,
		"timeout":         int(timeout.Seconds()),
		"allowed_updates": []string{"message"},
		"limit":           100,
	}
	var out []Update
	if err := call(ctx, c, "getUpdates", payload, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SendMessage 发送纯文本，禁用链接预览以减少骚扰面。
func (c *Client) SendMessage(ctx context.Context, chatID, text string) (int64, error) {
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	var out struct {
		MessageID int64 `json:"message_id"`
	}
	if err := call(ctx, c, "sendMessage", payload, &out); err != nil {
		return 0, err
	}
	return out.MessageID, nil
}

// GetMe 校验 token 有效性。
func (c *Client) GetMe(ctx context.Context) (string, error) {
	var out struct {
		Username string `json:"username"`
	}
	if err := call(ctx, c, "getMe", nil, &out); err != nil {
		return "", err
	}
	return out.Username, nil
}
