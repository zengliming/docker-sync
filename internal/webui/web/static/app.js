/* docker-sync web console — vanilla ES2020, no external dependencies */
(function () {
  'use strict';

  /* ================================================================== */
  /* 常量与文案                                                          */
  /* ================================================================== */

  const ROUTES = {
    dashboard: { title: '仪表盘', sub: '服务概览与最近运行' },
    tasks: { title: '任务', sub: '镜像同步任务管理' },
    runs: { title: '运行记录', sub: '同步与检查历史' },
    files: { title: '文件', sub: 'tar 产物下载与清理' },
    settings: { title: '设置', sub: '服务参数与仓库凭据' }
  };

  const PLATFORM_PRESETS = ['linux/amd64', 'linux/arm64', 'linux/arm/v7'];
  const PHASE_ORDER = ['queued', 'resolving', 'downloading', 'packing', 'uploading', 'done'];

  const TASK_STATUS = { idle: '空闲', queued: '排队中', running: '同步中', error: '错误', paused: '已暂停' };
  const TASK_STATUS_CLASS = { idle: 'badge-idle', queued: 'badge-warn', running: 'badge-run', error: 'badge-error', paused: 'badge-paused' };

  const RUN_STATUS = { running: '运行中', success: '成功', skipped: '已跳过', error: '失败', canceled: '已取消' };
  const RUN_STATUS_CLASS = { running: 'badge-run', success: 'badge-ok', skipped: 'badge-skip', error: 'badge-error', canceled: 'badge-idle' };

  const TRIGGER_TEXT = { manual: '手动', schedule: '定时', check: '检查', upload: '上传' };
  const PHASE_TEXT = { queued: '排队', resolving: '解析清单', downloading: '下载层', packing: '打包', uploading: '上传 SMB', done: '完成' };
  const LEVEL_TEXT = { info: 'INFO', warn: 'WARN', error: 'ERROR' };

  const ICONS = {
    task: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 8l-9-5-9 5 9 5 9-5z"/><path d="M3 8v8l9 5 9-5V8"/><path d="M12 13v8"/></svg>',
    run: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M10 8.5l6 3.5-6 3.5z"/></svg>',
    file: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="4" rx="1"/><path d="M5 8v11a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8"/><path d="M10 12h4"/></svg>',
    database: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v14c0 1.66 3.58 3 8 3s8-1.34 8-3V5"/><path d="M4 12c0 1.66 3.58 3 8 3s8-1.34 8-3"/></svg>',
    clock: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg>',
    alert: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3l9.5 17h-19z"/><path d="M12 9v5"/><path d="M12 17.5h.01"/></svg>',
    inbox: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 13l2.5-7h11L20 13"/><path d="M4 13v4a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-4"/><path d="M4 13h4l1.5 2h5L16 13h4"/></svg>',
    check: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20 6L9 17l-5-5"/></svg>',
    globe: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M3 12h18"/><path d="M12 3a15 15 0 0 1 0 18"/><path d="M12 3a15 15 0 0 0 0 18"/></svg>'
  };

  /* ================================================================== */
  /* 状态                                                                */
  /* ================================================================== */

  const state = {
    route: 'dashboard',
    health: null,
    healthError: null,
    sse: 'connecting',
    overview: null,
    tasks: null,
    runs: null,
    files: null,
    settings: null,
    settingsDraft: null,
    settingsDirty: false,
    proxyTest: null,
    loading: { overview: false, tasks: false, runs: false, files: false, settings: false },
    errors: { overview: null, tasks: null, runs: null, files: null, settings: null },
    runFilter: { taskId: '', status: '' },
    drawerRunId: null,
    drawerRun: null,
    drawerError: null,
    runPollTimer: null,
    taskModal: null,
    confirmResolve: null,
    eventSource: null
  };

  const loadTokens = Object.create(null);

  /* ================================================================== */
  /* 通用工具                                                            */
  /* ================================================================== */

  function esc(value) {
    const s = value === null || value === undefined ? '' : String(value);
    return s.replace(/[&<>"']/g, function (ch) {
      switch (ch) {
        case '&': return '&amp;';
        case '<': return '&lt;';
        case '>': return '&gt;';
        case '"': return '&quot;';
        default: return '&#39;';
      }
    });
  }

  function num(value) {
    const n = Number(value);
    return isFinite(n) ? n : 0;
  }

  function truncate(value, max) {
    const s = value === null || value === undefined ? '' : String(value);
    if (s.length <= max) return s;
    return s.slice(0, Math.max(0, max - 1)) + '…';
  }

  function pad2(n) {
    return (n < 10 ? '0' : '') + n;
  }

  function shortDigest(digest) {
    const s = digest === null || digest === undefined ? '' : String(digest);
    if (!s) return '—';
    return s.length > 12 ? s.slice(0, 12) + '…' : s;
  }

  function fmtBytes(bytes) {
    const n = Number(bytes);
    if (!isFinite(n) || n <= 0) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB'];
    let v = n;
    let i = 0;
    while (v >= 1024 && i < units.length - 1) {
      v = v / 1024;
      i += 1;
    }
    if (i === 0) return Math.round(v) + ' B';
    return v.toFixed(1) + ' ' + units[i];
  }

  function fmtUptime(seconds) {
    const s = Math.max(0, Math.floor(num(seconds)));
    if (s < 60) return s + ' 秒';
    const m = Math.floor(s / 60);
    if (m < 60) return m + ' 分钟';
    const h = Math.floor(m / 60);
    const mm = m % 60;
    if (h < 24) return mm ? h + ' 小时 ' + mm + ' 分' : h + ' 小时';
    const d = Math.floor(h / 24);
    const hh = h % 24;
    return hh ? d + ' 天 ' + hh + ' 小时' : d + ' 天';
  }

  function fmtDuration(startIso, endIso) {
    if (!startIso) return '—';
    const start = Date.parse(startIso);
    if (!isFinite(start)) return '—';
    const end = endIso ? Date.parse(endIso) : Date.now();
    if (!isFinite(end)) return '—';
    const sec = Math.max(0, (end - start) / 1000);
    if (sec < 60) {
      if (sec < 10) return sec.toFixed(1) + ' 秒';
      return Math.round(sec) + ' 秒';
    }
    const m = Math.floor(sec / 60);
    const s = Math.round(sec % 60);
    if (m < 60) return s ? m + ' 分 ' + s + ' 秒' : m + ' 分钟';
    const h = Math.floor(m / 60);
    const mm = m % 60;
    if (h < 24) return mm ? h + ' 小时 ' + mm + ' 分' : h + ' 小时';
    const d = Math.floor(h / 24);
    const hh = h % 24;
    return hh ? d + ' 天 ' + hh + ' 小时' : d + ' 天';
  }

  function fmtTime(iso) {
    if (!iso) return '—';
    const t = Date.parse(iso);
    if (!isFinite(t)) return '—';
    const d = new Date(t);
    return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) +
      ' ' + pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds());
  }

  function fmtClock(iso) {
    if (!iso) return '—';
    const t = Date.parse(iso);
    if (!isFinite(t)) return '—';
    const d = new Date(t);
    return pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds());
  }

  function fmtRelative(iso) {
    if (!iso) return '—';
    const t = Date.parse(iso);
    if (!isFinite(t)) return '—';
    const diff = Date.now() - t;
    const abs = Math.abs(diff);
    const future = diff < 0;
    const suffix = future ? '后' : '前';
    if (abs < 45 * 1000) return future ? '即将' : '刚刚';
    if (abs < 60 * 60 * 1000) return Math.max(1, Math.round(abs / 60000)) + ' 分钟' + suffix;
    if (abs < 24 * 60 * 60 * 1000) return Math.round(abs / 3600000) + ' 小时' + suffix;
    if (abs < 30 * 24 * 60 * 60 * 1000) return Math.round(abs / 86400000) + ' 天' + suffix;
    return fmtTime(iso);
  }

  function timeCell(iso, primary) {
    if (!iso) return '<span class="muted">—</span>';
    const abs = fmtTime(iso);
    const rel = fmtRelative(iso);
    const main = primary === 'rel' ? rel : abs;
    const sub = primary === 'rel' ? abs : rel;
    return '<span class="time-cell" title="' + esc(abs) + '">' +
      '<span class="time-main">' + esc(main) + '</span>' +
      '<span class="time-sub">' + esc(sub) + '</span>' +
      '</span>';
  }

  function digestCell(digest) {
    if (!digest) return '<span class="muted">—</span>';
    return '<span class="mono digest" title="' + esc(digest) + '">' + esc(shortDigest(digest)) + '</span>';
  }

  function fileUrlByName(name) {
    return '/api/files/' + encodeURIComponent(String(name || ''));
  }

  function fileUrl(file) {
    if (file && typeof file.url === 'string' && file.url.indexOf('/api/files/') === 0) {
      return file.url;
    }
    return fileUrlByName(file ? file.name : '');
  }

  function taskStatusBadge(status, title) {
    const cls = TASK_STATUS_CLASS[status] || 'badge-idle';
    return '<span class="badge ' + cls + '"' + (title ? ' title="' + esc(title) + '"' : '') + '>' +
      esc(TASK_STATUS[status] || status || '未知') + '</span>';
  }

  function runStatusBadge(status, title) {
    const cls = RUN_STATUS_CLASS[status] || 'badge-idle';
    return '<span class="badge ' + cls + '"' + (title ? ' title="' + esc(title) + '"' : '') + '>' +
      esc(RUN_STATUS[status] || status || '未知') + '</span>';
  }

  function triggerBadge(trigger) {
    return '<span class="badge badge-trigger">' + esc(TRIGGER_TEXT[trigger] || trigger || '—') + '</span>';
  }

  function btn(label, action, options) {
    const o = options || {};
    const cls = 'btn ' + (o.cls || 'btn-ghost') + (o.size === 'sm' ? ' btn-sm' : '');
    let attrs = ' type="button" class="' + cls + '" data-action="' + esc(action) + '"';
    if (o.data) {
      Object.keys(o.data).forEach(function (key) {
        attrs += ' data-' + key + '="' + esc(o.data[key]) + '"';
      });
    }
    if (o.title) attrs += ' title="' + esc(o.title) + '"';
    if (o.disabled) attrs += ' disabled';
    return '<button' + attrs + '>' + esc(label) + '</button>';
  }

  function pageHead(title, sub, actions) {
    return '<div class="page-head"><div><h2>' + esc(title) + '</h2>' +
      (sub ? '<p class="page-sub">' + esc(sub) + '</p>' : '') +
      '</div><div class="page-actions">' + (actions || '') + '</div></div>';
  }

  function statCard(label, value, sub, iconSvg, tone) {
    return '<div class="stat-card">' +
      '<div class="stat-icon' + (tone ? ' tone-' + tone : '') + '">' + iconSvg + '</div>' +
      '<div class="stat-body">' +
      '<div class="stat-label">' + esc(label) + '</div>' +
      '<div class="stat-value">' + esc(value) + '</div>' +
      (sub ? '<div class="stat-sub">' + esc(sub) + '</div>' : '') +
      '</div></div>';
  }

  function skeletonCards(count) {
    let items = '';
    for (let i = 0; i < count; i += 1) {
      items += '<div class="stat-card stat-skeleton">' +
        '<div class="skeleton skeleton-icon"></div>' +
        '<div class="stat-body">' +
        '<div class="skeleton skeleton-line" style="width:62%"></div>' +
        '<div class="skeleton skeleton-line" style="width:45%;height:18px"></div>' +
        '<div class="skeleton skeleton-line" style="width:52%"></div>' +
        '</div></div>';
    }
    return '<div class="stat-grid">' + items + '</div>';
  }

  function skeletonTable(cols, rows) {
    let head = '';
    for (let c = 0; c < cols; c += 1) {
      head += '<th><div class="skeleton skeleton-line" style="width:' + (56 + ((c * 13) % 34)) + '%"></div></th>';
    }
    let body = '';
    for (let r = 0; r < rows; r += 1) {
      body += '<tr>';
      for (let c = 0; c < cols; c += 1) {
        body += '<td><div class="skeleton skeleton-line" style="width:' + (46 + ((r * 7 + c * 11) % 42)) + '%"></div></td>';
      }
      body += '</tr>';
    }
    return '<div class="table-wrap"><table class="table skeleton-table"><thead><tr>' + head +
      '</tr></thead><tbody>' + body + '</tbody></table></div>';
  }

  function emptyState(title, hint, actionHtml) {
    return '<div class="empty">' +
      '<div class="empty-icon">' + ICONS.inbox + '</div>' +
      '<h3>' + esc(title) + '</h3>' +
      '<p>' + esc(hint) + '</p>' +
      (actionHtml ? '<div class="empty-action">' + actionHtml + '</div>' : '') +
      '</div>';
  }

  function errorCard(message, retryAction) {
    return '<div class="card"><div class="empty">' +
      '<div class="empty-icon">' + ICONS.alert + '</div>' +
      '<h3>数据加载失败</h3>' +
      '<p>' + esc(message || '无法获取数据，请确认服务已启动后重试。') + '</p>' +
      '<div class="empty-action"><button type="button" class="btn btn-ghost" data-action="' + esc(retryAction || 'refresh') + '">重试</button></div>' +
      '</div></div>';
  }

  function errorBanner(message) {
    return message ? '<div class="banner banner-error">刷新失败：' + esc(message) + '</div>' : '';
  }

  function toast(message, type) {
    const root = document.getElementById('toast-root');
    if (!root) return;
    const item = document.createElement('div');
    item.className = 'toast toast-' + (type || 'info');
    item.setAttribute('role', 'status');
    const text = document.createElement('span');
    text.textContent = message === null || message === undefined ? '' : String(message);
    item.appendChild(text);
    root.appendChild(item);
    const ttl = type === 'error' ? 6000 : 3600;
    window.setTimeout(function () {
      if (item.parentNode) item.parentNode.removeChild(item);
    }, ttl);
  }

  function setBusy(el, busy, text) {
    if (!el || !el.dataset) return;
    if (busy) {
      if (el.dataset.originalText === undefined) el.dataset.originalText = el.textContent;
      el.disabled = true;
      if (text) el.textContent = text;
    } else {
      el.disabled = false;
      if (el.dataset.originalText !== undefined) {
        el.textContent = el.dataset.originalText;
        delete el.dataset.originalText;
      }
    }
  }

  function focusField(id) {
    const el = document.getElementById(id);
    if (el && typeof el.focus === 'function') {
      try { el.focus(); } catch (err) { /* ignore */ }
    }
  }

  /* ================================================================== */
  /* API                                                                 */
  /* ================================================================== */

  async function api(path, options) {
    const opts = Object.assign({}, options || {});
    const headers = Object.assign({ Accept: 'application/json' }, opts.headers || {});
    if (opts.body !== undefined && opts.body !== null) {
      headers['Content-Type'] = 'application/json; charset=utf-8';
    }
    opts.headers = headers;

    let res;
    try {
      res = await fetch(path, opts);
    } catch (err) {
      throw new Error('无法连接到服务，请确认后端已启动（' + path + '）');
    }

    const contentType = res.headers.get('content-type') || '';
    let body = null;
    if (contentType.indexOf('application/json') !== -1) {
      try { body = await res.json(); } catch (err) { body = null; }
    }

    if (!res.ok) {
      const message = body && typeof body.error === 'string' && body.error
        ? body.error
        : '请求失败（HTTP ' + res.status + '）';
      const error = new Error(message);
      error.status = res.status;
      error.body = body;
      throw error;
    }
    return body;
  }

  /* ================================================================== */
  /* 数据加载                                                            */
  /* ================================================================== */

  function nextToken(key) {
    loadTokens[key] = (loadTokens[key] || 0) + 1;
    return loadTokens[key];
  }

  function isCurrent(key, token) {
    return loadTokens[key] === token;
  }

  function renderIfActive(route) {
    if (state.route === route) render();
  }

  async function loadHealth() {
    try {
      const data = await api('/api/health');
      state.health = data || null;
      state.healthError = data && data.status === 'ok' ? null : '服务状态异常';
    } catch (err) {
      state.health = null;
      state.healthError = err.message;
    }
    updateChrome();
  }

  async function loadOverview(silent) {
    const token = nextToken('overview');
    if (!silent) {
      state.loading.overview = true;
      renderIfActive('dashboard');
    }
    try {
      const data = await api('/api/overview');
      if (!isCurrent('overview', token)) return;
      state.overview = data || null;
      state.errors.overview = null;
    } catch (err) {
      if (!isCurrent('overview', token)) return;
      state.errors.overview = err.message;
    } finally {
      if (isCurrent('overview', token)) {
        state.loading.overview = false;
        renderIfActive('dashboard');
      }
    }
  }

  async function loadTasks(silent) {
    const token = nextToken('tasks');
    if (!silent) {
      state.loading.tasks = true;
      renderIfActive('tasks');
    }
    try {
      const data = await api('/api/tasks');
      if (!isCurrent('tasks', token)) return;
      state.tasks = data && Array.isArray(data.tasks) ? data.tasks : [];
      state.errors.tasks = null;
    } catch (err) {
      if (!isCurrent('tasks', token)) return;
      state.errors.tasks = err.message;
    } finally {
      if (isCurrent('tasks', token)) {
        state.loading.tasks = false;
        renderIfActive('tasks');
      }
    }
  }

  async function loadRuns(silent) {
    const token = nextToken('runs');
    if (!silent) {
      state.loading.runs = true;
      renderIfActive('runs');
    }
    try {
      const data = await api('/api/runs?limit=100');
      if (!isCurrent('runs', token)) return;
      state.runs = data && Array.isArray(data.runs) ? data.runs : [];
      state.errors.runs = null;
    } catch (err) {
      if (!isCurrent('runs', token)) return;
      state.errors.runs = err.message;
    } finally {
      if (isCurrent('runs', token)) {
        state.loading.runs = false;
        renderIfActive('runs');
      }
    }
  }

  async function loadFiles(silent) {
    const token = nextToken('files');
    if (!silent) {
      state.loading.files = true;
      renderIfActive('files');
    }
    try {
      const data = await api('/api/files');
      if (!isCurrent('files', token)) return;
      state.files = data && Array.isArray(data.files) ? data.files : [];
      state.errors.files = null;
    } catch (err) {
      if (!isCurrent('files', token)) return;
      state.errors.files = err.message;
    } finally {
      if (isCurrent('files', token)) {
        state.loading.files = false;
        renderIfActive('files');
      }
    }
  }

  function settingsDraftFrom(data) {
    const d = data || {};
    const registries = Array.isArray(d.registries) ? d.registries.map(function (r) {
      return {
        host: r && typeof r.host === 'string' ? r.host : '',
        username: r && typeof r.username === 'string' ? r.username : '',
        password: '',
        hasPassword: !!(r && r.hasPassword),
        insecure: !!(r && r.insecure),
        proxyUrl: r && typeof r.proxyUrl === 'string' ? r.proxyUrl : ''
      };
    }) : [];
    return {
      smb: Object.assign({ enabled: false, share: '', directory: '', username: '', password: '', hasPassword: false, domain: '' }, d.smb || {}, { password: '' }),
      defaultPlatform: typeof d.defaultPlatform === 'string' ? d.defaultPlatform : '',
      proxyUrl: typeof d.proxyUrl === 'string' ? d.proxyUrl : '',
      noProxy: typeof d.noProxy === 'string' ? d.noProxy : '',
      maxConcurrentRuns: Number.isFinite(Number(d.maxConcurrentRuns)) ? Number(d.maxConcurrentRuns) : 2,
      maxConcurrentDownloads: Number.isFinite(Number(d.maxConcurrentDownloads)) ? Number(d.maxConcurrentDownloads) : 4,
      blobCacheMaxGB: Number.isFinite(Number(d.blobCacheMaxGB)) ? Number(d.blobCacheMaxGB) : 20,
      blobCacheMaxAgeDays: Number.isFinite(Number(d.blobCacheMaxAgeDays)) ? Number(d.blobCacheMaxAgeDays) : 7,
      dataDir: typeof d.dataDir === 'string' ? d.dataDir : '',
      registries: registries
    };
  }

  async function loadSettings(silent, forceDraft) {
    const token = nextToken('settings');
    if (!silent) {
      state.loading.settings = true;
      renderIfActive('settings');
    }
    try {
      const data = await api('/api/settings');
      if (!isCurrent('settings', token)) return;
      state.settings = data || null;
      state.errors.settings = null;
      if (!state.settingsDraft || forceDraft || !state.settingsDirty) {
        state.settingsDraft = settingsDraftFrom(data);
      }
    } catch (err) {
      if (!isCurrent('settings', token)) return;
      state.errors.settings = err.message;
    } finally {
      if (isCurrent('settings', token)) {
        state.loading.settings = false;
        renderIfActive('settings');
      }
    }
  }

  async function loadRunDetail(id, silent) {
    if (!state.drawerRunId || state.drawerRunId !== id) return;
    try {
      const run = await api('/api/runs/' + encodeURIComponent(id));
      if (state.drawerRunId !== id) return;
      state.drawerRun = run || null;
      state.drawerError = null;
      renderDrawer();
      if (!run || run.status !== 'running') stopRunPolling();
    } catch (err) {
      if (state.drawerRunId !== id) return;
      if (!silent) {
        state.drawerError = err.message;
        renderDrawer();
      }
      if (err.status === 404) stopRunPolling();
    }
  }

  function loadRouteData(route, silent) {
    switch (route) {
      case 'dashboard': loadOverview(silent); break;
      case 'tasks': loadTasks(silent); break;
      case 'runs': loadRuns(silent); break;
      case 'files': loadFiles(silent); ensureSettingsForFiles(); break;
      case 'settings': loadSettings(silent, false); break;
      default: break;
    }
  }

  function routeHasData(route) {
    switch (route) {
      case 'dashboard': return !!state.overview;
      case 'tasks': return !!state.tasks;
      case 'runs': return !!state.runs;
      case 'files': return !!state.files;
      case 'settings': return !!state.settingsDraft;
      default: return false;
    }
  }

  async function refreshCurrent() {
    loadHealth();
    switch (state.route) {
      case 'dashboard': await loadOverview(false); break;
      case 'tasks': await loadTasks(false); break;
      case 'runs': await loadRuns(false); break;
      case 'files': await loadFiles(false); break;
      case 'settings': await loadSettings(false, false); break;
      default: break;
    }
  }

  /* ================================================================== */
  /* 页面：仪表盘                                                        */
  /* ================================================================== */

  function renderDashboard() {
    const actions = btn('新建任务', 'task-create', { cls: 'btn-primary' }) +
      btn('刷新', 'refresh', { cls: 'btn-ghost' });
    const head = pageHead('服务概览', '任务、缓存与最近运行一览', actions);

    if (!state.overview) {
      if (state.loading.overview) return head + skeletonCards(6) + skeletonTable(7, 5);
      return head + errorCard(state.errors.overview, 'refresh');
    }

    const o = state.overview;
    const recent = Array.isArray(o.recentRuns) ? o.recentRuns : [];

    const stats = '<div class="stat-grid">' + [
      statCard('任务总数', String(num(o.taskCount)), '启用 ' + num(o.enabledCount) + ' 个', ICONS.task, 'blue'),
      statCard('运行中', String(num(o.runningCount)), '正在同步的任务', ICONS.run, 'blue'),
      statCard('错误任务', String(num(o.errorCount)), '需要关注的失败任务', ICONS.alert, 'red'),
      statCard('tar 产物', String(num(o.tarCount)), fmtBytes(num(o.tarBytes)), ICONS.file, 'green'),
      statCard('blob 缓存', String(num(o.blobCount)), fmtBytes(num(o.blobBytes)), ICONS.database, 'amber'),
      statCard('运行时长', fmtUptime(num(o.uptimeSec)), '服务版本 ' + (o.version || '—'), ICONS.clock, 'purple')
    ].join('') + '</div>';

    const meta = '<div class="meta-strip">' +
      '<span>数据目录 <code>' + esc(o.dataDir || '—') + '</code></span>' +
      '<span>服务版本 <code>' + esc(o.version || '—') + '</code></span>' +
      '<span>运行时长 ' + esc(fmtUptime(num(o.uptimeSec))) + '</span>' +
      '</div>';

    const rows = recent.length ? recent.map(function (run) {
      return '<tr class="clickable" data-run-id="' + esc(run.id) + '">' +
        '<td><div class="cell-main">' + esc(run.taskName || '—') + '</div>' +
        '<div class="cell-sub mono" title="' + esc(run.image || '') + '">' + esc(run.image || '') + '</div></td>' +
        '<td>' + triggerBadge(run.trigger) + '</td>' +
        '<td>' + runStatusBadge(run.status, run.error) + '</td>' +
        '<td>' + esc(fmtDuration(run.startedAt, run.finishedAt)) + '</td>' +
        '<td>' + digestCell(run.remoteDigest || run.previousDigest || run.configDigest) + '</td>' +
        '<td>' + timeCell(run.startedAt) + '</td>' +
        '<td>' + btn('查看日志', 'run-open', { data: { id: run.id }, size: 'sm' }) + '</td>' +
        '</tr>';
    }).join('') : '<tr><td colspan="7">' +
      emptyState('暂无运行记录', '任务开始同步或检查后，运行记录会显示在这里。',
        '<a class="btn btn-ghost" href="#/tasks">前往任务</a>') + '</td></tr>';

    return head + errorBanner(state.errors.overview) + stats + meta +
      '<div class="section-head"><h3>最近运行</h3><a href="#/runs">查看全部</a></div>' +
      '<div class="table-wrap"><table class="table table-recent"><thead><tr>' +
      '<th>任务</th><th>触发方式</th><th>状态</th><th>耗时</th><th>Digest</th><th>开始时间</th><th>操作</th>' +
      '</tr></thead><tbody>' + rows + '</tbody></table></div>';
  }

  /* ================================================================== */
  /* 页面：任务                                                          */
  /* ================================================================== */

  function scheduleCell(task) {
    if (task.scheduleType === 'cron') {
      return task.cron
        ? '<code class="mono" title="' + esc(task.cron) + '">' + esc(task.cron) + '</code>'
        : '<span class="muted">—</span>';
    }
    const minutes = num(task.intervalMinutes) > 0 ? num(task.intervalMinutes) : 60;
    return '每 ' + esc(String(minutes)) + ' 分钟';
  }

  function updateCell(task) {
    if (!task.updateAvailable) return '<span class="muted">—</span>';
    return '<div class="update-flag">' +
      '<span class="badge badge-warn">有新版本</span>' +
      '<span class="mono digest" title="' + esc(task.remoteDigest || '') + '">' + esc(shortDigest(task.remoteDigest)) + '</span>' +
      '</div>';
  }

  function taskTimesCell(task) {
    const checked = task.lastCheckedAt
      ? '<div>检查 <span title="' + esc(fmtTime(task.lastCheckedAt)) + '">' + esc(fmtRelative(task.lastCheckedAt)) + '</span></div>'
      : '<div class="muted">检查 —</div>';
    const synced = task.lastSyncedAt
      ? '<div>同步 <span title="' + esc(fmtTime(task.lastSyncedAt)) + '">' + esc(fmtRelative(task.lastSyncedAt)) + '</span></div>'
      : '<div class="muted">同步 —</div>';
    return '<div class="stack">' + checked + synced + '</div>';
  }

  function latestTarCell(task) {
    if (!task.latestTar) return '<span class="muted">暂无</span>';
    return '<a class="link mono" href="' + esc(fileUrlByName(task.latestTar)) + '" download="' + esc(task.latestTar) + '" title="' + esc(task.latestTar) + '">' +
      esc(truncate(task.latestTar, 26)) + '</a>' +
      '<div class="cell-sub">' + esc(fmtBytes(task.latestTarSize)) + '</div>';
  }

  function taskActions(task) {
    const busy = task.status === 'running' || task.status === 'queued';
    return '<div class="row-actions">' +
      btn(busy ? '同步中' : '立即同步', 'task-sync', { data: { id: task.id }, cls: 'btn-primary', size: 'sm', disabled: busy }) +
      btn('检查更新', 'task-check', { data: { id: task.id }, size: 'sm' }) +
      btn(task.enabled ? '暂停' : '启用', 'task-toggle', { data: { id: task.id }, size: 'sm' }) +
      btn('编辑', 'task-edit', { data: { id: task.id }, size: 'sm' }) +
      btn('删除', 'task-delete', { data: { id: task.id }, cls: 'btn-danger', size: 'sm' }) +
      '</div>';
  }

  function taskRowHtml(task) {
    const errorLine = task.lastError
      ? '<div class="cell-sub error-text" title="' + esc(task.lastError) + '">' + esc(truncate(task.lastError, 30)) + '</div>'
      : '';
    return '<tr>' +
      '<td><div class="cell-main">' + esc(task.name || task.image || task.id) + '</div>' +
      '<div class="cell-sub mono" title="' + esc(task.image || '') + '">' + esc(task.image || '') + '</div></td>' +
      '<td><span class="mono">' + esc(task.platform || '默认') + '</span></td>' +
      '<td>' + scheduleCell(task) + '</td>' +
      '<td>' + taskStatusBadge(task.status, task.lastError) + errorLine + '</td>' +
      '<td>' + updateCell(task) + '</td>' +
      '<td>' + taskTimesCell(task) + '</td>' +
      '<td>' + latestTarCell(task) + '</td>' +
      '<td>' + taskActions(task) + '</td>' +
      '</tr>';
  }

  function renderTasks() {
    const actions = btn('新建任务', 'task-create', { cls: 'btn-primary' }) +
      btn('刷新', 'refresh', { cls: 'btn-ghost' });
    const head = pageHead('任务', '镜像同步任务的创建、调度与管理', actions);

    if (!state.tasks) {
      if (state.loading.tasks) return head + skeletonTable(8, 4);
      return head + errorCard(state.errors.tasks, 'refresh');
    }

    const tasks = state.tasks;
    const rows = tasks.length ? tasks.map(taskRowHtml).join('') :
      '<tr><td colspan="8">' + emptyState('还没有任务',
        '创建一个任务，按固定间隔或 cron 定时检查并同步镜像 tar。',
        btn('新建任务', 'task-create', { cls: 'btn-primary' })) + '</td></tr>';

    return head + errorBanner(state.errors.tasks) +
      '<div class="table-wrap"><table class="table table-tasks"><thead><tr>' +
      '<th>名称 / 镜像</th><th>平台</th><th>调度</th><th>状态</th><th>更新提示</th>' +
      '<th>最近检查 / 同步</th><th>最新 tar</th><th>操作</th>' +
      '</tr></thead><tbody>' + rows + '</tbody></table></div>';
  }

  /* ================================================================== */
  /* 页面：运行记录                                                      */
  /* ================================================================== */

  function renderRuns() {
    const head = pageHead('运行记录', '最近 100 条同步与检查记录', btn('刷新', 'refresh', { cls: 'btn-ghost' }));

    if (!state.runs) {
      if (state.loading.runs) return head + skeletonTable(9, 6);
      return head + errorCard(state.errors.runs, 'refresh');
    }

    const runs = state.runs;
    const taskNames = new Map();
    runs.forEach(function (run) {
      const id = run.taskId || '';
      if (!taskNames.has(id)) taskNames.set(id, run.taskName || id || '未知任务');
    });

    let taskOptions = '<option value="">全部任务</option>';
    taskNames.forEach(function (name, id) {
      taskOptions += '<option value="' + esc(id) + '"' + (state.runFilter.taskId === id ? ' selected' : '') + '>' +
        esc(name) + '</option>';
    });

    const statusList = [['', '全部状态'], ['running', '运行中'], ['success', '成功'], ['skipped', '已跳过'], ['error', '失败'], ['canceled', '已取消']];
    let statusOptions = '';
    statusList.forEach(function (pair) {
      statusOptions += '<option value="' + esc(pair[0]) + '"' + (state.runFilter.status === pair[0] ? ' selected' : '') + '>' +
        esc(pair[1]) + '</option>';
    });

    const filtered = runs.filter(function (run) {
      if (state.runFilter.taskId && run.taskId !== state.runFilter.taskId) return false;
      if (state.runFilter.status && run.status !== state.runFilter.status) return false;
      return true;
    });

    const toolbar = '<div class="toolbar">' +
      '<label class="toolbar-field" for="run-filter-task">任务<select id="run-filter-task" class="input">' + taskOptions + '</select></label>' +
      '<label class="toolbar-field" for="run-filter-status">状态<select id="run-filter-status" class="input">' + statusOptions + '</select></label>' +
      '<span class="toolbar-info">显示 ' + filtered.length + ' / ' + runs.length + ' 条</span>' +
      '</div>';

    let rows;
    if (!runs.length) {
      rows = '<tr><td colspan="9">' + emptyState('暂无运行记录',
        '任务开始同步或检查后，运行记录会显示在这里。',
        '<a class="btn btn-ghost" href="#/tasks">前往任务</a>') + '</td></tr>';
    } else if (!filtered.length) {
      rows = '<tr><td colspan="9">' + emptyState('没有符合筛选条件的记录', '尝试调整任务或状态筛选条件。') + '</td></tr>';
    } else {
      rows = filtered.map(function (run) {
        const updatedAt = run.finishedAt || run.startedAt;
        const updatedCell = timeCell(updatedAt, 'rel') +
          (run.status === 'running' ? ' <span class="live-mark" title="运行中"></span>' : '');
        const errorCell = run.error
          ? '<span class="err-cell" title="' + esc(run.error) + '">' + esc(truncate(run.error, 52)) + '</span>'
          : '<span class="muted">—</span>';
        return '<tr class="clickable" data-run-id="' + esc(run.id) + '">' +
          '<td><div class="cell-main">' + esc(run.taskName || '—') + '</div>' +
          '<div class="cell-sub mono">' + esc(run.id) + '</div></td>' +
          '<td>' + triggerBadge(run.trigger) + '</td>' +
          '<td>' + runStatusBadge(run.status, run.error) + '</td>' +
          '<td>' + esc(PHASE_TEXT[run.phase] || run.phase || '—') + '</td>' +
          '<td>' + esc(fmtDuration(run.startedAt, run.finishedAt)) + '</td>' +
          '<td>' + updatedCell + '</td>' +
          '<td>' + (num(run.tarSize) > 0 ? esc(fmtBytes(run.tarSize)) : '<span class="muted">—</span>') + '</td>' +
          '<td>' + errorCell + '</td>' +
          '<td>' + btn('查看日志', 'run-open', { data: { id: run.id }, size: 'sm' }) + '</td>' +
          '</tr>';
      }).join('');
    }

    return head + errorBanner(state.errors.runs) + toolbar +
      '<div class="table-wrap"><table class="table table-runs"><thead><tr>' +
      '<th>任务</th><th>触发</th><th>状态</th><th>阶段</th><th>耗时</th><th>更新时间</th><th>大小</th><th>错误信息</th><th>操作</th>' +
      '</tr></thead><tbody>' + rows + '</tbody></table></div>';
  }

  /* ================================================================== */
  /* 页面：运行详情抽屉                                                  */
  /* ================================================================== */

  function stepsHtml(run) {
    const finished = run.status === 'success' || run.status === 'skipped';
    const failed = run.status === 'error';
    const canceled = run.status === 'canceled';
    let current = PHASE_ORDER.indexOf(run.phase);
    if (current < 0) current = 0;

    const steps = PHASE_ORDER.map(function (phase, index) {
      let cls = 'step';
      if (finished) {
        cls += ' done';
      } else if (failed) {
        if (index < current) cls += ' done';
        else if (index === current) cls += ' error';
      } else if (canceled) {
        if (index < current) cls += ' done';
        else if (index === current) cls += ' canceled';
      } else {
        if (index < current) cls += ' done';
        else if (index === current) cls += ' active';
      }
      return '<div class="' + cls + '"><span class="step-dot"></span>' +
        '<span class="step-label">' + esc(PHASE_TEXT[phase] || phase) + '</span></div>';
    }).join('');

    return '<div class="steps">' + steps + '</div>';
  }

  function runDetailBody(run) {
    const updatedAt = run.finishedAt || run.startedAt;
    const kv = [
      { label: '任务', value: run.taskName || '—' },
      { label: '任务 ID', value: run.taskId || '—', mono: true },
      { label: '运行 ID', value: run.id || '—', mono: true },
      { label: '镜像', value: run.image || '—', mono: true },
      { label: '平台', value: run.platform || '—', mono: true },
      { label: '触发方式', value: TRIGGER_TEXT[run.trigger] || run.trigger || '—' },
      { label: '状态', value: RUN_STATUS[run.status] || run.status || '—' },
      { label: '阶段', value: PHASE_TEXT[run.phase] || run.phase || '—' },
      { label: '开始时间', value: fmtTime(run.startedAt) },
      { label: '结束时间', value: run.finishedAt ? fmtTime(run.finishedAt) : (run.status === 'running' ? '进行中' : '—') },
      { label: '耗时', value: fmtDuration(run.startedAt, run.finishedAt) },
      { label: 'tar 文件', value: run.tarName || '—', mono: true },
      { label: 'tar 大小', value: num(run.tarSize) > 0 ? fmtBytes(run.tarSize) : '—' },
      { label: '镜像层数', value: String(num(run.layers)) },
      { label: '下载字节', value: fmtBytes(run.bytesDownloaded) },
      { label: '复用 blob', value: String(num(run.blobsReused)) },
      { label: '发现新版本', value: run.updateAvailable ? '是' : '否' },
      { label: '远端 digest', value: run.remoteDigest || '—', mono: true },
      { label: '上次 digest', value: run.previousDigest || '—', mono: true },
      { label: '配置 digest', value: run.configDigest || '—', mono: true },
      { label: '错误', value: run.error || '无', error: !!run.error }
    ];

    const kvHtml = '<div class="kv-grid">' + kv.map(function (item) {
      let cls = 'kv-value';
      if (item.mono) cls += ' mono';
      if (item.error) cls += ' is-error';
      return '<div class="kv"><div class="kv-label">' + esc(item.label) + '</div>' +
        '<div class="' + cls + '">' + esc(item.value) + '</div></div>';
    }).join('') + '</div>';

    const logs = Array.isArray(run.logs) ? run.logs : [];
    const logsHtml = logs.length ? logs.map(function (line) {
      const level = line && line.level === 'error' ? 'error' : (line && line.level === 'warn' ? 'warn' : 'info');
      return '<div class="log-line log-' + level + '">' +
        '<span class="log-time">' + esc(fmtClock(line ? line.time : '')) + '</span>' +
        '<span class="log-level">' + esc(LEVEL_TEXT[level]) + '</span>' +
        '<span class="log-msg">' + esc(line ? line.message : '') + '</span>' +
        '</div>';
    }).join('') : '<div class="log-empty">暂无日志</div>';

    const runningHint = run.status === 'running'
      ? '<span class="muted">运行中 · 每 2 秒自动刷新</span>'
      : '<span class="muted">更新于 ' + esc(fmtRelative(updatedAt)) + '</span>';

    return '<div class="drawer-topline">' +
      runStatusBadge(run.status, run.error) + triggerBadge(run.trigger) +
      '<span class="badge badge-trigger">阶段：' + esc(PHASE_TEXT[run.phase] || run.phase || '—') + '</span>' +
      '</div>' +
      (run.error ? '<div class="banner banner-error">' + esc(run.error) + '</div>' : '') +
      '<div class="drawer-section"><h4>进度</h4>' + stepsHtml(run) + '</div>' +
      '<div class="drawer-section"><h4>详情</h4>' + kvHtml + '</div>' +
      '<div class="drawer-section">' +
      '<div class="drawer-section-head"><h4>日志</h4>' + runningHint + '</div>' +
      '<div class="log-list" id="run-log-list">' + logsHtml + '</div>' +
      '</div>';
  }

  function drawerHtml() {
    const run = state.drawerRun;
    const error = state.drawerError;

    let body;
    if (error && !run) {
      body = '<div class="empty"><div class="empty-icon">' + ICONS.alert + '</div>' +
        '<h3>运行详情加载失败</h3><p>' + esc(error) + '</p>' +
        '<div class="empty-action"><button type="button" class="btn btn-ghost" data-action="run-retry">重试</button></div></div>';
    } else if (!run) {
      body = '<div class="drawer-loading"><span class="spinner"></span><span>加载运行详情…</span></div>';
    } else {
      body = runDetailBody(run);
    }

    let download = '';
    if (run && run.tarName && num(run.tarSize) > 0) {
      download = '<a class="btn btn-sm btn-ghost" href="' + esc(fileUrlByName(run.tarName)) + '" download="' + esc(run.tarName) + '">下载 tar</a>';
    }

    return '<div class="drawer-backdrop" data-action="drawer-close"></div>' +
      '<aside class="drawer" role="dialog" aria-modal="true" aria-label="运行详情">' +
      '<header class="drawer-head">' +
      '<div><div class="drawer-eyebrow">运行详情</div>' +
      '<h3>' + esc(run ? (run.taskName || run.id) : '加载中…') + '</h3>' +
      (run ? '<div class="drawer-sub mono">' + esc(run.id) + '</div>' : '') +
      '</div>' +
      '<div class="drawer-head-actions">' + download +
      '<button type="button" class="btn btn-icon btn-ghost" data-action="drawer-close" aria-label="关闭">✕</button>' +
      '</div></header>' +
      '<div class="drawer-body">' + body + '</div>' +
      '</aside>';
  }

  function renderDrawer() {
    const root = document.getElementById('drawer-root');
    if (!root) return;
    if (!state.drawerRunId) {
      root.innerHTML = '';
      document.body.classList.remove('drawer-open');
      return;
    }

    const prevList = root.querySelector('.log-list');
    const nearBottom = prevList ? (prevList.scrollHeight - prevList.scrollTop - prevList.clientHeight < 40) : true;
    const prevTop = prevList ? prevList.scrollTop : 0;

    root.innerHTML = drawerHtml();
    document.body.classList.add('drawer-open');

    const list = document.getElementById('run-log-list');
    if (list) {
      if (nearBottom) list.scrollTop = list.scrollHeight;
      else list.scrollTop = prevTop;
    }
  }

  function openRunDrawer(id) {
    if (!id) return;
    state.drawerRunId = id;
    state.drawerRun = null;
    state.drawerError = null;
    renderDrawer();
    loadRunDetail(id, false);
    startRunPolling();
  }

  function closeDrawer() {
    stopRunPolling();
    state.drawerRunId = null;
    state.drawerRun = null;
    state.drawerError = null;
    renderDrawer();
  }

  function startRunPolling() {
    stopRunPolling();
    state.runPollTimer = window.setInterval(function () {
      if (state.drawerRunId) loadRunDetail(state.drawerRunId, true);
    }, 2000);
  }

  function stopRunPolling() {
    if (state.runPollTimer) {
      window.clearInterval(state.runPollTimer);
      state.runPollTimer = null;
    }
  }

  /* ================================================================== */
  /* 页面：文件                                                          */
  /* ================================================================== */

  function renderFiles() {
    const head = pageHead('文件', '数据目录 tars/ 下的镜像 tar 产物', btn('刷新', 'refresh', { cls: 'btn-ghost' }));

    if (!state.files) {
      if (state.loading.files) return head + skeletonTable(6, 4);
      return head + errorCard(state.errors.files, 'refresh');
    }

    const files = state.files;
    const totalSize = files.reduce(function (sum, file) { return sum + num(file.size); }, 0);

    const toolbar = '<div class="toolbar">' +
      '<span class="toolbar-info">共 ' + files.length + ' 个文件 · 总占用 <strong>' + esc(fmtBytes(totalSize)) + '</strong></span>' +
      '</div>';

    let rows;
    if (!files.length) {
      rows = '<tr><td colspan="6">' + emptyState('暂无 tar 文件',
        '任务完成同步后会在这里生成 tar 产物，可直接下载到本地 docker load。',
        '<a class="btn btn-ghost" href="#/tasks">前往任务</a>') + '</td></tr>';
    } else {
      rows = files.map(function (file) {
        const taskCell = file.taskName || file.taskId
          ? '<div class="cell-main">' + esc(file.taskName || '—') + '</div>' +
            (file.taskId ? '<div class="cell-sub mono">' + esc(file.taskId) + '</div>' : '')
          : '<span class="muted">—</span>';
        const imageCell = file.image
          ? '<span class="mono cell-sub" title="' + esc(file.image) + '">' + esc(file.image) + '</span>'
          : '<span class="muted">—</span>';
        return '<tr>' +
          '<td><span class="mono file-name" title="' + esc(file.name) + '">' + esc(file.name) + '</span>' +
          (file.digest ? '<div class="cell-sub mono" title="' + esc(file.digest) + '">' + esc(shortDigest(file.digest)) + '</div>' : '') +
          '</td>' +
          '<td>' + esc(fmtBytes(file.size)) + '</td>' +
          '<td>' + timeCell(file.modTime) + '</td>' +
          '<td>' + taskCell + '</td>' +
          '<td>' + imageCell + '</td>' +
          '<td><div class="row-actions">' +
          '<a class="btn btn-sm btn-ghost" href="' + esc(fileUrl(file)) + '" download="' + esc(file.name) + '">下载</a>' +
          btn('上传 SMB', 'file-smb', {
            data: { name: file.name },
            cls: 'btn-ghost',
            size: 'sm',
            title: smbReady() ? '把该 tar 复制到已配置的 SMB 共享' : '请先在设置页启用 SMB 远端存储',
            disabled: !smbReady()
          }) +
          btn('删除', 'file-delete', { data: { name: file.name }, cls: 'btn-danger', size: 'sm' }) +
          '</div></td>' +
          '</tr>';
      }).join('');
    }

    return head + errorBanner(state.errors.files) + toolbar +
      '<div class="table-wrap"><table class="table table-files"><thead><tr>' +
      '<th>文件名</th><th>大小</th><th>修改时间</th><th>关联任务</th><th>镜像</th><th>操作</th>' +
      '</tr></thead><tbody>' + rows + '</tbody></table></div>';
  }

  /* ================================================================== */
  /* 页面：设置                                                          */
  /* ================================================================== */

  function platformOptions(selected, emptyLabel) {
    let html = '<option value=""' + (selected === '' ? ' selected' : '') + '>' + esc(emptyLabel) + '</option>';
    PLATFORM_PRESETS.forEach(function (platform) {
      html += '<option value="' + esc(platform) + '"' + (selected === platform ? ' selected' : '') + '>' +
        esc(platform) + '</option>';
    });
    const custom = selected !== '' && PLATFORM_PRESETS.indexOf(selected) === -1;
    html += '<option value="__custom__"' + (custom ? ' selected' : '') + '>自定义…</option>';
    return html;
  }

  function settingNumberField(name, label, value, min, max, hint) {
    return '<div class="field"><label for="set-' + esc(name) + '">' + esc(label) + '</label>' +
      '<input id="set-' + esc(name) + '" name="' + esc(name) + '" type="number" class="input" min="' + min + '" max="' + max +
      '" step="1" value="' + esc(String(value)) + '">' +
      (hint ? '<span class="field-hint">' + esc(hint) + '</span>' : '') +
      '</div>';
  }

  function registryRowsHtml(registries) {
    if (!registries.length) {
      return '<tr><td colspan="6"><div class="empty empty-compact"><p>暂无仓库凭据。公共镜像无需凭据；私有仓库可添加 host、用户名与密码。</p></div></td></tr>';
    }
    return registries.map(function (reg, index) {
      return '<tr data-reg-row data-has-password="' + (reg.hasPassword ? 'true' : 'false') + '">' +
        '<td><input class="input mono" data-reg-field="host" data-reg-index="' + index + '" value="' + esc(reg.host || '') +
        '" placeholder="docker.io / registry.local:5000" aria-label="仓库地址"></td>' +
        '<td><input class="input" data-reg-field="username" data-reg-index="' + index + '" value="' + esc(reg.username || '') +
        '" placeholder="用户名" aria-label="用户名"></td>' +
        '<td><input class="input" type="password" autocomplete="new-password" data-reg-field="password" data-reg-index="' + index +
        '" value="' + esc(reg.password || '') + '" placeholder="' + (reg.hasPassword ? '已设置，留空保持不变' : '密码（可留空）') +
        '" aria-label="密码"></td>' +
        '<td><input class="input mono" data-reg-field="proxyUrl" data-reg-index="' + index +
        '" value="' + esc(reg.proxyUrl || '') + '" placeholder="留空=跟随全局，- =直连" aria-label="该仓库代理"></td>' +
        '<td class="center"><input type="checkbox" class="checkbox" data-reg-field="insecure" data-reg-index="' + index + '"' +
        (reg.insecure ? ' checked' : '') + ' aria-label="允许明文 HTTP"></td>' +
        '<td class="right">' + btn('移除', 'registry-remove', { data: { index: String(index) }, cls: 'btn-danger', size: 'sm' }) + '</td>' +
        '</tr>';
    }).join('');
  }

  function proxyTestHtml() {
    const t = state.proxyTest;
    if (!t) return '';
    if (t.status === 'running') {
      return '<div class="proxy-test-row is-running"><span class="spinner"></span><span>测试中…</span></div>';
    }
    const meta = function (res) {
      const parts = [];
      if (res.target) parts.push('目标 ' + res.target);
      if (typeof res.elapsedMs === 'number') parts.push('耗时 ' + res.elapsedMs + ' ms');
      if (typeof res.proxyUrl === 'string' && res.proxyUrl) parts.push('代理 ' + res.proxyUrl);
      return parts.join(' · ');
    };
    if (t.status === 'ok') {
      return '<div class="proxy-test-row tone-ok"><span class="proxy-test-icon">' + ICONS.check + '</span>' +
        '<span class="proxy-test-body"><strong>连通正常</strong>' +
        '<span class="proxy-test-meta mono">' + esc(meta(t)) + '</span></span></div>';
    }
    if (t.status === 'error') {
      return '<div class="proxy-test-row tone-error"><span class="proxy-test-icon">' + ICONS.alert + '</span>' +
        '<span class="proxy-test-body"><strong>连通失败</strong>' +
        '<span class="proxy-test-meta">' + esc(t.message || '未知错误') + '</span>' +
        (t.target ? '<span class="proxy-test-meta mono">' + esc(meta(t)) + '</span>' : '') +
        '</span></div>';
    }
    return '';
  }

  function setProxyTestResult() {
    const box = document.getElementById('proxy-test-result');
    if (box) box.innerHTML = proxyTestHtml();
  }

  async function runProxyTest(button) {
    const form = document.querySelector('form[data-form="settings"]');
    if (!form || !state.settingsDraft) return;
    const proxyInput = form.querySelector('[name="proxyUrl"]');
    const noProxyInput = form.querySelector('[name="noProxy"]');
    const imageInput = document.getElementById('settings-proxy-test-image');
    const body = {
      proxyUrl: proxyInput ? String(proxyInput.value).trim() : '',
      noProxy: noProxyInput ? String(noProxyInput.value).trim() : '',
      image: imageInput ? String(imageInput.value).trim() : ''
    };
    state.proxyTest = { status: 'running' };
    setProxyTestResult();
    setBusy(button, true, '测试中…');
    try {
      const res = await api('/api/settings/proxy-test', { method: 'POST', body: JSON.stringify(body) });
      if (res && res.ok) {
        state.proxyTest = {
          status: 'ok',
          target: res.target || '',
          proxyUrl: res.proxyUrl || '',
          noProxy: res.noProxy || '',
          elapsedMs: typeof res.elapsedMs === 'number' ? res.elapsedMs : 0
        };
        toast('代理连通性测试通过', 'success');
      } else {
        state.proxyTest = {
          status: 'error',
          target: res ? (res.target || '') : '',
          proxyUrl: res ? (res.proxyUrl || '') : '',
          elapsedMs: res && typeof res.elapsedMs === 'number' ? res.elapsedMs : 0,
          message: (res && res.error) || '代理未能连通目标仓库'
        };
        toast(state.proxyTest.message, 'error');
      }
    } catch (err) {
      state.proxyTest = { status: 'error', message: err && err.message ? err.message : '测试请求失败' };
      toast(state.proxyTest.message, 'error');
    }
    setBusy(button, false);
    setProxyTestResult();
  }

  function smbSettingsHtml(d) {
    const cfg = d.smb || {};
    let html = '<section class="card card-pad"><div class="card-head"><h3>SMB 远端存储</h3><p>tar 校验后自动上传，本地副本保留。目标目录须预先创建；远端文件不随本地清理删除。</p></div>';
    html += '<label><input type="checkbox" name="smbEnabled"' + (cfg.enabled ? ' checked' : '') + '> 启用 SMB 上传</label><div class="settings-grid">';
    [['share', '共享地址', '//192.168.1.10/backups'], ['directory', '共享内目录（可空）', 'docker-images'], ['username', '用户名', ''], ['password', '密码', cfg.hasPassword ? '已设置，留空保留；更换共享或账号须重填' : ''], ['domain', '域（可空）', 'WORKGROUP']].forEach(function (f) {
      html += '<div class="field"><label for="smb-' + f[0] + '">' + f[1] + '</label><input class="input" id="smb-' + f[0] + '" name="smb-' + f[0] + '" type="' + (f[0] === 'password' ? 'password' : 'text') + '" autocomplete="off" placeholder="' + esc(f[2]) + '" value="' + esc(cfg[f[0]] || '') + '"></div>';
    });
    return html + '</div><span class="field-hint">凭据保存在服务端 state.json；请限制数据目录访问权限。上传失败保留本地 tar，下次同步会重试。</span></section>';
  }

  function captureSMB(form, previous) {
    const cfg = { enabled: !!form.querySelector('[name="smbEnabled"]').checked, hasPassword: !!(previous && previous.hasPassword) };
    ['share', 'directory', 'username', 'password', 'domain'].forEach(function (key) {
      cfg[key] = form.querySelector('[name="smb-' + key + '"]').value;
    });
    return cfg;
  }

  function renderSettings() {
    const head = pageHead('设置', '服务参数、缓存策略与仓库凭据', btn('刷新', 'refresh', { cls: 'btn-ghost' }));

    if (!state.settingsDraft) {
      if (state.loading.settings) return head + skeletonTable(4, 4);
      return head + errorCard(state.errors.settings, 'refresh');
    }

    const d = state.settingsDraft;
    const platform = d.defaultPlatform || '';
    const platformCustom = platform !== '' && PLATFORM_PRESETS.indexOf(platform) === -1;
    const dataDir = d.dataDir || (state.settings && state.settings.dataDir) || '—';

    const platformField = '<div class="field"><label for="settings-platform">默认平台</label>' +
      '<select id="settings-platform" name="defaultPlatform" class="input">' + platformOptions(platform, '（未设置）') + '</select>' +
      '<input id="settings-platform-custom" name="defaultPlatformCustom" class="input mono" placeholder="例如 linux/riscv64" value="' +
      esc(platformCustom ? platform : '') + '"' + (platformCustom ? '' : ' hidden') + '>' +
      '<span class="field-hint">新建任务未指定平台时使用。</span></div>';

    const form = '<form data-form="settings" novalidate>' +
      '<div class="settings-stack">' + smbSettingsHtml(d) +
      '<section class="card card-pad">' +
      '<div class="card-head"><h3>基础设置</h3><p>影响调度、下载并发与 blob 缓存清理策略。</p></div>' +
      '<div class="settings-grid">' +
      platformField +
      settingNumberField('maxConcurrentRuns', '最大并发运行数', d.maxConcurrentRuns, 1, 16, '同时执行的同步任务上限（1–16）。') +
      settingNumberField('maxConcurrentDownloads', '最大并发下载数', d.maxConcurrentDownloads, 1, 16, '单个任务内并行下载 blob 的上限（1–16）。') +
      settingNumberField('blobCacheMaxGB', 'blob 缓存上限 (GB)', d.blobCacheMaxGB, 1, 1024, '超过上限时按最近使用时间清理（1–1024）。') +
      settingNumberField('blobCacheMaxAgeDays', '缓存过期天数', d.blobCacheMaxAgeDays, 1, 365, '超过天数的 blob 会被清理（1–365）。') +
      '<div class="field"><label>数据目录</label><div class="readonly-box">' +
      '<span class="mono">' + esc(dataDir) + '</span></div>' +
      '<span class="field-hint">只读，由服务启动参数决定。</span></div>' +
      '</div></section>' +
      '<section class="card card-pad">' +
      '<div class="card-head"><h3>仓库凭据</h3><p>用于拉取私有镜像仓库。密码仅提交给服务端，读取接口只返回是否已设置。</p></div>' +
      '<div class="table-wrap"><table class="table registry-table"><thead><tr>' +
      '<th>仓库地址</th><th>用户名</th><th>密码</th><th>代理</th><th class="center">明文 HTTP</th><th class="right">操作</th>' +
      '</tr></thead><tbody>' + registryRowsHtml(d.registries || []) + '</tbody></table></div>' +
      '<div class="section-actions"><button type="button" class="btn btn-ghost" data-action="registry-add">＋ 添加仓库</button></div>' +
      '</section>' +
      '<section class="card card-pad">' +
      '<div class="card-head"><h3>网络代理</h3><p>拉取镜像时使用的出站代理。留空表示跟随服务端 HTTP_PROXY / HTTPS_PROXY 环境变量。</p></div>' +
      '<div class="settings-grid">' +
      '<div class="field"><label for="settings-proxy-url">代理地址</label>' +
      '<input id="settings-proxy-url" name="proxyUrl" class="input mono" placeholder="留空跟随环境变量；输入 - 强制直连；例如 http://127.0.0.1:7890 / socks5://127.0.0.1:1080" value="' +
      esc(d.proxyUrl || '') + '">' +
      '<span class="field-hint">支持 http / https / socks5 / socks5h / socks4；省略协议时按 http:// 处理；填 <code>-</code> 表示强制直连。</span></div>' +
      '<div class="field"><label for="settings-no-proxy">不走代理的地址</label>' +
      '<input id="settings-no-proxy" name="noProxy" class="input mono" placeholder="docker.io,*.local,*.corp.local（仅 host / host:port / *.suffix，不支持 CIDR）" value="' +
      esc(d.noProxy || '') + '">' +
      '<span class="field-hint">逗号分隔的主机名、<code>host:port</code> 或 <code>*.suffix</code>；<code>*</code> 表示全部直连。</span></div>' +
      '</div>' +
      '<div class="proxy-test-bar">' +
      '<div class="field proxy-test-field"><label for="settings-proxy-test-image">测试目标镜像（可选）</label>' +
      '<input id="settings-proxy-test-image" class="input mono" placeholder="例如 alpine:3.20，留空探测 registry-1.docker.io">' +
      '<span class="field-hint">会向镜像所属仓库的 /v2/ 发起请求；该仓库若勾选明文 HTTP 则走 http。</span></div>' +
      btn('测试连通性', 'proxy-test', { cls: 'btn-ghost' }) +
      '</div>' +
      '<div id="proxy-test-result">' + proxyTestHtml() + '</div>' +
      '</section>' +
      '</div>' +
      '<div class="form-actions"><button type="submit" class="btn btn-primary">保存设置</button></div>' +
      '</form>';

    return head + errorBanner(state.errors.settings) + form;
  }

  /* ================================================================== */
  /* 渲染入口与外壳                                                      */
  /* ================================================================== */

  const PAGES = {
    dashboard: renderDashboard,
    tasks: renderTasks,
    runs: renderRuns,
    files: renderFiles,
    settings: renderSettings
  };

  function render() {
    const view = document.getElementById('view');
    if (!view) return;
    const page = PAGES[state.route] || renderDashboard;
    let html;
    try {
      html = page();
    } catch (err) {
      html = errorCard('页面渲染失败：' + (err && err.message ? err.message : '未知错误'), 'refresh');
    }
    view.innerHTML = html;
  }

  function updateChrome() {
    const meta = ROUTES[state.route] || ROUTES.dashboard;
    const titleEl = document.getElementById('page-title');
    if (titleEl) titleEl.textContent = meta.title;
    const subEl = document.getElementById('page-sub');
    if (subEl) subEl.textContent = meta.sub;

    const links = document.querySelectorAll('.nav a[data-route]');
    for (let i = 0; i < links.length; i += 1) {
      const link = links[i];
      const active = link.getAttribute('data-route') === state.route;
      link.classList.toggle('active', active);
      if (active) link.setAttribute('aria-current', 'page');
      else link.removeAttribute('aria-current');
    }

    const pill = document.getElementById('service-status');
    if (pill) {
      if (state.health && state.health.status === 'ok') {
        pill.className = 'status-pill ok';
        pill.title = '服务状态正常';
        pill.innerHTML = '<span class="dot dot-green"></span><span>服务正常</span>' +
          (state.health.version ? ' · v' + esc(state.health.version) : '');
      } else if (state.health) {
        pill.className = 'status-pill bad';
        pill.title = state.healthError || '服务状态异常';
        pill.innerHTML = '<span class="dot dot-red"></span><span>服务异常</span>';
      } else if (state.healthError) {
        pill.className = 'status-pill bad';
        pill.title = state.healthError;
        pill.innerHTML = '<span class="dot dot-red"></span><span>无法连接</span>';
      } else {
        pill.className = 'status-pill';
        pill.title = '正在检查服务状态';
        pill.innerHTML = '<span class="dot"></span><span>连接中…</span>';
      }
    }

    const versionEl = document.getElementById('sidebar-version');
    if (versionEl) {
      versionEl.textContent = state.health && state.health.version ? 'v' + state.health.version : '—';
    }

    updateSseUi();
  }

  function updateSseUi() {
    const el = document.getElementById('sse-status');
    if (!el) return;
    el.classList.remove('online', 'offline', 'connecting');
    const label = document.getElementById('sse-label');
    if (state.sse === 'online') {
      el.classList.add('online');
      el.title = '实时事件已连接';
      if (label) label.textContent = '实时已连接';
    } else if (state.sse === 'offline') {
      el.classList.add('offline');
      el.title = '实时连接已断开，正在重连（15 秒轮询兜底）';
      if (label) label.textContent = '实时已断开';
    } else {
      el.classList.add('connecting');
      el.title = '正在连接实时事件…';
      if (label) label.textContent = '连接中';
    }
  }

  function setSse(status) {
    if (state.sse === status) return;
    state.sse = status;
    updateSseUi();
  }

  /* ================================================================== */
  /* 路由                                                                */
  /* ================================================================== */

  function parseHash() {
    const raw = String(location.hash || '').replace(/^#\/?/, '').split('?')[0];
    return Object.prototype.hasOwnProperty.call(ROUTES, raw) ? raw : 'dashboard';
  }

  function onRouteChange() {
    const next = parseHash();
    const changed = next !== state.route;
    state.route = next;
    updateChrome();
    if (changed) closeDrawer();
    if (changed && next !== 'settings') state.proxyTest = null;
    render();
    loadRouteData(next, routeHasData(next));
  }

  /* ================================================================== */
  /* SSE                                                                 */
  /* ================================================================== */

  function connectSSE() {
    if (!window.EventSource) {
      setSse('offline');
      return;
    }
    if (state.eventSource) {
      try { state.eventSource.close(); } catch (err) { /* ignore */ }
    }
    const source = new EventSource('/api/events');
    state.eventSource = source;

    source.onopen = function () {
      setSse('online');
    };

    source.onerror = function () {
      setSse('offline');
    };

    source.addEventListener('update', function (event) {
      setSse('online');
      let payload = null;
      try { payload = JSON.parse(event.data); } catch (err) { return; }
      if (!payload || typeof payload !== 'object') return;
      handleUpdateEvent(payload);
    });
  }

  function handleUpdateEvent(payload) {
    const type = payload.type;
    const id = payload.id;
    if (type === 'task') {
      loadTasks(true);
      loadOverview(true);
    } else if (type === 'run') {
      loadRuns(true);
      loadOverview(true);
      if (state.drawerRunId && (!id || id === state.drawerRunId)) loadRunDetail(state.drawerRunId, true);
    } else if (type === 'file') {
      loadFiles(true);
      loadOverview(true);
    } else if (type === 'settings' && state.route !== 'settings') {
      loadSettings(true, false);
    }
    /* 日志行通过 2 秒轮询获取，避免高频刷新请求轰炸后端 */
  }

  /* ================================================================== */
  /* 模态框                                                              */
  /* ================================================================== */

  function openModalHtml(html) {
    const root = document.getElementById('modal-root');
    if (!root) return;
    root.innerHTML = html;
    document.body.classList.add('modal-open');
    const focusEl = root.querySelector('[data-autofocus]') || root.querySelector('input, select, textarea, button');
    if (focusEl && typeof focusEl.focus === 'function') {
      try { focusEl.focus(); } catch (err) { /* ignore */ }
    }
  }

  function clearModal() {
    const root = document.getElementById('modal-root');
    if (root) root.innerHTML = '';
    document.body.classList.remove('modal-open');
  }

  function closeModal() {
    state.taskModal = null;
    const resolve = state.confirmResolve;
    state.confirmResolve = null;
    clearModal();
    if (resolve) resolve(false);
  }

  function settleConfirm(value) {
    const resolve = state.confirmResolve;
    state.confirmResolve = null;
    clearModal();
    state.taskModal = null;
    if (resolve) resolve(value);
  }

  function confirmDialog(options) {
    const o = options || {};
    return new Promise(function (resolve) {
      state.confirmResolve = resolve;
      const danger = o.danger !== false;
      openModalHtml('<div class="modal-backdrop" data-action="modal-close">' +
        '<div class="modal modal-sm" role="alertdialog" aria-modal="true" aria-labelledby="confirm-title">' +
        '<header class="modal-head"><h3 id="confirm-title">' + esc(o.title || '确认操作') + '</h3>' +
        '<button type="button" class="btn btn-icon btn-ghost" data-action="modal-close" aria-label="关闭">✕</button></header>' +
        '<div class="modal-body"><p class="confirm-text">' + esc(o.message || '') + '</p></div>' +
        '<footer class="modal-foot">' +
        '<button type="button" class="btn btn-ghost" data-action="confirm-no">取消</button>' +
        '<button type="button" class="btn ' + (danger ? 'btn-danger' : 'btn-primary') + '" data-action="confirm-yes">' +
        esc(o.confirmText || '确定') + '</button>' +
        '</footer></div></div>');
    });
  }

  function taskModalHtml(mode, task) {
    const isEdit = mode === 'edit' && task;
    const t = task || {};
    const scheduleType = t.scheduleType === 'cron' ? 'cron' : 'interval';
    const interval = num(t.intervalMinutes) > 0 ? num(t.intervalMinutes) : 60;
    const platform = t.platform || '';
    const platformCustom = platform !== '' && PLATFORM_PRESETS.indexOf(platform) === -1;
    const platformValue = platformCustom ? '__custom__' : platform;
    const keepVersions = num(t.keepVersions) > 0 ? num(t.keepVersions) : 3;
    const autoSync = t.autoSync === undefined ? true : !!t.autoSync;
    const enabled = t.enabled === undefined ? true : !!t.enabled;
    const name = t.name || '';
    const image = t.image || '';
    const cron = t.cron || '';

    return '<div class="modal-backdrop" data-action="modal-close">' +
      '<div class="modal" role="dialog" aria-modal="true" aria-labelledby="modal-title">' +
      '<form data-form="task" novalidate>' +
      '<header class="modal-head"><h3 id="modal-title">' + (isEdit ? '编辑任务' : '新建任务') + '</h3>' +
      '<button type="button" class="btn btn-icon btn-ghost" data-action="modal-close" aria-label="关闭">✕</button></header>' +
      '<div class="modal-body"><div class="form-stack">' +
      '<div class="field"><label for="task-name">任务名称</label>' +
      '<input id="task-name" name="name" class="input" maxlength="100" value="' + esc(name) + '" placeholder="例如：alpine 3.20（留空则使用镜像名）">' +
      '</div>' +
      '<div class="field"><label for="task-image">镜像 <span class="req">*</span></label>' +
      '<input id="task-image" name="image" class="input mono" value="' + esc(image) + '" placeholder="alpine:3.20 / ghcr.io/owner/app:v1" data-autofocus>' +
      '<span class="field-hint">支持 Docker Hub、ghcr.io 等 registry 引用，也可使用 digest 固定版本。</span>' +
      '</div>' +
      '<div class="grid-2">' +
      '<div class="field"><label for="task-platform">平台</label>' +
      '<select id="task-platform" name="platform" class="input">' + platformOptions(platformValue === '__custom__' ? '__custom__' : platform, '跟随默认平台') + '</select>' +
      '<input id="task-platform-custom" name="platformCustom" class="input mono" placeholder="例如 linux/riscv64" value="' +
      esc(platformCustom ? platform : '') + '"' + (platformCustom ? '' : ' hidden') + '>' +
      '</div>' +
      '<div class="field"><label for="task-schedule">调度类型</label>' +
      '<select id="task-schedule" name="scheduleType" class="input">' +
      '<option value="interval"' + (scheduleType === 'interval' ? ' selected' : '') + '>固定间隔</option>' +
      '<option value="cron"' + (scheduleType === 'cron' ? ' selected' : '') + '>Cron 表达式</option>' +
      '</select></div>' +
      '</div>' +
      '<div class="grid-2">' +
      '<div class="field" id="task-interval-wrap"' + (scheduleType === 'cron' ? ' hidden' : '') + '>' +
      '<label for="task-interval">间隔（分钟）</label>' +
      '<input id="task-interval" name="intervalMinutes" type="number" class="input" min="1" step="1" value="' + esc(String(interval)) + '">' +
      '<span class="field-hint">最小 1 分钟，默认 60 分钟。</span></div>' +
      '<div class="field" id="task-cron-wrap"' + (scheduleType === 'cron' ? '' : ' hidden') + '>' +
      '<label for="task-cron">Cron 表达式</label>' +
      '<input id="task-cron" name="cron" class="input mono" value="' + esc(cron) + '" placeholder="*/30 * * * *">' +
      '<span class="field-hint">5 段：分 时 日 月 周（服务器本地时区）。示例：*/30 * * * *（每 30 分钟）、0 3 * * *（每天 03:00）。</span></div>' +
      '</div>' +
      '<div class="field"><label class="check" for="task-autosync">' +
      '<input id="task-autosync" name="autoSync" type="checkbox"' + (autoSync ? ' checked' : '') + '>' +
      '<span>自动同步<small>定时检查发现新版本时自动拉取并生成 tar；关闭后仅在任务页标记「有新版本」，等待手动同步。</small></span>' +
      '</label></div>' +
      '<div class="grid-2">' +
      '<div class="field"><label for="task-keep">保留份数</label>' +
      '<input id="task-keep" name="keepVersions" type="number" class="input" min="1" max="50" step="1" value="' + esc(String(keepVersions)) + '">' +
      '<span class="field-hint">每个任务保留的 tar 份数（1–50），更早的会自动清理。</span></div>' +
      '<div class="field"><label>任务状态</label>' +
      '<label class="check" for="task-enabled" style="min-height:38px;align-items:center">' +
      '<input id="task-enabled" name="enabled" type="checkbox"' + (enabled ? ' checked' : '') + '>' +
      '<span>启用任务<small>暂停后不再按调度检查，配置与已有 tar 保留。</small></span>' +
      '</label></div>' +
      '</div>' +
      '</div></div>' +
      '<footer class="modal-foot">' +
      '<button type="button" class="btn btn-ghost" data-action="modal-close">取消</button>' +
      '<button type="submit" class="btn btn-primary">' + (isEdit ? '保存修改' : '创建任务') + '</button>' +
      '</footer>' +
      '</form></div></div>';
  }

  function openTaskModal(mode, task) {
    state.taskModal = { mode: mode, task: task || null };
    openModalHtml(taskModalHtml(mode, task));
  }

  function toggleTaskPlatformCustom() {
    const select = document.getElementById('task-platform');
    const input = document.getElementById('task-platform-custom');
    if (!select || !input) return;
    const custom = select.value === '__custom__';
    input.hidden = !custom;
    if (custom) focusField('task-platform-custom');
  }

  function toggleTaskSchedule() {
    const select = document.getElementById('task-schedule');
    const intervalWrap = document.getElementById('task-interval-wrap');
    const cronWrap = document.getElementById('task-cron-wrap');
    if (!select || !intervalWrap || !cronWrap) return;
    const cron = select.value === 'cron';
    intervalWrap.hidden = cron;
    cronWrap.hidden = !cron;
    if (cron) focusField('task-cron');
    else focusField('task-interval');
  }

  function toggleSettingsPlatform() {
    const select = document.getElementById('settings-platform');
    const input = document.getElementById('settings-platform-custom');
    if (!select || !input) return;
    const custom = select.value === '__custom__';
    input.hidden = !custom;
    if (custom) focusField('settings-platform-custom');
  }

  /* ================================================================== */
  /* 表单提交                                                            */
  /* ================================================================== */

  function taskInputFrom(task, overrides) {
    const input = {
      name: task.name || '',
      image: task.image || '',
      platform: task.platform || '',
      scheduleType: task.scheduleType === 'cron' ? 'cron' : 'interval',
      intervalMinutes: num(task.intervalMinutes) > 0 ? num(task.intervalMinutes) : 60,
      cron: task.cron || '',
      autoSync: !!task.autoSync,
      keepVersions: num(task.keepVersions) > 0 ? num(task.keepVersions) : 3,
      enabled: !!task.enabled
    };
    if (overrides) Object.assign(input, overrides);
    return input;
  }

  async function submitTaskForm(form) {
    const submitted = form.querySelector('button[type="submit"]');
    try {
      const fd = new FormData(form);
      const image = String(fd.get('image') || '').trim();
      const name = String(fd.get('name') || '').trim();
      const scheduleType = fd.get('scheduleType') === 'cron' ? 'cron' : 'interval';
      const cron = String(fd.get('cron') || '').trim();
      const intervalRaw = String(fd.get('intervalMinutes') || '').trim();
      const interval = parseInt(intervalRaw, 10);
      const platformSelect = String(fd.get('platform') || '');
      const platform = platformSelect === '__custom__'
        ? String(fd.get('platformCustom') || '').trim()
        : platformSelect;
      const keepRaw = String(fd.get('keepVersions') || '').trim();
      const keepVersions = parseInt(keepRaw, 10);

      if (!image) {
        focusField('task-image');
        throw new Error('请填写镜像地址');
      }
      if (platform && /\s/.test(platform)) {
        focusField('task-platform-custom');
        throw new Error('平台格式不正确，应形如 linux/amd64');
      }
      if (scheduleType === 'interval' && (!isFinite(interval) || interval < 1)) {
        focusField('task-interval');
        throw new Error('间隔分钟数必须为大于等于 1 的整数');
      }
      if (scheduleType === 'cron') {
        const parts = cron.split(/\s+/).filter(Boolean);
        if (!cron) {
          focusField('task-cron');
          throw new Error('请填写 Cron 表达式');
        }
        if (parts.length !== 5) {
          focusField('task-cron');
          throw new Error('Cron 表达式必须是 5 段：分 时 日 月 周');
        }
      }
      if (!isFinite(keepVersions) || keepVersions < 1 || keepVersions > 50) {
        focusField('task-keep');
        throw new Error('保留份数必须在 1–50 之间');
      }

      const payload = {
        name: name || image,
        image: image,
        platform: platform,
        scheduleType: scheduleType,
        intervalMinutes: isFinite(interval) && interval > 0 ? interval : 60,
        cron: scheduleType === 'cron' ? cron : '',
        autoSync: fd.get('autoSync') === 'on',
        keepVersions: keepVersions,
        enabled: fd.get('enabled') === 'on'
      };

      const modal = state.taskModal || { mode: 'create', task: null };
      setBusy(submitted, true, '保存中…');

      if (modal.mode === 'edit' && modal.task && modal.task.id) {
        await api('/api/tasks/' + encodeURIComponent(modal.task.id), { method: 'PUT', body: JSON.stringify(payload) });
        toast('任务已更新', 'success');
      } else {
        await api('/api/tasks', { method: 'POST', body: JSON.stringify(payload) });
        toast('任务已创建', 'success');
      }

      closeModal();
      await Promise.all([loadTasks(true), loadOverview(true)]);
    } catch (err) {
      toast(err && err.message ? err.message : '保存失败', 'error');
      setBusy(submitted, false);
    }
  }

  function captureSettingsForm() {
    const form = document.querySelector('form[data-form="settings"]');
    if (!form || !state.settingsDraft) return;
    const draft = state.settingsDraft;

    const platformSelect = form.querySelector('[name="defaultPlatform"]');
    const platformCustom = form.querySelector('[name="defaultPlatformCustom"]');
    if (platformSelect) {
      draft.defaultPlatform = platformSelect.value === '__custom__'
        ? String(platformCustom ? platformCustom.value : '').trim()
        : platformSelect.value;
    }

    ['maxConcurrentRuns', 'maxConcurrentDownloads', 'blobCacheMaxGB', 'blobCacheMaxAgeDays'].forEach(function (key) {
      const input = form.querySelector('[name="' + key + '"]');
      if (input) draft[key] = input.value;
    });

    draft.smb = captureSMB(form, draft.smb);

    ['proxyUrl', 'noProxy'].forEach(function (key) {
      const input = form.querySelector('[name="' + key + '"]');
      if (input) draft[key] = input.value;
    });

    const rows = form.querySelectorAll('[data-reg-row]');
    const registries = [];
    for (let i = 0; i < rows.length; i += 1) {
      const row = rows[i];
      const get = function (field) {
        return row.querySelector('[data-reg-field="' + field + '"]');
      };
      const hostInput = get('host');
      const usernameInput = get('username');
      const passwordInput = get('password');
      const insecureInput = get('insecure');
      const proxyInput = get('proxyUrl');
      registries.push({
        host: hostInput ? hostInput.value : '',
        username: usernameInput ? usernameInput.value : '',
        password: passwordInput ? passwordInput.value : '',
        hasPassword: row.getAttribute('data-has-password') === 'true',
        insecure: !!(insecureInput && insecureInput.checked),
        proxyUrl: proxyInput ? proxyInput.value : ''
      });
    }
    draft.registries = registries;
    state.settingsDirty = true;
  }

  async function submitSettingsForm(form) {
    const submitted = form.querySelector('button[type="submit"]');
    try {
      const fd = new FormData(form);
      const platformSelect = String(fd.get('defaultPlatform') || '');
      const platform = platformSelect === '__custom__'
        ? String(fd.get('defaultPlatformCustom') || '').trim()
        : platformSelect;

      if (!platform) {
        focusField('settings-platform');
        throw new Error('请选择或填写默认平台');
      }
      if (/\s/.test(platform)) {
        focusField('settings-platform-custom');
        throw new Error('默认平台格式不正确，应形如 linux/amd64');
      }

      const numberFields = [
        { name: 'maxConcurrentRuns', label: '最大并发运行数', min: 1, max: 16 },
        { name: 'maxConcurrentDownloads', label: '最大并发下载数', min: 1, max: 16 },
        { name: 'blobCacheMaxGB', label: 'blob 缓存上限', min: 1, max: 1024 },
        { name: 'blobCacheMaxAgeDays', label: '缓存过期天数', min: 1, max: 365 }
      ];
      const payload = {
        defaultPlatform: platform,
        maxConcurrentRuns: 0,
        maxConcurrentDownloads: 0,
        blobCacheMaxGB: 0,
        blobCacheMaxAgeDays: 0,
        proxyUrl: String(fd.get('proxyUrl') || '').trim(),
        noProxy: String(fd.get('noProxy') || '').trim(),
        smb: captureSMB(form, state.settingsDraft.smb),
        registries: []
      };
      numberFields.forEach(function (field) {
        const value = parseInt(String(fd.get(field.name) || '').trim(), 10);
        if (!isFinite(value) || value < field.min || value > field.max) {
          focusField('set-' + field.name);
          throw new Error(field.label + '必须是 ' + field.min + '–' + field.max + ' 的整数');
        }
        payload[field.name] = value;
      });

      const rows = form.querySelectorAll('[data-reg-row]');
      const seen = Object.create(null);
      for (let i = 0; i < rows.length; i += 1) {
        const row = rows[i];
        const hostInput = row.querySelector('[data-reg-field="host"]');
        const usernameInput = row.querySelector('[data-reg-field="username"]');
        const passwordInput = row.querySelector('[data-reg-field="password"]');
        const insecureInput = row.querySelector('[data-reg-field="insecure"]');
        const proxyInput = row.querySelector('[data-reg-field="proxyUrl"]');
        const host = String(hostInput ? hostInput.value : '').trim();
        if (!host) {
          if (hostInput) hostInput.focus();
          throw new Error('仓库地址不能为空');
        }
        const hostKey = host.toLowerCase();
        if (seen[hostKey]) {
          if (hostInput) hostInput.focus();
          throw new Error('仓库地址不能重复：' + host);
        }
        seen[hostKey] = true;
        const password = String(passwordInput ? passwordInput.value : '');
        const hasPassword = row.getAttribute('data-has-password') === 'true' || password.length > 0;
        const registry = {
          host: host,
          username: String(usernameInput ? usernameInput.value : '').trim(),
          insecure: !!(insecureInput && insecureInput.checked),
          proxyUrl: String(proxyInput ? proxyInput.value : '').trim(),
          hasPassword: hasPassword
        };
        if (password) registry.password = password;
        payload.registries.push(registry);
      }

      setBusy(submitted, true, '保存中…');
      await api('/api/settings', { method: 'PUT', body: JSON.stringify(payload) });
      state.settingsDirty = false;
      await loadSettings(true, true);
      toast('设置已保存', 'success');
    } catch (err) {
      toast(err && err.message ? err.message : '保存设置失败', 'error');
      setBusy(submitted, false);
    }
  }

  /* ================================================================== */
  /* 操作按钮                                                            */
  /* ================================================================== */

  function findTask(id) {
    if (!state.tasks) return null;
    for (let i = 0; i < state.tasks.length; i += 1) {
      if (state.tasks[i].id === id) return state.tasks[i];
    }
    return null;
  }

  function findFile(name) {
    if (!state.files) return null;
    for (let i = 0; i < state.files.length; i += 1) {
      if (state.files[i].name === name) return state.files[i];
    }
    return null;
  }

  async function deleteTask(button) {
    const task = findTask(button.getAttribute('data-id'));
    if (!task) return;
    const label = task.name || task.image || task.id;
    const ok = await confirmDialog({
      title: '删除任务',
      message: '确定删除任务「' + label + '」吗？该任务产生的 tar 文件也会一并删除，此操作不可恢复。',
      confirmText: '删除任务'
    });
    if (!ok) return;

    setBusy(button, true, '删除中…');
    try {
      await api('/api/tasks/' + encodeURIComponent(task.id), { method: 'DELETE' });
      toast('任务已删除', 'success');
      await Promise.all([loadTasks(true), loadOverview(true)]);
    } catch (err) {
      toast(err.message, 'error');
    } finally {
      setBusy(button, false);
    }
  }

  async function toggleTask(button) {
    const task = findTask(button.getAttribute('data-id'));
    if (!task) return;
    setBusy(button, true, task.enabled ? '暂停中…' : '启用中…');
    try {
      const payload = taskInputFrom(task, { enabled: !task.enabled });
      await api('/api/tasks/' + encodeURIComponent(task.id), { method: 'PUT', body: JSON.stringify(payload) });
      toast(task.enabled ? '任务已暂停' : '任务已启用', 'success');
      await Promise.all([loadTasks(true), loadOverview(true)]);
    } catch (err) {
      toast(err.message, 'error');
    } finally {
      setBusy(button, false);
    }
  }

  async function syncTask(button) {
    const task = findTask(button.getAttribute('data-id'));
    if (!task) return;
    setBusy(button, true, '同步中…');
    try {
      const res = await api('/api/tasks/' + encodeURIComponent(task.id) + '/sync', {
        method: 'POST',
        body: JSON.stringify({ force: false })
      });
      const run = res && res.run ? res.run : null;
      if (run && run.status === 'skipped') {
        toast('已是最新版本，跳过下载', 'info');
      } else {
        toast('同步已开始，正在跟踪运行进度', 'success');
      }
      if (run && run.id) openRunDrawer(run.id);
      loadTasks(true);
      loadRuns(true);
      loadOverview(true);
    } catch (err) {
      toast(err.message, 'error');
      loadTasks(true);
    } finally {
      setBusy(button, false);
    }
  }

  async function checkTask(button) {
    const task = findTask(button.getAttribute('data-id'));
    if (!task) return;
    setBusy(button, true, '检查中…');
    try {
      const res = await api('/api/tasks/' + encodeURIComponent(task.id) + '/check', {
        method: 'POST',
        body: JSON.stringify({})
      });
      if (res && res.updateAvailable) {
        toast('发现新版本：' + shortDigest(res.remoteDigest), 'warn');
      } else {
        toast('已是最新版本', 'success');
      }
      loadTasks(true);
      loadOverview(true);
    } catch (err) {
      toast(err.message, 'error');
    } finally {
      setBusy(button, false);
    }
  }

  // smbReady reports whether SMB upload is enabled in the saved settings.
  function smbReady() {
    return !!(state.settings && state.settings.smb && state.settings.smb.enabled);
  }

  // ensureSettingsForFiles loads /api/settings once so the files page can tell
  // whether the "上传 SMB" action is usable.
  async function ensureSettingsForFiles() {
    if (state.settings || state.smbLoading) return;
    state.smbLoading = true;
    try {
      state.settings = await api('/api/settings');
    } catch (err) {
      state.smbLoading = false;
      return;
    }
    state.smbLoading = false;
    renderIfActive('files');
  }

  async function uploadFileSMB(button) {
    const name = button.getAttribute('data-name');
    if (!name) return;
    setBusy(button, true, '上传中…');
    let run = null;
    try {
      const res = await api('/api/files/' + encodeURIComponent(name) + '/smb', { method: 'POST' });
      run = res && res.run ? res.run : null;
      toast('已上传到 SMB 共享：' + name, 'success');
    } catch (err) {
      run = err && err.body && err.body.run ? err.body.run : null;
      toast(err.message, 'error');
    } finally {
      setBusy(button, false);
      // 手动上传同样会写入运行记录，这里同步刷新，方便立刻在「运行记录」里查看。
      loadRuns(true);
      loadOverview(true);
      if (run && run.id) {
        openRunDrawer(run.id);
      }
    }
  }

  async function deleteFile(button) {
    const name = button.getAttribute('data-name');
    if (!name) return;
    const file = findFile(name);
    const label = file && file.taskName ? name + '（任务：' + file.taskName + '）' : name;
    const ok = await confirmDialog({
      title: '删除文件',
      message: '确定删除 tar 文件「' + label + '」吗？删除后不会影响任务配置，下次同步会重新生成。',
      confirmText: '删除文件'
    });
    if (!ok) return;

    setBusy(button, true, '删除中…');
    try {
      await api('/api/files/' + encodeURIComponent(name), { method: 'DELETE' });
      toast('文件已删除', 'success');
      await Promise.all([loadFiles(true), loadOverview(true)]);
    } catch (err) {
      toast(err.message, 'error');
    } finally {
      setBusy(button, false);
    }
  }

  async function handleAction(action, el) {
    switch (action) {
      case 'refresh':
        await refreshCurrent();
        break;
      case 'task-create':
        openTaskModal('create', null);
        break;
      case 'task-edit': {
        const task = findTask(el.getAttribute('data-id'));
        if (task) openTaskModal('edit', task);
        break;
      }
      case 'task-delete':
        await deleteTask(el);
        break;
      case 'task-toggle':
        await toggleTask(el);
        break;
      case 'task-sync':
        await syncTask(el);
        break;
      case 'task-check':
        await checkTask(el);
        break;
      case 'run-open':
        openRunDrawer(el.getAttribute('data-id'));
        break;
      case 'run-retry':
        if (state.drawerRunId) loadRunDetail(state.drawerRunId, false);
        break;
      case 'drawer-close':
        closeDrawer();
        break;
      case 'modal-close':
        closeModal();
        break;
      case 'confirm-yes':
        settleConfirm(true);
        break;
      case 'confirm-no':
        settleConfirm(false);
        break;
      case 'registry-add':
        captureSettingsForm();
        if (state.settingsDraft) {
          state.settingsDraft.registries.push({ host: '', username: '', password: '', hasPassword: false, insecure: false, proxyUrl: '' });
          render();
        }
        break;
      case 'registry-remove': {
        const index = parseInt(el.getAttribute('data-index'), 10);
        captureSettingsForm();
        if (state.settingsDraft && isFinite(index) && index >= 0 && index < state.settingsDraft.registries.length) {
          state.settingsDraft.registries.splice(index, 1);
          render();
        }
        break;
      }
      case 'file-smb':
        await uploadFileSMB(el);
        break;
      case 'file-delete':
        await deleteFile(el);
        break;
      case 'proxy-test':
        await runProxyTest(el);
        break;
      default:
        break;
    }
  }

  /* ================================================================== */
  /* 事件绑定                                                            */
  /* ================================================================== */

  function bindEvents() {
    document.addEventListener('click', function (event) {
      const actionEl = event.target.closest('[data-action]');
      if (actionEl) {
        if (actionEl.classList.contains('modal-backdrop') && event.target !== actionEl) return;
        const action = actionEl.getAttribute('data-action');
        Promise.resolve(handleAction(action, actionEl)).catch(function (err) {
          toast(err && err.message ? err.message : '操作失败', 'error');
        });
        return;
      }
      const row = event.target.closest('tr[data-run-id]');
      if (row) openRunDrawer(row.getAttribute('data-run-id'));
    });

    document.addEventListener('submit', function (event) {
      const form = event.target.closest('form[data-form]');
      if (!form) return;
      event.preventDefault();
      const kind = form.getAttribute('data-form');
      if (kind === 'task') {
        Promise.resolve(submitTaskForm(form)).catch(function () { /* 已在内部提示 */ });
      } else if (kind === 'settings') {
        Promise.resolve(submitSettingsForm(form)).catch(function () { /* 已在内部提示 */ });
      }
    });

    document.addEventListener('change', function (event) {
      const target = event.target;
      if (target.id === 'task-platform') toggleTaskPlatformCustom();
      else if (target.id === 'task-schedule') toggleTaskSchedule();
      else if (target.id === 'settings-platform') toggleSettingsPlatform();
      else if (target.id === 'run-filter-task') {
        state.runFilter.taskId = target.value;
        render();
      } else if (target.id === 'run-filter-status') {
        state.runFilter.status = target.value;
        render();
      }
      if (target.closest && target.closest('form[data-form="settings"]')) state.settingsDirty = true;
    });

    document.addEventListener('input', function (event) {
      const target = event.target;
      if (target.closest && target.closest('form[data-form="settings"]')) state.settingsDirty = true;
    });

    document.addEventListener('keydown', function (event) {
      if (event.key !== 'Escape') return;
      const modalRoot = document.getElementById('modal-root');
      if (modalRoot && modalRoot.firstChild) {
        closeModal();
        return;
      }
      if (state.drawerRunId) closeDrawer();
    });

    window.addEventListener('hashchange', onRouteChange);
  }

  /* ================================================================== */
  /* 启动                                                                */
  /* ================================================================== */

  function init() {
    bindEvents();

    const rawHash = String(location.hash || '').replace(/^#\/?/, '').split('?')[0];
    state.route = parseHash();
    if (!rawHash || !Object.prototype.hasOwnProperty.call(ROUTES, rawHash)) {
      try {
        history.replaceState(null, '', '#/' + state.route);
      } catch (err) {
        location.hash = '#/' + state.route;
      }
    }

    updateChrome();
    render();
    loadRouteData(state.route, false);
    loadHealth();
    connectSSE();

    /* SSE 之外的 15 秒兜底轮询：健康状态 + 当前页数据 */
    window.setInterval(function () {
      loadHealth();
      // Keep the settings form stable while the user is editing.
      if (state.route !== 'settings') loadRouteData(state.route, true);
    }, 15000);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
