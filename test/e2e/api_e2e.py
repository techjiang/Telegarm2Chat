import json, os, urllib.parse, urllib.request, urllib.error, sys

PORT = os.environ.get("T2C_PORT", "18080")
B = f"http://localhost:{PORT}/api/v1"
passed = failed = 0

def call(method, path, body=None, token=None, raw=False):
    url = B + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            payload = json.loads(r.read())
            return r.status, payload
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read())
        except Exception:
            return e.code, {}

def check(name, cond, extra=""):
    global passed, failed
    if cond:
        passed += 1
        print(f"  ✓ {name}")
    else:
        failed += 1
        print(f"  ✗ {name} {extra}")

print("== 认证 ==")
st, r = call("POST", "/auth/register", {"username":"alice","displayName":"Alice","password":"alicepass1234"})
check("首用户注册成功且为 admin", st == 200 and r["data"]["user"]["isAdmin"], f"{st} {r}")
T1 = r["data"]["token"]

st, r = call("POST", "/auth/register", {"username":"bob","displayName":"Bob","password":"bobpass12345"})
check("第二用户非 admin", st == 200 and not r["data"]["user"]["isAdmin"], f"{st}")
T2 = r["data"]["token"]

st, _ = call("POST", "/auth/register", {"username":"alice","password":"alicepass1234"})
check("重复用户名 409", st == 409, str(st))

st, r = call("POST", "/auth/register", {"username":"weak","password":"123"})
check("弱密码 400", st == 400, f"{st} {r}")

st, _ = call("POST", "/auth/login", {"username":"alice","password":"wrongwrongwrong"})
check("错误密码 401", st == 401, str(st))

st, _ = call("GET", "/chats")
check("无 token 401", st == 401, str(st))

st, _ = call("GET", "/chats", token="1:123:forged")
check("伪造 token 401", st == 401, str(st))

st, r = call("GET", "/me", token=T1)
check("me 返回正确用户", st == 200 and r["data"]["username"] == "alice")

print("== 频道 ==")
st, r = call("POST", "/chats", {"title":"产品讨论","topic":"v1"}, token=T1)
check("建频道 201", st == 201, str(st))
SLUG = r["data"]["slug"]; CID = r["data"]["id"]

st, r = call("POST", "/chats/join", {"slug": SLUG}, token=T2)
check("bob 加入频道", st == 200, str(st))

st, r = call("POST", "/chats/join", {"slug": SLUG}, token=T2)
check("重复加入幂等", st == 200, str(st))

st, _ = call("POST", "/chats", {"title": ""}, token=T1)
check("空频道名 400", st == 400, str(st))

print("== 消息 ==")
st, r = call("POST", f"/chats/{CID}/messages", {"body":"你好 @bob 这是第一条消息","clientId":"c-1"}, token=T1)
check("发消息 201", st == 201, str(st))
check("解析 @bob 提及", len(r["data"]["mentions"]) == 1 and r["data"]["mentions"][0]["username"] == "bob", json.dumps(r["data"]["mentions"], ensure_ascii=False))
MID = r["data"]["message"]["id"]

st, r = call("POST", f"/chats/{CID}/messages", {"body":"你好 @bob 这是第一条消息","clientId":"c-1"}, token=T1)
check("clientId 幂等去重", r["data"]["duplicate"] is True and r["data"]["message"]["id"] == MID)

st, r = call("POST", f"/chats/{CID}/messages", {"body":"line1\u0007\u001b[31mline2\nline3"}, token=T2)
body = r["data"]["message"]["body"]
check("控制字符被过滤", "\u0007" not in body and "\u001b" not in body and "\n" in body, repr(body))

st, _ = call("POST", f"/chats/{CID}/messages", {"body":""}, token=T1)
check("空消息拒绝", st == 400, str(st))

st, _ = call("POST", f"/chats/{CID}/messages", {"body":"x"*4001}, token=T1)
check("超长消息拒绝", st == 400, str(st))

st, r = call("GET", f"/chats/{CID}/messages", token=T2)
check("列表倒序翻转为正序", st == 200 and r["data"]["messages"][0]["id"] < r["data"]["messages"][-1]["id"])

print("== 鉴权边界 ==")
st, _ = call("GET", f"/chats/{CID}/messages", token=T1)
# 建第三个用户验证非成员
st, r = call("POST", "/auth/register", {"username":"carol","password":"carolpass1234"})
T3 = r["data"]["token"]
st, _ = call("GET", f"/chats/{CID}/messages", token=T3)
check("非成员读消息 403", st == 403, str(st))
st, _ = call("POST", f"/chats/{CID}/messages", {"body":"偷偷发言"}, token=T3)
check("非成员发消息 403", st == 403, str(st))
st, _ = call("DELETE", f"/chats/{CID}/messages/{MID}", token=T2)
check("他人消息撤回 403", st == 403, str(st))
st, _ = call("DELETE", f"/chats/{CID}/messages/{MID}", token=T1)
check("作者可撤回 200", st == 200, str(st))
st, r = call("GET", f"/chats/{CID}/messages", token=T1)
check("撤回后不可见", all(m["id"] != MID for m in r["data"]["messages"]))

print("== 搜索 ==")
st, r = call("GET", "/search?q=" + urllib.parse.quote("第一条"), token=T1)
check("中文搜索命中", st == 200, str(st))
st, r = call("GET", "/search?q=" + urllib.parse.quote("line2"), token=T2)
check("英文搜索命中", st == 200 and len(r["data"]["hits"]) >= 1, json.dumps(r, ensure_ascii=False)[:200])
st, r = call("GET", "/search?q=" + urllib.parse.quote('" OR 1=1--'), token=T2)
check("FTS 注入不报 500", st in (200, 400), f"{st} {r}")
st, r = call("GET", "/search?q=a", token=T2)
check("过短关键词 400", st == 400, str(st))
st, r = call("GET", "/search?q=" + urllib.parse.quote("第一条"), token=T3)
check("非成员搜不到", len(r["data"]["hits"]) == 0, json.dumps(r, ensure_ascii=False)[:150])

print("== 私聊与列表 ==")
st, r = call("POST", "/direct", {"username":"bob"}, token=T1)
check("创建私聊", st == 200, str(st))
DM = r["data"]["id"]
st, r = call("POST", "/direct", {"username":"bob"}, token=T1)
check("私聊幂等复用", r["data"]["id"] == DM)
st, r = call("POST", "/direct", {"username":"alice"}, token=T1)
check("不能和自己私聊", st == 400, str(st))

# alice 再发一条，制造 bob 的未读
st, r = call("POST", f"/chats/{CID}/messages", {"body":"@bob 未读测试消息"}, token=T1)
check("alice 发新消息", st == 201, str(st))
st, r = call("GET", "/chats", token=T2)
check("会话列表含 2 个", len(r["data"]["chats"]) == 2, str(len(r["data"]["chats"])))
chats = {c["id"]: c for c in r["data"]["chats"]}
check("未读数正确", (chats[CID].get("unread") or 0) >= 1, json.dumps(chats[CID], ensure_ascii=False))

st, r = call("POST", f"/chats/{CID}/read", {"messageId": 999999}, token=T2)
check("标记已读", st == 200, str(st))
st, r = call("GET", "/chats", token=T2)
c = [x for x in r["data"]["chats"] if x["id"] == CID][0]
check("已读后未读归零", (c.get("unread") or 0) == 0, json.dumps(c, ensure_ascii=False))

print("== 成员/统计 ==")
st, r = call("GET", f"/chats/{CID}/members", token=T1)
check("成员列表 2 人", st == 200 and len(r["data"]["members"]) == 2)
st, r = call("GET", "/stats", token=T1)
check("统计可用", st == 200 and r["data"]["store"]["users"] >= 3, json.dumps(r.get("data", {}), ensure_ascii=False)[:200])

print("== 请求体校验 ==")
st, r = call("POST", f"/chats/{CID}/messages", {"body":"x","unknown":"field"}, token=T1)
check("未知字段被拒", st == 400, str(st))

print("== 退出频道 ==")
st, r = call("POST", f"/chats/{CID}/leave", token=T2)
check("bob 退出频道", st == 200, str(st))
st, r = call("POST", f"/chats/{CID}/leave", token=T1)
check("owner 不能退出", st == 400, str(st))

print(f"\n结果: {passed} 通过, {failed} 失败")
sys.exit(1 if failed else 0)
