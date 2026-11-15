# HTTP API

所有业务响应统一封装：

```json
{ "ok": true, "data": { } }
{ "ok": false, "error": { "code": "forbidden", "message": "无权访问该资源" } }
```

认证用 `Authorization: Bearer <token>`；WebSocket 因浏览器限制可用 `?token=`。

## 公共

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/healthz` | 存活探针 |
| GET | `/readyz` | 就绪探针（带 DB ping） |
| GET | `/api/v1/meta` | 引导信息：是否首启、是否启用 TG、消息长度上限 |
| POST | `/api/v1/auth/register` | 注册，首用户自动为管理员 |
| POST | `/api/v1/auth/login` | 登录，返回令牌 |

```bash
curl -X POST localhost:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","displayName":"Alice","password":"at-least-10-chars"}'
```

口令要求 10..256 字符；用户名 3..32，仅小写字母/数字/`_`/`-`。

## 需认证

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/v1/me` | 当前用户 |
| PATCH | `/api/v1/me` | 改昵称 `{displayName}` |
| POST | `/api/v1/me/avatar` | 上传头像（原始图片字节，≤256 KB） |
| DELETE | `/api/v1/me/avatar` | 删除头像，回落字母头像 |
| POST | `/api/v1/auth/logout` | 登出（审计用，令牌由客户端丢弃） |
| GET | `/api/v1/chats` | 会话列表（含未读数、最后一条预览） |
| POST | `/api/v1/chats` | 建频道 `{title, topic}` |
| POST | `/api/v1/chats/join` | 加入频道 `{slug}` |
| POST | `/api/v1/chats/{id}/leave` | 退出频道（owner 不可退） |
| GET | `/api/v1/chats/{id}/messages` | 消息分页 `?limit=&before=&after=` |
| POST | `/api/v1/chats/{id}/messages` | 发消息 `{body, clientId?}` |
| DELETE | `/api/v1/chats/{id}/messages/{mid}` | 撤回（作者或管理员） |
| POST | `/api/v1/chats/{id}/read` | 标记已读 `{messageId}` |
| GET | `/api/v1/chats/{id}/members` | 成员列表 |
| POST | `/api/v1/chats/{id}/members` | 邀请用户进频道 `{username}` |
| POST | `/api/v1/chats/{id}/telegram` | 绑定 TG 群 `{tgChatId}` |
| DELETE | `/api/v1/chats/{id}/telegram` | 解绑 |
| POST | `/api/v1/direct` | 打开私聊 `{username}` |
| GET | `/api/v1/users/search` | 用户名检索 `?q=` |
| GET | `/api/v1/search` | 全文搜索 `?q=&limit=` |
| GET | `/api/v1/stats` | 运行时统计 |

### 分页语义

- 不带 `before`/`after`：返回最新 N 条（响应内按时间**正序**）
- `before=<id>`：向历史翻页
- `after=<id>`：增量补拉，用于断线重连

### 幂等发送

带上 `clientId`（前端用 UUID），同一会话内重复提交只落库一次，响应里 `duplicate: true`。
这是弱网重试的正确姿势——不用猜"到底发出去没有"。

```bash
curl -X POST localhost:8080/api/v1/chats/1/messages \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"body":"@bob 看一下这个","clientId":"9f1c-..."}'
```

### 头像

上传走裸字节而不是 multipart——前端已把图缩到 256px，没必要再包一层边界。

```bash
curl -X POST localhost:8080/api/v1/me/avatar \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/octet-stream' \
  --data-binary @avatar.png
```

服务端只按**魔数**判定类型（PNG / JPEG / GIF / WebP），不信任声明的 `Content-Type`；
单边超过 512px 或体积超过 256 KB 直接拒绝。

读取接口公开，方便 `<img>` 直接引用：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/v1/users/{id}/avatar` | 返回图片字节；没有头像时 404 |
| HEAD | `/api/v1/users/{id}/avatar` | 只取响应头 |

响应带 `ETag` 与 `Cache-Control: max-age=86400`，支持 `If-None-Match` 返回 304。
用户的 `avatarVersion` 是头像更新时间戳，前端拼成 `?v=<ver>` 打破缓存；同一秒内
重复上传时服务端保证版本号严格递增。

## WebSocket

```
GET /api/v1/ws?token=<token>
```

连接成功先收到 `ready`。之后服务端单向推送：

| 事件 | 载荷 |
| --- | --- |
| `ready` | `{userId, username}` |
| `message` | 完整消息对象 |
| `message.deleted` | `{id}` |
| `chat` | 会话变更 |
| `read` | `{messageId}` |
| `profile` | 用户对象（昵称或头像变更，多端同步用） |

客户端可发 `{"type":"read","chatId":1,"messageId":42}` 复用同一条连接标记已读。

## 错误码

| code | HTTP | 含义 |
| --- | --- | --- |
| `invalid_request` | 400 | 参数不合法（含未知字段） |
| `unauthorized` | 401 | 令牌缺失/无效/过期，或口令错误 |
| `forbidden` | 403 | 非成员访问、跨站 WebSocket |
| `invalid_avatar` | 400 | 头像格式或尺寸不合规 |
| `avatar_too_large` | 413 | 头像超过 256 KB |
| `not_found` | 404 | 资源不存在 |
| `conflict` | 409 | 用户名或绑定冲突 |
| `rate_limited` | 429 | 触发限流，看 `Retry-After` |
| `internal_error` | 500 | 服务内部错误 |

## 指标

```bash
curl localhost:9080/metrics
```

暴露 `t2c_online_users`、`t2c_connections`、`t2c_messages_total` 等，Prometheus 文本格式，无第三方依赖。
