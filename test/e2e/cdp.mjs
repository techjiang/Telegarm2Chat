// 极简 CDP 客户端：驱动 headless Chrome 做端到端 UI 测试。
const BASE = process.env.CDP || 'http://localhost:9222';

async function connect(fresh = false) {
  const list = await (await fetch(`${BASE}/json/list`)).json();
  let target = fresh ? null : list.find(t => t.type === 'page' && t.url !== 'about:blank');
  if (!target && !fresh) target = list.find(t => t.type === 'page');
  if (!target || fresh) {
    target = await (await fetch(`${BASE}/json/new?about:blank`, { method: 'PUT' })).json();
  }
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  let id = 0;
  const pending = new Map();
  const events = [];
  ws.onmessage = (e) => {
    const msg = JSON.parse(e.data);
    if (msg.id && pending.has(msg.id)) {
      const { res, rej } = pending.get(msg.id);
      pending.delete(msg.id);
      msg.error ? rej(new Error(JSON.stringify(msg.error))) : res(msg.result);
    } else if (msg.method) {
      events.push(msg);
    }
  };
  const send = (method, params = {}) => new Promise((res, rej) => {
    const mid = ++id;
    pending.set(mid, { res, rej });
    ws.send(JSON.stringify({ id: mid, method, params }));
  });
  return { send, events, ws, targetId: target.id };
}

const sleep = ms => new Promise(r => setTimeout(r, ms));

async function evaluate(c, expr, awaitPromise = true) {
  const r = await c.send('Runtime.evaluate', {
    expression: expr, awaitPromise, returnByValue: true, userGesture: true,
  });
  if (r.exceptionDetails) {
    throw new Error('JS 异常: ' + JSON.stringify(r.exceptionDetails.exception?.description || r.exceptionDetails));
  }
  return r.result.value;
}

async function navigate(c, url) {
  await c.send('Page.enable');
  await c.send('Runtime.enable');
  // 清空历史事件，否则会误命中上一次导航的 load
  c.events.length = 0;
  const done = new Promise(res => {
    const t = setTimeout(res, 15000);
    const iv = setInterval(() => {
      if (c.events.some(e => e.method === 'Page.loadEventFired')) {
        clearInterval(iv); clearTimeout(t); res();
      }
    }, 30);
  });
  await c.send('Page.navigate', { url });
  await done;
  await sleep(350);
}

async function shot(c, path) {
  const r = await c.send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: false });
  const fs = await import('node:fs');
  fs.writeFileSync(path, Buffer.from(r.data, 'base64'));
}

async function setViewport(c, w, h, mobile = false) {
  await c.send('Emulation.setDeviceMetricsOverride', {
    width: w, height: h, deviceScaleFactor: 2, mobile,
  });
}

export { connect, evaluate, navigate, shot, sleep, setViewport };
