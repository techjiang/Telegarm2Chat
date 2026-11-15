# 架构说明

## 一次发消息的完整路径

```
浏览器 ──POST /chats/{id}/messages──► api.handlePostMessage
                                        │
                                        ▼
                                    chat.Service.Send
                                        │ 1. 校验成员身份（store.IsMember）
                                        │ 2. 落库（store.AppendMessage，clientID 幂等）
                                        │ 3. 解析 @提及（仅会话成员）
                                        │ 4. 定向推送（hub.ToUsers(members)）
                                        │ 5. 更新已读位
                                        │ 6. 入 TG 外发队列（若有绑定）
                                        ▼
                              ┌─────────┴─────────┐
                        WebSocket 在线端      tg_outbox 表
                                                │
                                                ▼
                                        telegram.Bridge.outboxLoop
                                                │ 失败 → 指数退避重试（≤8 次）
                                                ▼
                                          Bot API sendMessage
```

关键点：**推送在事务提交之后**。先落库再广播，前端拿到的 `id` 一定是持久的，
重连补拉时按 `id > lastSeen` 就能对齐，不会出现"幽灵消息"。

## 为什么是 SQLite

单机聊天服务的瓶颈通常不是数据库，而是推送扇出。SQLite 在 WAL 模式下读写不互斥，
配合 `busy_timeout` 和连接数上限，单机能轻松扛住万级消息/分钟；同时省掉了运维 PG 的成本。
如果将来要水平扩展，`store` 包把数据访问收敛在一层，替换实现即可。

FTS5 用 `unicode61` 分词器而非 `trigram`——实测前者对中文连续文本的可检索性更好，
再叠加一层 `LIKE` 子串回退，覆盖"搜片段"这种真实用法。

## 实时推送的连接模型

```
Hub
 ├── clients[userID] → {conn1, conn2, ...}   // 同账号多端
 └── 每个 Client: send chan []byte (64) + done chan struct{}
```

- `send` 通道**只由 Hub 写入、由该连接的 writer 协程读取，永不 close**，
  避免"向已关闭 channel 发送"的 panic
- `done` 由 `Close()` 经 `sync.Once` 关闭，所有发送路径都同时监听 `done`
- 缓冲满 => 说明客户端消费不过来 => 主动断开，由前端重连补拉，而不是拖垮全局

这个结构有专门的并发测试（`TestConcurrentSendAndClose`），在 `-race` 下跑到干净。

## 中间件顺序

```
withClientIP → requestLog → recoverer → securityHeaders → rateLimit → maxBody → mux
```

`withClientIP` 必须在最外层，否则限流拿不到真实 IP。
`requestLog` 包装了 `ResponseWriter`，所以它必须显式实现 `Hijack`——
`coder/websocket` 用类型断言取 `http.Hijacker`，不会走 `Unwrap`。这是个真实踩过的坑：
所有 HTTP 测试都绿，只有真浏览器连 WS 时才暴露 501。

## 鉴权

令牌形如 `userID:issuedAt:HMAC-SHA256(payload)[:16]`，Base64URL 编码。
无状态意味着水平扩展不用共享 session；代价是**无法单条撤销**——
登出只是客户端丢弃令牌，服务端靠 TTL 兜底。若需要即时吊销，加一张 `revoked_before` 表即可。

## Telegram 桥的幂等与退避

- 入站：`getUpdates` 长轮询，offset 持久化到 `tg_state`，进程重启不重复消费
- 入站过滤：机器人自己发的、未绑定群的消息直接丢
- 出站：`tg_outbox` 表 + `ON CONFLICT(message_id) DO NOTHING`，一条消息只入队一次
- 退避：`next_try = now + 2^attempts`（1s→64s），429 优先用服务端 `retry_after`
- 不可重试错误（4xx）直接放弃，避免毒消息堵住队列
- 错误信息里的 token 会被脱敏，防止写进日志

## 头像为什么要单独一张表

`user_avatars(user_id, mime, bytes, updated_at)`，主键是 `user_id`。

- **不塞进 `users`**：`users` 每读一次都要全列，头像平均几十 KB，
  让「查用户」这种热路径背上大 `BLOB` 不划算；单独表按需 JOIN
- **不做 `ALTER TABLE`**：老库升级只要 `CREATE TABLE IF NOT EXISTS` 就够，
  迁移不用写版本判断
- **`updated_at` 既是时间戳也是缓存版本**：前端拼 `/avatar?v=<updated_at>`，
  上传后立刻换 URL，不用手动清缓存。同一秒内连传两张时必须递增（取 `prev+1`），
  否则秒级时间戳不变，浏览器会拿旧图——这个坑在浏览器测试里露过一次

类型判定只看**魔数**，不信客户端声明的 `Content-Type`。否则把一段 HTML 声明成
`image/png` 存进同源，就是一个存储型 XSS 的载体（`<img>` 会按图片处理，
但直接访问 URL 会按 HTML 渲染）。宽高用 `image.DecodeConfig` 读，
只读头部不解码整图，顺手挡掉像素炸弹；WebP 标准库没有解码器，
按 RIFF 头里 VP8/VP8L/VP8X 三种子格式手工解析。

传输用裸字节而非 multipart：前端已经把图缩到 256px，服务端不需要解析边界。

## 前端

无构建、无框架、无依赖，三个静态文件内嵌进二进制。
所有用户数据只经 `textContent` 与 `createElement` 写入 DOM——这不是洁癖，
是因为聊天内容天然不可信，任何 `innerHTML` 都是 XSS 的入口。
自带的 XSS 测试会真的往消息里塞 `<img onerror=...>` 并断言脚本没执行。

WebSocket 断线用指数退避重连（0.8s→30s），重连前先探一次 `/me`，
避免过期令牌导致死循环重试。
