# 端到端测试

三层验证，从协议到像素。

## 1. API 端到端（Python，无依赖）

```bash
./run_api_e2e.sh          # 默认端口 18080
./run_api_e2e.sh 19090    # 指定端口
```

自己起一个干净实例、跑完清理。覆盖 42 项：注册/登录/越权/幂等/分页/
搜索注入/控制字符/XSS 载荷/频道生命周期等。

## 2. 浏览器端到端（Node，需 CDP）

需要一个开启远程调试的 Chrome：

```bash
chromium --headless=new --remote-debugging-port=9222 --no-sandbox --user-data-dir=/tmp/cp

# 另起一个干净实例
T2C_DATA_DIR=$(mktemp -d) T2C_ADDR=:18081 ./t2c &

T2C_URL=http://localhost:18081 node ui_test.mjs    # 32 项：单用户全流程
T2C_URL=http://localhost:18083 node ui_multi.mjs   # 10 项：双端实时互通
```

`ui_test.mjs` 覆盖注册、发消息、XSS 防护、刷新恢复、撤回、搜索、
移动端抽屉与遮罩、登出、错误提示。

`ui_multi.mjs` 用两个独立标签页模拟两个用户，验证：
A 发消息 B 不刷新就能收到、未读自动清除、B 回复 A 实时可见、
撤回事件双端同步。

## 3. Go 测试

```bash
go test ./... -race
```

单元 + 集成测试，包含真实 WebSocket 握手与并发注册竞争。
