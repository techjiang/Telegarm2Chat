'use strict';
/**
 * Telegarm2Chat 前端：零依赖单页应用。
 * 安全约定：所有用户数据只经 textContent 写入 DOM，绝不拼接 innerHTML。
 */
(() => {
  // ---------------------------------------------------------------- DOM 小工具
  const $ = (sel, root = document) => root.querySelector(sel);

  /** el('div', {class:'x', onclick:fn}, child...) */
  function el(tag, props, ...children) {
    const node = document.createElement(tag);
    if (props) {
      for (const [k, v] of Object.entries(props)) {
        if (v === null || v === undefined || v === false) continue;
        if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
        else if (k === 'class') node.className = v;
        else if (k === 'text') node.textContent = String(v);
        else if (k === 'dataset') Object.assign(node.dataset, v);
        else node.setAttribute(k, v === true ? '' : String(v));
      }
    }
    for (const c of children.flat()) {
      if (c === null || c === undefined || c === false) continue;
      node.append(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    return node;
  }

  const icon = (d, viewBox = '0 0 24 24') => {
    const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    s.setAttribute('viewBox', viewBox);
    s.setAttribute('aria-hidden', 'true');
    const p = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    p.setAttribute('d', d);
    s.append(p);
    return s;
  };

  const ICONS = {
    plus: 'M11 5h2v6h6v2h-6v6h-2v-6H5v-2h6z',
    search: 'M10 4a6 6 0 1 0 3.6 10.8l4.3 4.3 1.4-1.4-4.3-4.3A6 6 0 0 0 10 4zm0 2a4 4 0 1 1 0 8 4 4 0 0 1 0-8z',
    send: 'M3 20.5 21 3.5l-2.6 15-6.2-4.2-3 3.1.4-5.6z',
    menu: 'M4 7h16v2H4zm0 5h16v2H4zm0 5h16v2H4z',
    gear: 'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8zm9 4c0 .6-.1 1.2-.2 1.8l2 1.5-2 3.4-2.4-1a7.6 7.6 0 0 1-3 1.7L15 22h-6l-.4-2.6a7.6 7.6 0 0 1-3-1.7l-2.4 1-2-3.4 2-1.5A8.4 8.4 0 0 1 3 12c0-.6.1-1.2.2-1.8l-2-1.5 2-3.4 2.4 1a7.6 7.6 0 0 1 3-1.7L9 2h6l.4 2.6a7.6 7.6 0 0 1 3 1.7l2.4-1 2 3.4-2 1.5c.1.6.2 1.2.2 1.8z',
    exit: 'M16 13v-2H7V8l-5 4 5 4v-3zM20 3h-8v2h8v14h-8v2h8a2 2 0 0 0 2-2V5a2 2 0 0 0-2-2z',
  };

  // ---------------------------------------------------------------- 状态
  const state = {
    token: localStorage.getItem('t2c.token') || '',
    me: null,
    meta: null,
    chats: [],
    activeChat: null,
    messages: new Map(),      // chatId -> Message[]
    pending: new Map(),       // clientId -> {chatId, body, node}
    unsub: null,
    ws: null,
    wsRetry: 0,
    online: false,
    oldestLoaded: new Map(),  // chatId -> 最旧消息 id
    hasMore: new Map(),       // chatId -> bool
    lastActivity: new Map(),  // chatId -> 最后活动时间
    sending: false,
  };

  // ---------------------------------------------------------------- API
  async function api(method, path, body) {
    const headers = { Accept: 'application/json' };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    if (state.token) headers.Authorization = `Bearer ${state.token}`;
    const res = await fetch(`/api/v1${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'same-origin',
    });
    let payload = null;
    try { payload = await res.json(); } catch { /* 非 JSON 响应 */ }
    if (!res.ok || (payload && payload.ok === false)) {
      const msg = (payload && payload.error && payload.error.message) || `请求失败（${res.status}）`;
      const err = new Error(msg);
      err.status = res.status;
      err.code = payload && payload.error && payload.error.code;
      throw err;
    }
    return payload ? payload.data : null;
  }

  // ---------------------------------------------------------------- 通知
  function toast(text, kind) {
    let wrap = $('.toast-wrap');
    if (!wrap) {
      wrap = el('div', { class: 'toast-wrap', role: 'status', 'aria-live': 'polite' });
      document.body.append(wrap);
    }
    const t = el('div', { class: `toast ${kind === 'err' ? 'err' : ''}`, text });
    wrap.append(t);
    setTimeout(() => t.remove(), 3200);
  }

  // ---------------------------------------------------------------- 时间
  const timeFmt = new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit' });
  const dayFmt = new Intl.DateTimeFormat('zh-CN', { month: 'numeric', day: 'numeric' });
  function fmtTime(ts) {
    const d = new Date(ts * 1000);
    const now = new Date();
    const sameDay = d.toDateString() === now.toDateString();
    if (sameDay) return timeFmt.format(d);
    const yday = new Date(now.getTime() - 86400000).toDateString() === d.toDateString();
    if (yday) return `昨天 ${timeFmt.format(d)}`;
    if (d.getFullYear() === now.getFullYear()) return dayFmt.format(d);
    return `${d.getFullYear()}/${dayFmt.format(d)}`;
  }

  const initial = (name) => (name || '?').trim().charAt(0).toUpperCase() || '?';
  const AUTHOR_RE = /@([a-z0-9_-]{3,32})/g;

  /** 把纯文本渲染成含 @提及高亮的片段，全程 textContent 语义。 */
  function renderText(parent, text, myUsername) {
    let last = 0;
    for (const m of text.matchAll(AUTHOR_RE)) {
      if (m.index > last) parent.append(text.slice(last, m.index));
      const name = m[1];
      const isMe = myUsername && name === myUsername;
      parent.append(el('span', { class: 'who-name' + (isMe ? '' : ''), text: '@' + name }));
      last = m.index + m[0].length;
    }
    if (last < text.length) parent.append(text.slice(last));
  }

  // ---------------------------------------------------------------- 认证界面
  function renderAuth(mode = 'login') {
    const root = $('#app');
    root.className = '';
    root.replaceChildren();

    const isRegister = mode === 'register';
    const alertBox = el('div', { class: 'alert hidden', role: 'alert' });

    const username = el('input', {
      id: 'f-user', type: 'text', autocomplete: 'username', required: true,
      placeholder: '小写字母、数字、_ 或 -', minlength: '3', maxlength: '32',
    });
    const displayName = el('input', {
      id: 'f-name', type: 'text', autocomplete: 'nickname', maxlength: '32', placeholder: '显示给别人看的名字',
    });
    const password = el('input', {
      id: 'f-pass', type: 'password', autocomplete: isRegister ? 'new-password' : 'current-password',
      required: true, minlength: '10', maxlength: '256', placeholder: '至少 10 位',
    });
    const invite = el('input', { id: 'f-invite', type: 'text', autocomplete: 'off', placeholder: '邀请码' });

    const submit = el('button', { class: 'btn', type: 'submit', text: isRegister ? '创建账号' : '登录' });

    const form = el('form', { novalidate: true },
      alertBox,
      el('div', { class: 'field' }, el('label', { for: 'f-user', text: '用户名' }), username),
      isRegister ? el('div', { class: 'field' }, el('label', { for: 'f-name', text: '昵称（可选）' }), displayName) : null,
      el('div', { class: 'field' }, el('label', { for: 'f-pass', text: '密码' }), password),
      isRegister && state.meta && state.meta.inviteRequired
        ? el('div', { class: 'field' }, el('label', { for: 'f-invite', text: '邀请码' }), invite)
        : null,
      submit,
    );

    const showAlert = (msg) => { alertBox.textContent = msg; alertBox.classList.remove('hidden'); };

    form.addEventListener('submit', async (ev) => {
      ev.preventDefault();
      alertBox.classList.add('hidden');
      if (!username.value.trim() || !password.value) { showAlert('请填写用户名和密码'); return; }
      submit.disabled = true;
      submit.textContent = isRegister ? '创建中…' : '登录中…';
      try {
        const path = isRegister ? '/auth/register' : '/auth/login';
        const body = isRegister
          ? {
              username: username.value.trim(),
              displayName: displayName.value.trim(),
              password: password.value,
              inviteCode: invite.value.trim(),
            }
          : { username: username.value.trim(), password: password.value };
        const data = await api('POST', path, body);
        state.token = data.token;
        state.me = data.user;
        localStorage.setItem('t2c.token', data.token);
        await enterApp();
      } catch (err) {
        showAlert(err.message);
        submit.disabled = false;
        submit.textContent = isRegister ? '创建账号' : '登录';
        if (isRegister && err.code === 'invite_required') {
          invite.closest('.field')?.classList.remove('hidden');
        }
      }
    });

    const card = el('div', { class: 'auth-card' },
      el('h1', { text: isRegister ? '创建账号' : 'Telegarm2Chat' }),
      el('p', { class: 'sub', text: isRegister
        ? (state.meta && state.meta.firstRun ? '你是第一个用户，将自动成为管理员。' : '加入这台服务器开始聊天。')
        : '输入凭据以继续。' }),
      form,
      el('div', { class: 'switch' },
        isRegister ? '已有账号？' : '还没有账号？',
        ' ',
        el('a', {
          text: isRegister ? '去登录' : '去注册',
          onclick: () => renderAuth(isRegister ? 'login' : 'register'),
        }),
      ),
    );

    root.append(el('div', { class: 'auth-wrap' }, card));
    username.focus();
  }

  // ---------------------------------------------------------------- 主界面
  async function enterApp() {
    disconnectWS();
    state.messages.clear();
    state.oldestLoaded.clear();
    state.hasMore.clear();

    const root = $('#app');
    root.className = '';
    root.replaceChildren();

    const connDot = el('span', { class: 'dot', title: '连接状态' });
    const whoName = el('span', { text: state.me.displayName || state.me.username });
    const searchInput = el('input', {
      type: 'search', placeholder: '搜索消息…', 'aria-label': '搜索消息',
      onkeydown: (ev) => { if (ev.key === 'Enter') openSearch(searchInput.value); },
    });
    const chatListEl = el('div', { class: 'chat-list', role: 'list' });
    const sidebar = el('aside', { class: 'sidebar' },
      el('div', { class: 'side-top' },
        el('div', { class: 'brand' },
          icon(ICONS.send),
          el('span', { class: 'name', text: 'Telegarm2Chat' }),
          el('span', { class: 'status', title: '连接状态' }, connDot),
        ),
        el('div', { class: 'me-row', title: state.me.displayName || state.me.username },
          el('span', { class: 'me-label', text: '登录为' }),
          whoName,
        ),
        el('div', { class: 'search-row' },
          searchInput,
          el('button', {
            class: 'icon-btn', title: '新建频道', 'aria-label': '新建频道',
            onclick: openCreateChat,
          }, icon(ICONS.plus)),
          el('button', {
            class: 'icon-btn', title: '设置', 'aria-label': '设置', onclick: openSettings,
          }, icon(ICONS.gear)),
        ),
      ),
      chatListEl,
    );

    const titleEl = el('h2', { text: '选择一个会话' });
    const subEl = el('div', { class: 'sub', text: '' });
    const slugChip = el('span', { class: 'slug-chip hidden' });
    const actionsEl = el('div', { class: 'actions' });
    const menuBtn = el('button', {
      class: 'icon-btn mobile-toggle', 'aria-label': '打开会话列表',
      onclick: () => toggleDrawer(),
    }, icon(ICONS.menu));
    const panelHead = el('header', { class: 'panel-head' },
      menuBtn,
      el('div', { class: 'titles' }, titleEl, subEl),
      slugChip,
      actionsEl,
    );

    const messagesEl = el('div', { class: 'messages', role: 'log', 'aria-live': 'polite' });
    const input = el('textarea', {
      id: 'input', rows: '1', placeholder: '写点什么…', 'aria-label': '消息输入框',
      oninput: autoGrow, onkeydown: onInputKey,
    });
    const sendBtn = el('button', {
      class: 'send-btn', title: '发送 (Enter)', 'aria-label': '发送消息', type: 'button',
      onclick: () => sendCurrent(),
    }, icon(ICONS.send));
    const composer = el('div', { class: 'composer' },
      el('div', { class: 'composer-inner' }, input, sendBtn),
      el('div', { class: 'composer-hint' },
        el('span', {}, el('kbd', { text: 'Enter' }), ' 发送'),
        el('span', {}, el('kbd', { text: 'Shift+Enter' }), ' 换行'),
      ),
    );
    composer.classList.add('hidden');

    const panel = el('main', { class: 'panel' }, panelHead, messagesEl, composer);
    const scrim = el('button', {
      class: 'scrim', 'aria-label': '关闭会话列表', tabindex: '-1',
      onclick: () => closeDrawer(),
    });
    const shell = el('div', { class: 'shell' }, sidebar, scrim, panel);
    root.append(shell);

    state.ui = {
      shell, connDot, chatListEl, messagesEl, titleEl, subEl,
      slugChip, actionsEl, composer, input, sendBtn, panel,
    };
    // 点击会话后收起抽屉
    chatListEl.addEventListener('click', () => closeDrawer());

    await Promise.all([loadChats(), refreshMe()]);
    connectWS();
    renderAuthlessHint();
  }

  function renderAuthlessHint() {
    if (state.chats.length === 0) {
      const hint = el('div', { class: 'empty-hint' },
        '还没有会话。点右上角 + 新建频道，或让朋友把频道号发给你。');
      state.ui.chatListEl.append(hint);
    }
  }

  async function refreshMe() {
    try { state.me = await api('GET', '/me'); } catch { /* 忽略 */ }
  }

  // ---------------------------------------------------------------- 会话列表
  async function loadChats() {
    try {
      const data = await api('GET', '/chats');
      state.chats = data.chats || [];
      state.chats.forEach((c) => state.lastActivity.set(c.id, c.lastActiveAt || c.createdAt));
      renderChats();
    } catch (err) {
      if (err.status === 401) return renderAuth('login');
      toast(err.message, 'err');
    }
  }

  function renderChats() {
    const list = state.ui.chatListEl;
    const scroll = list.scrollTop;
    list.replaceChildren();
    const sorted = [...state.chats].sort((a, b) =>
      (state.lastActivity.get(b.id) || 0) - (state.lastActivity.get(a.id) || 0));
    if (sorted.length === 0) {
      list.append(el('div', { class: 'empty-hint', text: '还没有会话' }));
      return;
    }
    for (const c of sorted) {
      const unread = c.unread || 0;
      const row = el('div', {
        class: 'chat-item' + (state.activeChat && state.activeChat.id === c.id ? ' active' : ''),
        role: 'listitem', tabindex: '0',
        onclick: () => openChat(c.id),
        onkeydown: (ev) => { if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); openChat(c.id); } },
      },
        el('div', { class: 'avatar', text: initial(c.title) }),
        el('div', { class: 'chat-meta' },
          el('div', { class: 'chat-title', text: c.title }),
          el('div', { class: 'chat-preview', text: c.lastMessage || (c.kind === 'direct' ? '私聊' : '暂无消息') }),
        ),
        unread > 0 ? el('div', { class: 'unread', text: unread > 99 ? '99+' : String(unread) }) : null,
      );
      list.append(row);
    }
    list.scrollTop = scroll;
  }

  function upsertChat(chat) {
    const idx = state.chats.findIndex((c) => c.id === chat.id);
    if (idx >= 0) state.chats[idx] = { ...state.chats[idx], ...chat, unread: state.chats[idx].unread };
    else state.chats.push(chat);
    renderChats();
  }

  // ---------------------------------------------------------------- 打开会话
  async function openChat(chatId) {
    const chat = state.chats.find((c) => c.id === chatId);
    if (!chat) return;
    state.activeChat = chat;
    closeDrawer();
    renderChats();

    const ui = state.ui;
    ui.titleEl.textContent = chat.title;
    ui.subEl.textContent = chat.kind === 'direct' ? '私聊' : (chat.topic || '频道');
    ui.slugChip.textContent = chat.slug;
    ui.slugChip.classList.remove('hidden');
    ui.slugChip.onclick = async () => {
      try {
        await navigator.clipboard.writeText(chat.slug);
        toast('频道号已复制');
      } catch { toast(chat.slug); }
    };
    ui.actionsEl.replaceChildren(
      el('button', {
        class: 'icon-btn', title: '成员', 'aria-label': '成员', onclick: () => openMembers(chat),
      }, icon(ICONS.gear)),
      el('button', {
        class: 'icon-btn', title: '退出频道', 'aria-label': '退出频道',
        onclick: () => leaveChat(chat),
      }, icon(ICONS.exit)),
    );

    ui.composer.classList.remove('hidden');
    ui.messagesEl.replaceChildren();

    const cached = state.messages.get(chatId);
    if (cached && cached.length) {
      renderMessages(chatId);
    } else {
      await loadMessages(chatId, true);
    }
    scrollToBottom(false);
    ui.input.focus();

    if (chat.unread) {
      const last = (state.messages.get(chatId) || []).at(-1);
      chat.unread = 0;
      renderChats();
      if (last) markRead(chatId, last.id);
    }
  }

  function toggleDrawer() {
    const shell = state.ui && state.ui.shell;
    if (!shell) return;
    const open = !shell.classList.contains('drawer-open');
    setDrawer(open);
  }

  // 抽屉状态要同时落在 shell（遮罩）与 sidebar（滑入）上：
  // 只加一个类，移动端点菜单就只剩一层灰。
  function setDrawer(open) {
    const shell = state.ui && state.ui.shell;
    if (!shell) return;
    shell.classList.toggle('drawer-open', open);
    const sidebar = shell.querySelector('.sidebar');
    if (sidebar) sidebar.classList.toggle('open', open);
    document.body.style.overflow = open ? 'hidden' : '';
  }

  function closeDrawer() {
    setDrawer(false);
  }

  const sidebarClose = closeDrawer;

  async function loadMessages(chatId, initial) {
    const ui = state.ui;
    const before = initial ? 0 : state.oldestLoaded.get(chatId) || 0;
    try {
      const data = await api('GET', `/chats/${chatId}/messages?limit=40${before ? `&before=${before}` : ''}`);
      const msgs = data.messages || [];
      const existing = state.messages.get(chatId) || [];
      const merged = initial ? msgs : [...msgs, ...existing];
      // 去重按 id，保证重连后不出现重复行。
      const seen = new Set();
      const clean = merged.filter((m) => (seen.has(m.id) ? false : seen.add(m.id)));
      state.messages.set(chatId, clean);
      if (clean.length) state.oldestLoaded.set(chatId, clean[0].id);
      state.hasMore.set(chatId, msgs.length === 40);
      if (state.activeChat && state.activeChat.id === chatId) renderMessages(chatId);
    } catch (err) {
      if (err.status === 401) return renderAuth('login');
      toast(err.message, 'err');
    }
  }

  // ---------------------------------------------------------------- 消息渲染
  function renderMessages(chatId) {
    const ui = state.ui;
    const container = ui.messagesEl;
    const msgs = state.messages.get(chatId) || [];
    const pending = [...state.pending.values()].filter((p) => p.chatId === chatId);
    const atBottom = isNearBottom();

    container.replaceChildren();

    if (state.hasMore.get(chatId)) {
      container.append(el('button', {
        class: 'btn ghost load-more', text: '加载更早的消息',
        onclick: async (ev) => {
          ev.currentTarget.disabled = true;
          await loadMessages(chatId, false);
        },
      }));
    }

    let prev = null;
    for (const m of msgs) {
      const cont = prev && prev.senderId === m.senderId && prev.source === m.source && m.createdAt - prev.createdAt < 300;
      container.append(renderMessage(m, cont, false));
      prev = m;
    }
    for (const p of pending) {
      container.append(el('div', { class: 'msg pending', dataset: { clientId: p.clientId } },
        el('div', { class: 'avatar av', text: initial(state.me.displayName || state.me.username) }),
        el('div', { class: 'msg-body' },
          el('div', { class: 'msg-head' },
            el('span', { class: 'name', text: '我' }),
            el('span', { class: 'time', text: '发送中…' }),
          ),
          el('p', { class: 'text', text: p.body }),
        ),
      ));
    }
    if (msgs.length === 0 && pending.length === 0) {
      container.append(el('div', { class: 'empty-hint', text: '这里还很安静，发出第一条消息吧。' }));
    }
    if (atBottom) scrollToBottom(false);
  }

  function renderMessage(m, compact, optimistic) {
    if (m.source === 'system') {
      return el('div', { class: 'system-line', text: m.body });
    }
    const isMe = state.me && m.senderId === state.me.id;
    const head = compact ? null : el('div', { class: 'msg-head' },
      el('span', { class: 'name', text: isMe ? '我' : m.senderName }),
      m.source === 'telegram' ? el('span', { class: 'badge-tg', text: 'TG' }) : null,
      el('span', { class: 'time', text: fmtTime(m.createdAt) }),
    );
    const textNode = el('p', { class: 'text' });
    renderText(textNode, m.body, state.me && state.me.username);

    const actions = isMe || (state.me && state.me.isAdmin)
      ? el('div', { class: 'msg-actions' },
          el('button', { text: '撤回', onclick: () => deleteMessage(m) }),
        )
      : null;

    return el('div', {
      class: 'msg' + (compact ? ' cont' : '') + (optimistic ? ' pending' : ''),
      dataset: { id: String(m.id) },
    },
      el('div', { class: 'avatar av', text: initial(isMe ? (state.me.displayName || state.me.username) : m.senderName) }),
      el('div', { class: 'msg-body' }, head, textNode, actions),
    );
  }

  function isNearBottom() {
    const c = state.ui && state.ui.messagesEl;
    if (!c) return true;
    return c.scrollHeight - c.scrollTop - c.clientHeight < 120;
  }

  function scrollToBottom(smooth = false) {
    const c = state.ui && state.ui.messagesEl;
    if (!c) return;
    c.scrollTo({ top: c.scrollHeight, behavior: smooth ? 'smooth' : 'auto' });
  }

  // ---------------------------------------------------------------- 发送
  function autoGrow() {
    const t = state.ui.input;
    t.style.height = 'auto';
    t.style.height = Math.min(t.scrollHeight, 160) + 'px';
  }

  function onInputKey(ev) {
    if (ev.key === 'Enter' && !ev.shiftKey && !ev.isComposing) {
      ev.preventDefault();
      sendCurrent();
    }
  }

  const newClientID = () =>
    (crypto.randomUUID ? crypto.randomUUID() : `c-${Date.now()}-${Math.random().toString(36).slice(2)}`);

  async function sendCurrent() {
    const chat = state.activeChat;
    const input = state.ui.input;
    if (!chat || state.sending) return;
    const body = input.value.trim();
    if (!body) return;
    if (body.length > 4000) { toast('消息过长（上限 4000 字）', 'err'); return; }

    state.sending = true;
    state.ui.sendBtn.disabled = true;
    const clientId = newClientID();
    state.pending.set(clientId, { chatId: chat.id, body, clientId, at: Date.now() });
    input.value = '';
    autoGrow();
    renderMessages(chat.id);
    scrollToBottom(true);

    try {
      const res = await api('POST', `/chats/${chat.id}/messages`, { body, clientId });
      const m = res.message;
      state.pending.delete(clientId);
      const list = state.messages.get(chat.id) || [];
      if (!list.some((x) => x.id === m.id)) list.push(m);
      state.messages.set(chat.id, list);
      state.lastActivity.set(chat.id, m.createdAt);
      const c = state.chats.find((x) => x.id === chat.id);
      if (c) { c.lastMessage = m.body; }
      state.unreadSuppress = (state.unreadSuppress || new Set()).add(m.id);
      renderChats();
      renderMessages(chat.id);
      scrollToBottom(true);
      markRead(chat.id, m.id);
    } catch (err) {
      const p = state.pending.get(clientId);
      state.pending.delete(clientId);
      if (p) {
        state.ui.input.value = body;
        autoGrow();
      }
      renderMessages(chat.id);
      if (err.status === 401) return renderAuth('login');
      toast(err.message, 'err');
    } finally {
      state.sending = false;
      state.ui.sendBtn.disabled = false;
      state.ui.input.focus();
    }
  }

  async function deleteMessage(m) {
    if (!state.activeChat) return;
    try {
      await api('DELETE', `/chats/${state.activeChat.id}/messages/${m.id}`);
      const list = state.messages.get(state.activeChat.id) || [];
      state.messages.set(state.activeChat.id, list.filter((x) => x.id !== m.id));
      renderMessages(state.activeChat.id);
    } catch (err) {
      toast(err.message, 'err');
    }
  }

  async function markRead(chatId, messageId) {
    try { await api('POST', `/chats/${chatId}/read`, { messageId }); } catch { /* 非关键 */ }
  }

  // ---------------------------------------------------------------- WebSocket
  function connectWS() {
    if (!state.token) return;
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    const url = `${proto}://${location.host}/api/v1/ws?token=${encodeURIComponent(state.token)}`;
    let ws;
    try { ws = new WebSocket(url); } catch { scheduleReconnect(); return; }
    state.ws = ws;

    ws.onopen = () => {
      state.wsRetry = 0;
      state.online = true;
      updateConnDot();
    };
    ws.onmessage = (ev) => {
      let payload;
      try { payload = JSON.parse(ev.data); } catch { return; }
      handleEvent(payload);
    };
    ws.onclose = () => {
      state.online = false;
      updateConnDot();
      if (state.ws === ws) scheduleReconnect();
    };
    ws.onerror = () => { /* onclose 会跟上 */ };
  }

  function scheduleReconnect() {
    state.wsRetry += 1;
    const delay = Math.min(30000, 800 * 2 ** Math.min(state.wsRetry, 5));
    setTimeout(async () => {
      if (!state.token) return;
      // 重连前校验令牌，避免过期 token 死循环。
      try { await api('GET', '/me'); } catch (err) {
        if (err.status === 401) { logout(); return; }
      }
      connectWS();
    }, delay);
  }

  function updateConnDot() {
    const dot = state.ui && state.ui.connDot;
    if (!dot) return;
    dot.className = 'dot ' + (state.online ? 'on' : 'off');
    dot.title = state.online ? '已连接' : '连接中断，正在重连';
  }

  function disconnectWS() {
    if (state.ws) {
      const ws = state.ws;
      state.ws = null;
      ws.onclose = null;
      try { ws.close(); } catch { /* 忽略 */ }
    }
  }

  function handleEvent(ev) {
    switch (ev.type) {
      case 'ready':
        break;
      case 'message': {
        const m = ev.data;
        const list = state.messages.get(m.chatId) || [];
        if (!list.some((x) => x.id === m.id)) {
          list.push(m);
          list.sort((a, b) => a.id - b.id);
          state.messages.set(m.chatId, list);
        }
        state.lastActivity.set(m.chatId, m.createdAt || Math.floor(Date.now() / 1000));
        const chat = state.chats.find((c) => c.id === m.chatId);
        if (chat) {
          chat.lastMessage = m.body;
          const mine = state.me && m.senderId === state.me.id;
          if (!mine && (!state.activeChat || state.activeChat.id !== m.chatId)) {
            chat.unread = (chat.unread || 0) + 1;
          }
        }
        const active = state.activeChat && state.activeChat.id === m.chatId;
        renderChats();
        if (active) {
          const stick = isNearBottom();
          renderMessages(m.chatId);
          if (stick) scrollToBottom(true);
          if (document.hasFocus() && m.senderId !== (state.me && state.me.id)) markRead(m.chatId, m.id);
        } else if (chat && m.source === 'telegram') {
          toast(`${m.senderName}：${m.body.slice(0, 40)}`);
        }
        break;
      }
      case 'message.deleted': {
        const list = state.messages.get(ev.chatId) || [];
        state.messages.set(ev.chatId, list.filter((x) => x.id !== ev.data.id));
        if (state.activeChat && state.activeChat.id === ev.chatId) renderMessages(ev.chatId);
        break;
      }
      case 'chat':
        upsertChat(ev.data);
        break;
      case 'read':
        break;
      default:
        break;
    }
  }

  // ---------------------------------------------------------------- 弹层
  function modal(title, sub, contentNode, footerNode) {
    const bg = el('div', { class: 'modal-bg', role: 'dialog', 'aria-modal': 'true' });
    const close = () => { bg.remove(); document.removeEventListener('keydown', onKey); };
    const onKey = (ev) => { if (ev.key === 'Escape') close(); };
    document.addEventListener('keydown', onKey);
    bg.addEventListener('click', (ev) => { if (ev.target === bg) close(); });
    bg.append(el('div', { class: 'modal' },
      el('h3', { text: title }),
      sub ? el('p', { class: 'sub', text: sub }) : null,
      contentNode,
      footerNode || null,
    ));
    document.body.append(bg);
    return close;
  }

  function openCreateChat() {
    const channelTitle = el('input', { placeholder: '例如：产品讨论', maxlength: '80', 'aria-label': '频道名' });
    const topic = el('input', { placeholder: '一句话说明（可选）', maxlength: '240', 'aria-label': '频道说明' });
    const dmUser = el('input', { placeholder: '对方的用户名，如 alice', 'aria-label': '私聊对象' });
    const alertBox = el('div', { class: 'alert hidden' });

    const close = modal('新建会话', '频道可以分享给多人，私聊只有你们两人。',
      el('div', {},
        alertBox,
        el('div', { class: 'field' }, el('label', { text: '频道名' }), channelTitle),
        el('div', { class: 'field' }, el('label', { text: '说明' }), topic),
        el('div', { class: 'field' }, el('label', { text: '或发起私聊' }), dmUser),
      ),
      el('div', { class: 'row' },
        el('button', { class: 'btn ghost', text: '取消', onclick: () => close() }),
        el('button', {
          class: 'btn', text: '创建',
          onclick: async (ev) => {
            const btn = ev.currentTarget;
            alertBox.classList.add('hidden');
            btn.disabled = true;
            try {
              if (dmUser.value.trim()) {
                const c = await api('POST', '/direct', { username: dmUser.value.trim() });
                close();
                upsertChat(c);
                openChat(c.id);
                return;
              }
              if (!channelTitle.value.trim()) throw new Error('请填写频道名');
              const c = await api('POST', '/chats', { title: channelTitle.value.trim(), topic: topic.value.trim() });
              close();
              upsertChat({ ...c, unread: 0, lastMessage: '' });
              openChat(c.id);
              toast(`频道已创建，频道号 ${c.slug}`);
            } catch (err) {
              alertBox.textContent = err.message;
              alertBox.classList.remove('hidden');
              btn.disabled = false;
            }
          },
        }),
      ));
    channelTitle.focus();
    return close;
  }

  async function openSearch(query) {
    if (!query || query.trim().length < 2) { toast('至少输入 2 个字符'); return; }
    const results = el('div', { class: 'result-list' }, el('div', { class: 'empty-hint', text: '搜索中…' }));
    const close = modal('搜索', `关键词：${query.trim()}`, results);
    try {
      const data = await api('GET', `/search?q=${encodeURIComponent(query.trim())}`);
      const hits = data.hits || [];
      results.replaceChildren();
      if (!hits.length) {
        results.append(el('div', { class: 'empty-hint', text: '没有找到匹配的消息' }));
        return;
      }
      for (const h of hits) {
        const snippet = el('div', { class: 's' });
        highlight(snippet, h.body, query.trim());
        results.append(el('div', {
          class: 'result-item',
          onclick: async () => {
            close();
            if (!state.chats.some((c) => c.id === h.chatId)) {
              await loadChats();
            }
            await openChat(h.chatId);
            setTimeout(() => {
              const node = state.ui.messagesEl.querySelector(`[data-id="${h.id}"]`);
              node?.scrollIntoView({ block: 'center', behavior: 'smooth' });
              node?.animate?.([{ background: 'rgba(240,180,41,.28)' }, { background: 'transparent' }], { duration: 1600 });
            }, 260);
          },
        },
          el('div', { class: 't', text: `${h.chatTitle} · ${h.senderName} · ${fmtTime(h.createdAt)}` }),
          snippet,
        ));
      }
    } catch (err) {
      results.replaceChildren(el('div', { class: 'empty-hint', text: err.message }));
    }
  }

  /** 高亮命中词，只用 textContent / mark 节点，杜绝注入。 */
  function highlight(parent, text, needle) {
    const lower = text.toLowerCase();
    const n = needle.toLowerCase();
    let i = 0;
    while (true) {
      const at = lower.indexOf(n, i);
      if (at < 0) { parent.append(text.slice(i)); return; }
      if (at > i) parent.append(text.slice(i, at));
      parent.append(el('mark', { text: text.slice(at, at + n.length) }));
      i = at + n.length;
    }
  }

  async function openMembers(chat) {
    const list = el('div', { class: 'result-list' }, el('div', { class: 'empty-hint', text: '加载中…' }));
    const inviteRow = el('div', { class: 'field' },
      el('label', { text: '邀请他人加入' }),
      el('input', { placeholder: '用户名', 'aria-label': '要邀请的用户名', id: 'invite-user' }),
    );
    const tgRow = el('div', { class: 'field' },
      el('label', { text: '绑定 Telegram 群 ID（-100… 格式）' }),
      el('input', { placeholder: '例如 -1001234567890', 'aria-label': 'Telegram 群 ID', id: 'tg-id' }),
    );
    const close = modal(`关于「${chat.title}」`,
      chat.kind === 'direct' ? '私聊成员' : `频道号 ${chat.slug}`,
      el('div', {}, list, chat.kind === 'channel' ? inviteRow : null, chat.kind === 'channel' ? tgRow : null),
      el('div', { class: 'row' },
        el('button', {
          class: 'btn', text: '保存',
          onclick: async (ev) => {
            const btn = ev.currentTarget;
            btn.disabled = true;
            try {
              const inviteUser = $('#invite-user');
              if (inviteUser && inviteUser.value.trim()) {
                const u = inviteUser.value.trim();
                await api('POST', '/direct', { username: u });
                toast('已为你们创建私聊');
              }
              const tg = $('#tg-id');
              if (tg && tg.value.trim()) {
                await api('POST', `/chats/${chat.id}/telegram`, { tgChatId: tg.value.trim() });
                toast('Telegram 绑定成功');
              }
              close();
              await loadChats();
            } catch (err) {
              toast(err.message, 'err');
              btn.disabled = false;
            }
          },
        }),
      ));

    try {
      const data = await api('GET', `/chats/${chat.id}/members`);
      list.replaceChildren();
      for (const u of data.members || []) {
        list.append(el('div', { class: 'result-item' },
          el('div', { class: 't', text: (u.displayName || u.username) + (u.id === state.me.id ? '（我）' : '') }),
          el('div', { class: 's', text: '@' + u.username + (u.isAdmin ? ' · 管理员' : '') }),
        ));
      }
    } catch (err) {
      list.replaceChildren(el('div', { class: 'empty-hint', text: err.message }));
    }
  }

  async function leaveChat(chat) {
    if (chat.kind === 'direct') {
      toast('私聊不能退出，可以直接忽略');
      return;
    }
    const close = modal('退出频道', `确认退出「${chat.title}」？之后可用频道号重新加入。`,
      el('div', {}, el('div', { class: 'result-item' }, el('div', { class: 't', text: chat.slug }))),
      el('div', { class: 'row' },
        el('button', { class: 'btn ghost', text: '取消', onclick: () => close() }),
        el('button', {
          class: 'btn', text: '确认退出',
          onclick: async () => {
            try {
              await api('POST', `/chats/${chat.id}/leave`);
              state.chats = state.chats.filter((c) => c.id !== chat.id);
              state.messages.delete(chat.id);
              if (state.activeChat && state.activeChat.id === chat.id) {
                state.activeChat = null;
                state.ui.messagesEl.replaceChildren();
                state.ui.titleEl.textContent = '选择一个会话';
                state.ui.subEl.textContent = '';
                state.ui.slugChip.classList.add('hidden');
                state.ui.composer.classList.add('hidden');
              }
              close();
              renderChats();
            } catch (err) { toast(err.message, 'err'); }
          },
        }),
      ));
  }

  async function openSettings() {
    let stats = null;
    try { stats = await api('GET', '/stats'); } catch { /* 忽略 */ }
    const rows = stats ? [
      ['在线用户', String(stats.online)],
      ['消息总数', String(stats.store.messages)],
      ['会话数', String(stats.store.chats)],
      ['待发 Telegram', String(stats.store.pendingTelegram)],
      ['运行时长', `${Math.floor(stats.uptimeSeconds / 60)} 分钟`],
      ['版本', state.meta ? state.meta.version : '-'],
    ] : [];
    const list = el('div', { class: 'result-list' });
    for (const [k, v] of rows) {
      list.append(el('div', { class: 'result-item' },
        el('div', { class: 't', text: k }), el('div', { class: 's', text: v })));
    }
    const close = modal(`设置 · ${state.me.displayName || state.me.username}`,
      `@${state.me.username}${state.me.isAdmin ? ' · 管理员' : ''}`,
      el('div', {}, list),
      el('div', { class: 'row' },
        el('button', { class: 'btn ghost', text: '关闭', onclick: () => close() }),
        el('button', {
          class: 'btn', text: '退出登录',
          onclick: () => { close(); logout(); },
        }),
      ));
  }

  function logout() {
    api('POST', '/auth/logout').catch(() => {});
    state.token = '';
    state.me = null;
    state.chats = [];
    state.activeChat = null;
    state.messages.clear();
    localStorage.removeItem('t2c.token');
    disconnectWS();
    renderAuth('login');
  }

  // ---------------------------------------------------------------- 启动
  async function boot() {
    try {
      state.meta = await api('GET', '/meta');
    } catch (err) {
      // meta 失败不阻塞，给个可用界面。
      state.meta = { firstRun: false, telegramEnabled: false, inviteRequired: false, version: 'dev' };
    }
    if (!state.token) {
      return renderAuth(state.meta && state.meta.firstRun ? 'register' : 'login');
    }
    try {
      state.me = await api('GET', '/me');
      await enterApp();
    } catch (err) {
      if (err.status === 401) {
        state.token = '';
        localStorage.removeItem('t2c.token');
        renderAuth('login');
      } else {
        renderAuth('login');
        toast(err.message, 'err');
      }
    }
  }

  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible' && state.token && !state.online) {
      connectWS();
    }
  });

  boot();
})();
