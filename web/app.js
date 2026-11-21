'use strict';

const $ = (s) => document.querySelector(s);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

let state = { envs: [], jobs: [], queue: { concurrency: 0, building: 0, queued: 0 } };
let pollTimer = null;

async function api(path, opts = {}) {
  const res = await fetch(path, { ...opts, headers: { 'Content-Type': 'application/json', ...(opts.headers || {}) } });
  if (res.status === 401 && path !== '/api/login') {
    showLogin();
    throw new Error('登录已失效');
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

function toast(msg, isErr) {
  const el = $('#toast');
  el.textContent = msg;
  el.className = 'toast' + (isErr ? ' err' : '');
  el.hidden = false;
  clearTimeout(toast.t);
  toast.t = setTimeout(() => { el.hidden = true; }, 3200);
}

/* ---------- auth ---------- */

function showLogin() {
  clearInterval(pollTimer);
  pollTimer = null;
  closeDrawer();
  $('#editor').hidden = true;
  $('#app').hidden = true;
  $('#login').hidden = false;
  $('#login-token').focus();
}

function showApp() {
  $('#login').hidden = true;
  $('#app').hidden = false;
  refresh();
  loadBranches();
  clearInterval(pollTimer);
  pollTimer = setInterval(refresh, 2500);
}

$('#login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  $('#login-error').textContent = '';
  try {
    await api('/api/login', { method: 'POST', body: JSON.stringify({ token: $('#login-token').value }) });
    $('#login-token').value = '';
    showApp();
  } catch (err) {
    $('#login-error').textContent = err.message;
  }
});

$('#logout').addEventListener('click', async () => {
  await api('/api/logout', { method: 'POST' }).catch(() => {});
  showLogin();
});

/* ---------- formatting ---------- */

function ago(t) {
  if (!t) return '';
  const s = Math.max(0, (Date.now() - new Date(t)) / 1000);
  if (s < 60) return '刚刚';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}

function duration(job) {
  if (!job.startedAt) return '';
  const end = job.finishedAt ? new Date(job.finishedAt) : new Date();
  const s = Math.round((end - new Date(job.startedAt)) / 1000);
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`;
}

const short = (c) => (c ? c.slice(0, 10) : '');
const prNum = (ref) => (/^pull\/(\d+)$/.exec(ref || '') || [])[1];
const tagName = (ref) => (/^tag:(.+)$/.exec(ref || '') || [])[1];
const commitSha = (ref) => (/^commit:([0-9a-f]{40})$/.exec(ref || '') || [])[1];
function refLabel(ref) {
  if (prNum(ref)) return `PR #${prNum(ref)}`;
  if (tagName(ref)) return `标签 ${tagName(ref)}`;
  if (commitSha(ref)) return `提交 ${commitSha(ref).slice(0, 10)}`;
  return ref;
}
const PR_STATE = { closed: ['err', '已关闭'], merged: ['run', '已合并'] };

function refCell(ref, pr, slug, subject) {
  if (tagName(ref) || commitSha(ref)) {
    const badge = tagName(ref) ? `<span class="pr-badge tag-badge">TAG</span>${esc(tagName(ref))}` : `<span class="pr-badge commit-badge">COMMIT</span>${esc(commitSha(ref).slice(0, 10))}`;
    return `<span class="branch-name">${badge}</span>${subject ? `<span class="pr-title" title="${esc(subject)}">${esc(subject)}</span>` : ''}<span class="sub">${esc(slug)}</span>`;
  }
  if (!prNum(ref)) return `<span class="branch-name" title="${esc(ref)}">${esc(ref)}</span><span class="sub">${esc(slug)}</span>`;
  const [cls, label] = !pr ? [] : pr.state === 'open' ? (pr.draft ? ['', '草稿'] : []) : PR_STATE[pr.state] || ['', pr.state];
  const title = pr ? `<a class="pr-title" href="${esc(pr.url)}" title="${esc(pr.title)}" target="_blank" rel="noopener">${esc(pr.title)}</a>` : '';
  return `<span class="branch-name"><span class="pr-badge">PR #${prNum(ref)}</span>${label ? `<span class="pr-state ${cls}">${label}</span>` : ''}</span>${title}`
    + `<span class="sub" title="${pr ? esc(pr.head) : ''}">${pr ? esc(pr.head) + ' · ' : ''}${esc(slug)}</span>`;
}

function queuePosition(job) {
  const queued = state.jobs.filter((j) => j.status === 'queued' && j.kind === 'build')
    .sort((a, b) => new Date(a.createdAt) - new Date(b.createdAt));
  return queued.findIndex((j) => j.id === job.id) + 1;
}

const JOB_STATUS = {
  queued: ['warn', '排队中'], running: ['run', '运行中'], succeeded: ['ok', '成功'],
  failed: ['err', '失败'], canceled: ['', '已取消'],
};
const KIND = { build: '构建', destroy: '销毁' };

function jobPill(job) {
  const [cls, label] = JOB_STATUS[job.status] || ['', job.status];
  return `<span class="pill ${cls}">${label}</span>`;
}

function envPill(env) {
  const job = env.job;
  if (job) {
    if (job.status === 'running') return `<span class="pill run">${job.kind === 'destroy' ? '销毁中' : '构建中'}</span>`;
    const pos = queuePosition(job);
    return `<span class="pill warn">${job.kind === 'destroy' ? '等待销毁' : '排队中' + (pos ? ' #' + pos : '')}</span>`;
  }
  const map = { running: ['ok', '运行中'], starting: ['warn', '启动中'], unhealthy: ['err', '异常'], stopped: ['', '已停止'], absent: ['', '未运行'] };
  const [cls, label] = map[env.state] || ['', env.state];
  return `<span class="pill ${cls}">${label}</span>`;
}

/* ---------- rendering ---------- */

async function refresh() {
  try {
    state = await api('/api/state');
  } catch {
    return;
  }
  render();
  if (drawer.slug) renderDrawerMeta();
}

function render() {
  const q = state.queue;
  const pill = $('#queue-pill');
  pill.className = 'queue-pill' + (q.building ? ' busy' : '');
  pill.innerHTML = `<i></i>构建 ${q.building}/${q.concurrency} · 排队 ${q.queued}`;
  if (state.repo) {
    $('#repo-link').href = state.repo;
    $('#repo-name').textContent = state.repo.replace(/^https?:\/\/github\.com\//, '');
  }

  $('#env-count').textContent = state.envs.length ? `${state.envs.length} 个` : '';
  $('#seed-note').hidden = !state.postScript;
  $('#seed-note').textContent = `首次启动执行 ${state.postScript || ''} · 编辑`;
  $('#env-empty').hidden = state.envs.length > 0;
  $('#env-rows').innerHTML = state.envs.map((env) => {
    const busy = !!env.job;
    const last = env.lastJob;
    const note = !busy && last && last.status === 'failed'
      ? `<span class="sub err">上次${KIND[last.kind]}失败 · ${ago(last.finishedAt)}</span>`
      : (env.detail ? `<span class="sub" title="${esc(env.detail)}">${esc(env.detail)}</span>` : '');
    const link = env.url && env.state !== 'absent'
      ? `<a class="url" href="${esc(env.url)}" title="${esc(env.url)}" target="_blank" rel="noopener">${esc(env.url.replace(/^https?:\/\//, ''))}</a>`
      : '<span class="mute">—</span>';
    const rebuildLabel = env.job && env.job.status === 'running' && env.job.kind === 'build' ? '追加重建' : '重建';
    return `<tr>
      <td>${refCell(env.branch, env.pr, env.slug, env.subject)}</td>
      <td>${envPill(env)}${note}</td>
      <td class="mono">${esc(short(env.commit)) || '<span class="mute">—</span>'}</td>
      <td>${link}</td>
      <td class="mute small">${ago(last ? last.finishedAt || last.createdAt : env.createdAt)}</td>
      <td><div class="actions">
        ${env.url && env.state === 'running' ? `<button class="act" data-act="open" data-slug="${esc(env.slug)}" title="打开并尽量以初始化脚本里的管理员登录">打开</button>` : ''}
        <button class="act" data-act="logs" data-slug="${esc(env.slug)}">日志</button>
        ${env.state !== 'absent' ? `<button class="act" data-act="terminal" data-slug="${esc(env.slug)}">终端</button>` : ''}
        <button class="act" data-act="rebuild" data-slug="${esc(env.slug)}" ${env.job && env.job.status === 'queued' && env.job.kind === 'build' ? 'disabled' : ''}>${rebuildLabel}</button>
        <button class="act danger" data-act="destroy" data-slug="${esc(env.slug)}" data-branch="${esc(refLabel(env.branch))}">销毁</button>
      </div></td>
    </tr>`;
  }).join('');

  const active = state.jobs.filter((j) => j.status === 'queued' || j.status === 'running').length;
  $('#job-count').textContent = active ? `${active} 个进行中` : '';
  $('#job-empty').hidden = state.jobs.length > 0;
  $('#job-rows').innerHTML = state.jobs.slice(0, 30).map((job) => {
    const pos = job.status === 'queued' && job.kind === 'build' ? queuePosition(job) : 0;
    return `<tr>
      <td><span class="branch-name">${esc(refLabel(job.branch))}</span><span class="sub">${KIND[job.kind] || job.kind} · ${esc(job.id)}</span></td>
      <td>${jobPill(job)}${pos ? `<span class="sub">第 ${pos} 位</span>` : ''}${job.error ? `<span class="sub err" title="${esc(job.error)}">${esc(job.error)}</span>` : ''}</td>
      <td class="mono">${esc(short(job.commit)) || '<span class="mute">—</span>'}</td>
      <td class="mono">${duration(job) || '<span class="mute">—</span>'}</td>
      <td class="mute small">${ago(job.createdAt)}</td>
      <td><div class="actions">
        <button class="act" data-act="joblog" data-slug="${esc(job.slug)}" data-job="${esc(job.id)}">日志</button>
        ${job.status === 'queued' || job.status === 'running' ? `<button class="act danger" data-act="cancel" data-job="${esc(job.id)}">取消</button>` : ''}
      </div></td>
    </tr>`;
  }).join('');
}

/* ---------- actions ---------- */

$('#create-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const branch = $('#branch').value.trim();
  const btn = e.submitter;
  $('#create-error').textContent = '';
  btn.disabled = true;
  try {
    const job = await api('/api/envs', { method: 'POST', body: JSON.stringify({ branch }) });
    $('#branch').value = '';
    toast(`已加入队列：${refLabel(job.branch)}`);
    await refresh();
    openDrawer(job.slug, job.id);
  } catch (err) {
    $('#create-error').textContent = err.message;
  } finally {
    btn.disabled = false;
  }
});

document.addEventListener('click', async (e) => {
  const el = e.target.closest('[data-act]');
  if (!el) return;
  const { act, slug, job, branch } = el.dataset;
  if (act === 'open') {
    openEnv(slug);
    return;
  }
  try {
    if (act === 'logs') openDrawer(slug);
    if (act === 'terminal') openDrawer(slug, '', 'terminal');
    if (act === 'joblog') openDrawer(slug, job);
    if (act === 'rebuild') {
      const j = await api(`/api/envs/${encodeURIComponent(slug)}/rebuild`, { method: 'POST' });
      toast('已加入重建队列');
      await refresh();
      openDrawer(slug, j.id);
    }
    if (act === 'destroy') {
      if (!confirm(`销毁 ${branch} 的环境？容器、数据库和数据目录都会被删除。`)) return;
      await api(`/api/envs/${encodeURIComponent(slug)}`, { method: 'DELETE' });
      toast('已提交销毁');
      refresh();
    }
    if (act === 'cancel') {
      await api(`/api/jobs/${encodeURIComponent(job)}/cancel`, { method: 'POST' });
      toast('已取消');
      refresh();
    }
  } catch (err) {
    toast(err.message, true);
  }
});

// The tab is opened synchronously inside the click so popup blockers allow it;
// it is pointed at the one-time login link once that arrives, else at the plain URL.
async function openEnv(slug) {
  const env = state.envs.find((e) => e.slug === slug);
  if (!env || !env.url) return;
  const win = window.open('about:blank', '_blank');
  let target = env.url;
  try {
    const t = await api(`/api/envs/${encodeURIComponent(slug)}/login-ticket`, { method: 'POST' });
    if (t.url) target = env.url + t.url;
  } catch { /* open without signing in */ }
  if (win) {
    win.opener = null;
    win.location.href = target;
  } else {
    window.open(target, '_blank', 'noopener');
  }
}

/* ---------- branch combobox ---------- */

const combo = { names: [], pulls: [], tags: [], items: [], active: -1, limit: 60, prLimit: 8, tagLimit: 6 };
const branchInput = $('#branch');
const branchMenu = $('#branch-menu');

async function loadBranches() {
  try {
    combo.names = await api('/api/branches');
    if (!branchMenu.hidden) renderCombo();
  } catch (err) {
    toast(err.message, true);
  }
  api('/api/pulls').then((list) => {
    combo.pulls = list;
    if (!branchMenu.hidden) renderCombo();
  }).catch(() => {});
  api('/api/tags').then((list) => {
    combo.tags = list;
    if (!branchMenu.hidden) renderCombo();
  }).catch(() => {});
}

// Lower is better; null means no match. "#47", "pr 47" and "47" match by number, other text by title, head and author.
function prScore(pr, q) {
  if (!q) return 0;
  const num = /^(?:#|pr\s*#?\s*|pull\/)?(\d+)$/i.exec(q);
  if (num) return String(pr.number) === num[1] ? 0 : String(pr.number).startsWith(num[1]) ? 1 : null;
  if (/^https?:\/\//.test(q)) return q.endsWith(`/pull/${pr.number}`) ? 0 : null;
  const i = pr.title.toLowerCase().indexOf(q);
  if (i >= 0) return 2 + i / 1000;
  return (pr.head + ' ' + pr.author).toLowerCase().includes(q) ? 3 : null;
}

// Lower is better: exact, prefix, prefix of a path segment, then substring; null means no match.
function branchScore(name, q) {
  if (!q) return name === 'main' || name === 'master' ? -1 : 0;
  const n = name.toLowerCase();
  const i = n.indexOf(q);
  if (i < 0) return null;
  if (n === q) return 0;
  if (i === 0) return 1;
  if (n[i - 1] === '/' || n[i - 1] === '-') return 2;
  return 3 + i / 100;
}

function highlight(name, q) {
  const i = q ? name.toLowerCase().indexOf(q) : -1;
  const slash = name.lastIndexOf('/') + 1;
  const wrap = (s, from) => {
    // Dim the directory part (codex/, feat/ …) so the branch's own name stands out.
    const dir = Math.max(0, Math.min(s.length, slash - from));
    return (dir ? `<span class="dir">${esc(s.slice(0, dir))}</span>` : '') + esc(s.slice(dir));
  };
  if (i < 0) return wrap(name, 0);
  return wrap(name.slice(0, i), 0) + `<mark>${esc(name.slice(i, i + q.length))}</mark>` + wrap(name.slice(i + q.length), i + q.length);
}

function renderCombo() {
  const q = branchInput.value.trim().toLowerCase();
  const withEnv = new Set(state.envs.map((e) => e.branch));
  const envTag = (ref) => (withEnv.has(ref) ? '<span class="tag">已有环境</span>' : '');

  const prs = [];
  for (const pr of combo.pulls) {
    const score = prScore(pr, q);
    if (score !== null) prs.push({ pr, score });
  }
  prs.sort((a, b) => a.score - b.score);
  const prItems = prs.slice(0, combo.prLimit).map(({ pr }) => ({
    value: `#${pr.number}`,
    html: `<span class="pr-num">#${pr.number}</span><span class="name pr-name">${esc(pr.title)}<span class="dir"> · ${esc(pr.head)}</span></span>${pr.draft ? '<span class="tag draft">草稿</span>' : ''}${envTag('pull/' + pr.number)}`,
  }));

  const matched = [];
  for (const name of combo.names) {
    const score = branchScore(name, q);
    if (score !== null) matched.push({ name, score });
  }
  matched.sort((a, b) => a.score - b.score || (q ? a.name.length - b.name.length : 0) || a.name.localeCompare(b.name));
  const branchItems = matched.slice(0, combo.limit).map(({ name }) => ({
    value: name, html: `<span class="name">${highlight(name, q)}</span>${envTag(name)}`,
  }));

  // Tags show only once the input could name one, so the bare list stays about branches.
  const tagQ = q.replace(/^(tag:|refs\/tags\/)/, '');
  const tags = q ? combo.tags.filter((t) => t.toLowerCase().includes(tagQ)) : [];
  const tagItems = tags.slice(0, combo.tagLimit).map((t) => ({
    value: t, html: `<span class="pr-num">TAG</span><span class="name">${highlight(t, tagQ)}</span>${envTag('tag:' + t)}`,
  }));
  const shaQ = q.replace(/^commit:/, '');
  const commitItems = /^[0-9a-f]{7,40}$/.test(shaQ) && !combo.names.includes(shaQ) && !combo.tags.includes(shaQ)
    ? [{ value: shaQ, html: `<span class="pr-num">提交</span><span class="name">${esc(shaQ)}<span class="dir"> · 构建这个提交</span></span>` }] : [];

  // An exact PR number reads as a PR request, so PRs lead; otherwise branches do.
  const prFirst = /^(#|pr\s*#?\s*\d|pull\/|https?:)/i.test(q) || /^\d+$/.test(q);
  const sections = [
    { title: q ? '匹配的分支' : '全部分支', items: branchItems, total: matched.length },
    { title: '标签', items: tagItems, total: tags.length },
    { title: 'Pull Request', items: prItems, total: prs.length },
  ];
  if (prFirst) sections.reverse();
  if (commitItems.length) sections.unshift({ title: '提交', items: commitItems, total: 1 });
  combo.items = sections.flatMap((s) => s.items);
  combo.active = combo.items.length ? Math.min(Math.max(combo.active, 0), combo.items.length - 1) : -1;

  if (!combo.names.length && !combo.pulls.length) {
    branchMenu.innerHTML = '<div class="combo-empty">正在加载分支…</div>';
    return;
  }
  if (!combo.items.length) {
    branchMenu.innerHTML = `<div class="combo-empty">没有匹配的分支、标签或 PR，按回车按原样提交（PR 输入 #编号，提交输入 7 位以上提交号）</div>`;
    return;
  }
  let i = 0;
  branchMenu.innerHTML = sections.filter((s) => s.items.length).map((s) => `<div class="combo-head"><span>${s.title}</span><span>${s.total}</span></div>`
    + s.items.map((item) => {
      const n = i++;
      return `<div class="opt${n === combo.active ? ' active' : ''}" role="option" data-i="${n}" aria-selected="${n === combo.active}">${item.html}</div>`;
    }).join('')
    + (s.total > s.items.length ? `<div class="combo-foot">还有 ${s.total - s.items.length} 个，继续输入以缩小范围</div>` : '')).join('');
}

function openCombo() {
  branchMenu.hidden = false;
  branchInput.setAttribute('aria-expanded', 'true');
  renderCombo();
}

function closeCombo() {
  branchMenu.hidden = true;
  branchInput.setAttribute('aria-expanded', 'false');
  combo.active = -1;
}

function moveActive(delta) {
  if (!combo.items.length) return;
  combo.active = (combo.active + delta + combo.items.length) % combo.items.length;
  branchMenu.querySelectorAll('.opt').forEach((el, i) => {
    el.classList.toggle('active', i === combo.active);
    el.setAttribute('aria-selected', String(i === combo.active));
    if (i === combo.active) el.scrollIntoView({ block: 'nearest' });
  });
}

function pickBranch(i) {
  branchInput.value = combo.items[i].value;
  closeCombo();
}

branchInput.addEventListener('focus', openCombo);
branchInput.addEventListener('input', () => { combo.active = 0; openCombo(); });
branchInput.addEventListener('blur', closeCombo);
branchInput.addEventListener('keydown', (e) => {
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    e.preventDefault();
    if (branchMenu.hidden) openCombo();
    else moveActive(e.key === 'ArrowDown' ? 1 : -1);
  } else if (e.key === 'Enter' && !branchMenu.hidden && combo.active >= 0 && combo.items[combo.active].value !== branchInput.value.trim()) {
    // The first Enter fills in the highlighted branch; the next one submits.
    e.preventDefault();
    pickBranch(combo.active);
  } else if (e.key === 'Escape') {
    closeCombo();
  }
});
// mousedown keeps focus in the input, so blur does not close the menu before the click lands.
branchMenu.addEventListener('mousedown', (e) => {
  e.preventDefault();
  const opt = e.target.closest('.opt');
  if (opt) pickBranch(Number(opt.dataset.i));
});

/* ---------- log drawer ---------- */

const drawer = { slug: '', tab: 'build', jobId: '', stream: null, carry: '' };
const logEl = $('#log');

function envJobs(slug) {
  return state.jobs.filter((j) => j.slug === slug);
}

function openDrawer(slug, jobId, tab = 'build') {
  if (drawer.slug && drawer.slug !== slug) stopTerminal();
  drawer.slug = slug;
  drawer.tab = tab;
  const jobs = envJobs(slug);
  drawer.jobId = jobId || (jobs.find((j) => j.status === 'running') || jobs[0] || {}).id || '';
  $('#drawer').classList.add('open');
  $('#drawer').setAttribute('aria-hidden', 'false');
  $('#scrim').hidden = false;
  renderDrawerMeta();
  connect();
}

function closeDrawer() {
  if (drawer.stream) drawer.stream.close();
  drawer.stream = null;
  stopTerminal();
  drawer.slug = '';
  $('#drawer').classList.remove('open');
  $('#drawer').setAttribute('aria-hidden', 'true');
  $('#scrim').hidden = true;
}

$('#drawer-close').addEventListener('click', closeDrawer);
$('#scrim').addEventListener('click', closeDrawer);
document.addEventListener('keydown', (e) => {
  // Esc belongs to the shell (vim, less …) while the terminal has focus.
  if (e.key !== 'Escape' || !drawer.slug || !$('#editor').hidden || $('#term').contains(document.activeElement)) return;
  closeDrawer();
});

document.querySelectorAll('#drawer .tabs button').forEach((b) => b.addEventListener('click', () => {
  if (drawer.tab === b.dataset.tab) return;
  drawer.tab = b.dataset.tab;
  renderDrawerMeta();
  connect();
}));

$('#job-select').addEventListener('change', (e) => {
  drawer.jobId = e.target.value;
  renderDrawerMeta();
  connect();
});

function renderDrawerMeta() {
  const env = state.envs.find((e) => e.slug === drawer.slug);
  const jobs = envJobs(drawer.slug);
  const job = jobs.find((j) => j.id === drawer.jobId);
  const branch = env ? env.branch : job ? job.branch : drawer.slug;
  $('#drawer-title').innerHTML = `${esc(refLabel(branch))} ${env ? envPill(env) : ''}`;
  const sub = [];
  if (env && env.pr) sub.push(`<a class="url" href="${esc(env.pr.url)}" target="_blank" rel="noopener">${esc(env.pr.title)}</a>`);
  if (env && env.commit) sub.push(`提交 ${short(env.commit)}`);
  if (env && env.url && env.state === 'running') sub.push(`<a class="url" href="${esc(env.url)}" target="_blank" rel="noopener">${esc(env.url)}</a>`);
  $('#drawer-sub').innerHTML = sub.join(' · ');

  document.querySelectorAll('#drawer .tabs button').forEach((b) => b.classList.toggle('active', b.dataset.tab === drawer.tab));
  const isTerm = drawer.tab === 'terminal';
  $('#follow-wrap').hidden = isTerm;
  $('#ct-select').hidden = !isTerm || !term.containers.length;
  $('#term-state').hidden = !isTerm;
  $('#ct-connect').hidden = !isTerm || !term.id;
  const sel = $('#job-select');
  sel.hidden = drawer.tab !== 'build' || !jobs.length;
  sel.innerHTML = jobs.map((j) => {
    const [, label] = JOB_STATUS[j.status] || ['', j.status];
    return `<option value="${esc(j.id)}" ${j.id === drawer.jobId ? 'selected' : ''}>${KIND[j.kind]} · ${label} · ${new Date(j.createdAt).toLocaleString()}</option>`;
  }).join('');
  const raw = $('#raw-log');
  raw.hidden = drawer.tab !== 'build' || !drawer.jobId;
  raw.href = drawer.jobId ? `/api/jobs/${encodeURIComponent(drawer.jobId)}/log` : '#';
}

function connect() {
  if (drawer.stream) drawer.stream.close();
  drawer.stream = null;
  drawer.carry = '';
  logEl.textContent = '';
  logEl.hidden = drawer.tab === 'terminal';
  $('#term').hidden = drawer.tab !== 'terminal';
  if (drawer.tab === 'terminal') {
    openTerminalTab();
    return;
  }
  stopTerminal();
  let url;
  if (drawer.tab === 'build') {
    if (!drawer.jobId) {
      appendMeta('这个环境还没有构建记录。');
      return;
    }
    const job = state.jobs.find((j) => j.id === drawer.jobId);
    if (job && job.status === 'queued') appendMeta(`任务排队中${queuePosition(job) ? `（第 ${queuePosition(job)} 位）` : ''}，开始后自动显示日志…`);
    url = `/api/jobs/${encodeURIComponent(drawer.jobId)}/log`;
  } else {
    appendMeta('正在连接 platform 容器日志（最近 500 行）…');
    url = `/api/envs/${encodeURIComponent(drawer.slug)}/logs`;
  }
  const es = new EventSource(url);
  drawer.stream = es;
  es.addEventListener('log', (e) => appendLog(JSON.parse(e.data)));
  es.addEventListener('end', (e) => {
    es.close();
    flushCarry();
    if (drawer.tab === 'runtime') appendMeta('日志流已结束。');
    refresh();
  });
  es.onerror = () => {
    if (es.readyState === EventSource.CLOSED) appendMeta('日志连接失败。');
  };
}

function lineClass(line) {
  if (/^\[lfenv\]/.test(line)) return 'l-meta';
  if (/^\s*▸/.test(line)) return 'l-step';
  if (/✓|\bDONE\b|Healthy\b|succeeded/.test(line)) return 'l-ok';
  if (/✗|\berror\b|\bERROR\b|failed|Conflict/i.test(line)) return 'l-err';
  if (/^#\d+ (CACHED|\[internal\])/.test(line)) return 'l-dim';
  return '';
}

function appendLines(lines) {
  const atBottom = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 60;
  const frag = document.createDocumentFragment();
  for (const line of lines) {
    const span = document.createElement('span');
    const cls = lineClass(line);
    if (cls) span.className = cls;
    span.textContent = line + '\n';
    frag.appendChild(span);
  }
  logEl.appendChild(frag);
  if ($('#follow').checked && (atBottom || lines.length > 50)) logEl.scrollTop = logEl.scrollHeight;
}

function appendLog(text) {
  text = drawer.carry + text.replace(/\x1b\[[0-9;?]*[A-Za-z]/g, '').replace(/\r\n?/g, '\n');
  const lines = text.split('\n');
  drawer.carry = lines.pop();
  if (lines.length) appendLines(lines);
}

function flushCarry() {
  if (drawer.carry) appendLines([drawer.carry]);
  drawer.carry = '';
}

function appendMeta(msg) {
  appendLines([`[lfenv] ${msg}`]);
}

$('#follow').addEventListener('change', (e) => { if (e.target.checked) logEl.scrollTop = logEl.scrollHeight; });

/* ---------- terminal ---------- */

const term = { containers: [], id: '', xterm: null, fit: null, ws: null, ro: null };

function containerLabel(c) {
  const state = c.state === 'running' ? '' : ` · ${c.state}`;
  if (c.kind === 'service') return `${c.service} · 服务容器${state}`;
  return `${c.name.slice(0, 24)} · 执行容器${c.workspace ? ' · ' + c.workspace : ''}${state}`;
}

function setTermState(text, cls = '') {
  const el = $('#term-state');
  el.textContent = text;
  el.className = 'term-state ' + cls;
}

async function loadContainers() {
  term.containers = await api(`/api/envs/${encodeURIComponent(drawer.slug)}/containers`);
  const sel = $('#ct-select');
  sel.innerHTML = term.containers.map((c) => `<option value="${esc(c.id)}" ${c.id === term.id ? 'selected' : ''} ${c.state === 'running' ? '' : 'disabled'}>${esc(containerLabel(c))}</option>`).join('');
}

async function openTerminalTab() {
  const slug = drawer.slug;
  setTermState('正在读取容器…');
  try {
    await loadContainers();
  } catch (err) {
    setTermState(err.message, 'err');
    return;
  }
  if (drawer.tab !== 'terminal' || drawer.slug !== slug) return;
  const running = term.containers.filter((c) => c.state === 'running');
  if (!running.some((c) => c.id === term.id)) {
    term.id = (running.find((c) => c.service === 'platform') || running[0] || {}).id || '';
    $('#ct-select').value = term.id;
  }
  renderDrawerMeta();
  if (!term.id) {
    stopTerminal();
    $('#term').innerHTML = `<div class="term-empty">${term.containers.length ? '没有运行中的容器' : '这个环境还没有容器，构建完成后再试'}</div>`;
    setTermState('');
    return;
  }
  startTerminal();
}

function startTerminal() {
  stopTerminal();
  const el = $('#term');
  el.innerHTML = '';
  const xterm = new Terminal({
    cursorBlink: true, fontSize: 13, lineHeight: 1.15, scrollback: 5000,
    fontFamily: '"Geist Mono", ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
    theme: { background: '#141418', foreground: '#d6d6dc', cursor: '#8f8fff', selectionBackground: 'rgba(143,143,255,.35)' },
  });
  const fit = new FitAddon.FitAddon();
  xterm.loadAddon(fit);
  xterm.open(el);
  fit.fit();
  Object.assign(term, { xterm, fit });

  const c = term.containers.find((x) => x.id === term.id);
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(`${proto}//${location.host}/api/envs/${encodeURIComponent(drawer.slug)}/containers/${encodeURIComponent(term.id)}/terminal?cols=${xterm.cols}&rows=${xterm.rows}`);
  ws.binaryType = 'arraybuffer';
  term.ws = ws;
  setTermState('连接中…');
  ws.onopen = () => {
    setTermState(`已连接 ${c ? c.service || c.name.slice(0, 24) : ''}`, 'ok');
    xterm.focus();
  };
  ws.onmessage = (e) => {
    if (typeof e.data !== 'string') {
      xterm.write(new Uint8Array(e.data));
      return;
    }
    const m = JSON.parse(e.data);
    if (m.type === 'error') {
      xterm.write(`\r\n\x1b[31m${m.message}\x1b[0m\r\n`);
      setTermState('连接失败', 'err');
    } else if (m.type === 'exit') {
      xterm.write(`\r\n\x1b[90m[会话已结束，退出码 ${m.code}]\x1b[0m\r\n`);
    }
  };
  ws.onclose = () => {
    if (term.ws !== ws) return;
    term.ws = null;
    if (!/失败/.test($('#term-state').textContent)) setTermState('已断开');
  };
  const enc = new TextEncoder();
  xterm.onData((d) => { if (ws.readyState === WebSocket.OPEN) ws.send(enc.encode(d)); });
  xterm.onBinary((d) => { if (ws.readyState === WebSocket.OPEN) ws.send(Uint8Array.from(d, (ch) => ch.charCodeAt(0))); });
  xterm.onResize(({ cols, rows }) => { if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'resize', cols, rows })); });
  term.ro = new ResizeObserver(() => { if (!el.hidden && el.clientWidth) fit.fit(); });
  term.ro.observe(el);
}

function stopTerminal() {
  if (term.ro) term.ro.disconnect();
  if (term.ws) {
    const ws = term.ws;
    term.ws = null;
    ws.close();
  }
  if (term.xterm) term.xterm.dispose();
  Object.assign(term, { xterm: null, fit: null, ro: null });
}

$('#ct-select').addEventListener('change', (e) => {
  term.id = e.target.value;
  startTerminal();
});
$('#ct-select').addEventListener('focus', () => { loadContainers().catch(() => {}); });
$('#ct-connect').addEventListener('click', () => openTerminalTab());

/* ---------- post-init editor ---------- */

const editor = { files: [], key: '' };
const editorText = $('#editor-text');
const FILE_LABEL = { script: '脚本', config: '配置' };

const currentFile = () => editor.files.find((f) => f.key === editor.key);
const isDirty = (f) => f.draft !== f.content;

async function openEditor() {
  let files;
  try {
    files = await api('/api/post-init');
  } catch (err) {
    toast(err.message, true);
    return;
  }
  if (!files.length) {
    toast('未配置初始化脚本（LFENV_POST_SCRIPT）', true);
    return;
  }
  editor.files = files.map((f) => ({ ...f, draft: f.content }));
  if (!currentFile()) editor.key = files[0].key;
  $('#editor').hidden = false;
  renderEditor();
  editorText.focus();
}

function closeEditor() {
  if (editor.files.some(isDirty) && !confirm('有未保存的修改，确定关闭？')) return;
  $('#editor').hidden = true;
  editor.files = [];
}

function renderEditor() {
  const f = currentFile();
  $('#editor-tabs').innerHTML = editor.files.map((x) => `<button type="button" data-key="${x.key}" class="${x.key === editor.key ? 'active' : ''}">${FILE_LABEL[x.key]} ${esc(x.path.split('/').pop())}${isDirty(x) ? ' <i class="dot"></i>' : ''}</button>`).join('');
  $('#editor-path').textContent = f.path;
  if (editorText.value !== f.draft) editorText.value = f.draft;
  $('#editor-error').hidden = true;
  renderEditorStatus();
}

function renderEditorStatus() {
  const f = currentFile();
  const dirty = isDirty(f);
  $('#editor-status').textContent = dirty ? '未保存' : f.version ? `已保存 · ${ago(f.modTime)}` : '文件尚不存在';
  $('#editor-status').classList.toggle('dirty', dirty);
  $('#editor-save').disabled = !dirty;
  $('#editor-revert').disabled = !dirty;
  document.querySelectorAll('#editor-tabs button').forEach((b) => {
    const x = editor.files.find((y) => y.key === b.dataset.key);
    const dot = b.querySelector('.dot');
    if (isDirty(x) && !dot) b.insertAdjacentHTML('beforeend', ' <i class="dot"></i>');
    if (!isDirty(x) && dot) dot.remove();
  });
}

async function saveEditor() {
  const f = currentFile();
  if (!f || !isDirty(f)) return;
  $('#editor-save').disabled = true;
  try {
    const saved = await api(`/api/post-init/${f.key}`, { method: 'PUT', body: JSON.stringify({ content: f.draft, version: f.version }) });
    Object.assign(f, { content: saved.content, version: saved.version, modTime: saved.modTime });
    $('#editor-error').hidden = true;
    toast('已保存');
  } catch (err) {
    $('#editor-error').textContent = err.message;
    $('#editor-error').hidden = false;
  }
  renderEditorStatus();
}

$('#seed-note').addEventListener('click', openEditor);
$('#editor-close').addEventListener('click', closeEditor);
$('#editor').addEventListener('mousedown', (e) => { if (e.target === e.currentTarget) closeEditor(); });
$('#editor-save').addEventListener('click', saveEditor);
$('#editor-revert').addEventListener('click', () => {
  const f = currentFile();
  f.draft = f.content;
  renderEditor();
});
$('#editor-tabs').addEventListener('click', (e) => {
  const b = e.target.closest('button');
  if (!b || b.dataset.key === editor.key) return;
  editor.key = b.dataset.key;
  renderEditor();
  editorText.scrollTop = 0;
});
editorText.addEventListener('input', () => {
  currentFile().draft = editorText.value;
  renderEditorStatus();
});
editorText.addEventListener('keydown', (e) => {
  if (e.key === 'Tab' && !e.shiftKey) {
    e.preventDefault();
    editorText.setRangeText(editor.key === 'script' ? '    ' : '  ', editorText.selectionStart, editorText.selectionEnd, 'end');
    editorText.dispatchEvent(new Event('input'));
  }
});
document.addEventListener('keydown', (e) => {
  if ($('#editor').hidden) return;
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
    e.preventDefault();
    saveEditor();
  } else if (e.key === 'Escape') {
    closeEditor();
  }
});

/* ---------- boot ---------- */

fetch('/api/state').then((r) => (r.ok ? showApp() : showLogin())).catch(showLogin);
