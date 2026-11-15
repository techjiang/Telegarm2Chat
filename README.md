<div align="center">

# Telegarm2Chat

**一个自托管的实时聊天服务，把 Web 端与 Telegram 群双向打通。**

Go 单二进制 · SQLite 持久化 · WebSocket 实时 · 零前端构建

</div>

---

## 它能做什么

- **实时聊天**：频道（多人）与私聊，WebSocket 推送，断线自动重连并补齐消息
- **Telegram 双向同步**：绑定一个 Telegram 群后，两边消息互通，TG 内失败自动退避重试
- **全文搜索**：FTS5 索引 + 子串回退，中文英文都能搜；结果严格限制在你加入的会话内
- **多端同步**：同一账号多设备在线，已读状态与撤回事件实时广播
- **开箱即用**：第一个注册的账号自动成为管理员，无需配置即可启动

## 快速开始

```bash
# 直接跑（数据落在 ./data）
go run ./cmd/t2c

# 或者用 Docker
docker run -d -p 8080:8080 -v t2c-data:/data -e T2C_DATA_DIR=/data ghcr.io/your-org/t2c:latest
```

打开 <http://localhost:8080>，第一次访问会引导你创建管理员账号。

### 接入 Telegram（可选）

1. 找 [@BotFather](https://t.me/BotFather) 创建 Bot，拿到 token
2. 把 Bot 拉进目标群，并设为管理员（否则收不到群消息）
3. 启动时带上 token：`T2C_TELEGRAM_TOKEN=123456:ABC-DEF... go run ./cmd/t2c`
4. 在 Web 端频道详情里填入群的 `chat_id`（超级群通常是 `-100` 开头）

## 配置

全部走环境变量，不写配置文件。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `T2C_ADDR` | `:8080` | 监听地址 |
| `T2C_DATA_DIR` | `./data` | 数据目录（数据库与密钥） |
| `T2C_DB_PATH` | `$DATA_DIR/t2c.db` | 数据库路径，可覆盖 |
| `T2C_SECRET` | 自动生成 | 签发令牌的密钥，不设则持久化到 `data/secret.key` |
| `T2C_TOKEN_TTL_HOURS` | `168` | 登录有效期（小时） |
| `T2C_MAX_BODY_KB` | `256` | 单请求体上限（1..8192 KiB） |
| `T2C_RATE_LIMIT_PER_MIN` | `240` | 单 IP 每分钟请求数，0 表示关闭 |
| `T2C_ALLOWED_ORIGINS` | 空（同源） | 允许的跨域来源，逗号分隔 |
| `T2C_INVITE_CODE` | 空 | 设置后注册需邀请码 |
| `T2C_TELEGRAM_TOKEN` | 空 | 填了就启用 TG 桥接 |
| `T2C_TELEGRAM_API_BASE` | `https://api.telegram.org` | 可指向自建 Bot API |
| `T2C_TRUST_PROXY` | `1` | 是否信任 `X-Forwarded-For` |
| `T2C_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

指标端点默认在 **主端口 + 1000**（如 `:8080` → `127.0.0.1:9080`），只监听回环。

## 安全设计

这不是"能跑就行"的玩具，几个关键决定：

- **口令**：argon2id（64MB / t=2 / p=4），PHC 格式存储；用户不存在时也跑一次等量运算，抹平用户名枚举的时间差
- **会话**：HMAC-SHA256 签名令牌，无状态、可水平扩展；密钥不入库、文件权限 0600
- **注入**：FTS5 查询串白名单化，`LIKE` 通配符转义；前端全程 `textContent`/`createElement`，不拼 `innerHTML`
- **越权**：每个读写都校验成员关系；推送按 `userID` 定向，不广播到无关连接（有专门的测试盯这条）
- **WebSocket**：校验 `Origin`、限制读帧体积、25s 心跳、慢消费者主动断开
- **传输**：CSP、`X-Frame-Options: DENY`、`nosniff`、HSTS（HTTPS 下）；篡改 `ResponseWriter` 的中间件必须转发 `Hijack`，否则握手会 501
- **限流**：按 IP 的令牌桶，带闲置桶回收，避免内存随 IP 数膨胀
- **审计**：登录、注册、绑定、删除都留痕

## 项目结构

```
cmd/t2c/            启动入口：装配依赖、优雅关闭
internal/
  api/              HTTP 路由、中间件、WebSocket 处理器
  auth/             argon2id 口令与 HMAC 令牌
  chat/             业务编排：权限、提及解析、推送、桥接挂载
  config/           环境变量配置
  metrics/          Prometheus 文本格式导出
  realtime/         连接中枢与定向推送
  store/            SQLite 持久化（WAL + FTS5）
  telegram/         Bot API 客户端与双向桥
web/                内嵌单页前端（无构建步骤）
```

## 开发

```bash
go test ./...                # 全部测试
go test ./... -race          # 竞态检测（需要 CGO）
go test ./... -cover         # 覆盖率
go build -o t2c ./cmd/t2c
```

测试分成三层：存储/鉴权的单元测试、服务与 API 的集成测试（含真实 WebSocket 握手），
以及浏览器端到端（注册、发消息、双端实时、撤回同步、XSS 防护、移动端布局）。

## 已知边界

- 单机设计：SQLite + 进程内推送中枢，没有做多副本一致性
- 消息为纯文本；图片/文件暂未支持
- Telegram 桥只同步文本消息，不支持编辑与删除的双向同步
- 无邮箱验证与找回密码：自托管场景下，管理员可重置数据库

## License

MIT
