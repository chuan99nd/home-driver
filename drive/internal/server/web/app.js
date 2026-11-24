// Drive app. Plain ES modules, no framework, no build step.
import { t, tn, getLang, setLang, applyI18n, LANGS } from './i18n.js';
import {
  $, el, clear, icon, iconButton, api, ApiError, errorText, fmtSize, fmtDate, toast, toastError, openDialog,
  confirmDanger, promptDialog, openMenu, pointOf, renderItem, skeleton, stateBox, openLightbox, transition, sortByName,
} from './ui.js';

const store = {
  get(k, d) { try { return localStorage.getItem('drive.' + k) || d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem('drive.' + k, v); } catch { /* private mode */ } },
};

const S = {
  session: null,
  me: null,
  route: { name: 'files', id: '' },
  items: [],
  folder: null, // current folder node (files view)
  role: 'owner',
  crumbs: [],
  view: store.get('view', 'grid'),
  sort: store.get('sort', 'name'),
  order: store.get('order', 'asc'),
  sel: new Set(),
  anchor: null,
  loadSeq: 0,
  error: null,
  loading: true,
};

// ---------------------------------------------------------------- boot

applyTheme(store.get('theme', 'auto'));
applyI18n(document);
init();

async function init() {
  if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => {});
  let s;
  try {
    s = await api('GET', '/api/session');
  } catch (e) {
    clear($('#boot')).append(stateBox({ icon: 'alert', title: t('err.network'), action: { label: t('action.retry'), onclick: () => location.reload() } }));
    return;
  }
  S.session = s;
  $('#boot').hidden = true;
  if (!s.authenticated) return showSignIn(s);
  S.me = s.user;
  $('#app').hidden = false;
  buildChrome();
  window.addEventListener('hashchange', onRoute);
  onRoute();
  loadUsage();
}

function showSignIn(s) {
  $('#signin').hidden = false;
  const denied = new URLSearchParams(location.search).has('denied');
  $('#signin-denied').hidden = !denied;
  $('#signin-google').hidden = !s.google_login;
  const dev = $('#signin-dev');
  dev.hidden = !s.dev_login;
  dev.addEventListener('submit', (ev) => {
    ev.preventDefault();
    location.href = '/dev/login?email=' + encodeURIComponent($('#dev-email').value.trim());
  });
  const lang = $('#signin-lang');
  const paint = () => { lang.textContent = getLang() === 'vi' ? 'English' : 'Tiếng Việt'; };
  paint();
  lang.addEventListener('click', () => { setLang(getLang() === 'vi' ? 'en' : 'vi'); applyI18n(document); paint(); });
}

function applyTheme(theme) {
  if (theme === 'light' || theme === 'dark') document.documentElement.dataset.theme = theme;
  else delete document.documentElement.dataset.theme;
}

// ---------------------------------------------------------------- chrome

const NAV = [
  { name: 'files', href: '#/files', icon: 'drive', key: 'nav.files' },
  { name: 'shared', href: '#/shared', icon: 'people', key: 'nav.shared' },
  { name: 'trash', href: '#/trash', icon: 'trash', key: 'nav.trash' },
];

function buildChrome() {
  renderNav();
  $('#account-btn').textContent = (S.me.name || S.me.email).trim().charAt(0).toLocaleUpperCase();
  $('#account-btn').addEventListener('click', (ev) => accountMenu(ev));
  $('#new-btn').addEventListener('click', (ev) => newMenu(pointOf(ev, ev.currentTarget)));
  $('#fab').addEventListener('click', () => newMenu(null));
  $('#pick-files').addEventListener('change', (ev) => { queueFiles([...ev.target.files].map((f) => ({ file: f, path: [] }))); ev.target.value = ''; });
  $('#pick-folder').addEventListener('change', (ev) => {
    queueFiles([...ev.target.files].map((f) => ({ file: f, path: (f.webkitRelativePath || '').split('/').slice(0, -1) })));
    ev.target.value = '';
  });

  const search = $('#search');
  let timer = 0;
  search.addEventListener('input', () => {
    clearTimeout(timer);
    timer = setTimeout(() => { location.hash = '#/search/' + encodeURIComponent(search.value.trim()); }, 250);
  });
  search.addEventListener('focus', () => { if (S.route.name !== 'search') location.hash = '#/search/' + encodeURIComponent(search.value.trim()); });
  $('#search-form').addEventListener('submit', (ev) => { ev.preventDefault(); clearTimeout(timer); location.hash = '#/search/' + encodeURIComponent(search.value.trim()); });

  setupDrop();
  document.addEventListener('keydown', onKey);
  window.addEventListener('beforeunload', (ev) => {
    if (U.tasks.some((x) => x.state === 'uploading' || x.state === 'queued')) { ev.preventDefault(); ev.returnValue = ''; }
  });
}

function renderNav() {
  for (const host of [$('#side-nav'), $('#bottom-nav')]) {
    clear(host);
    for (const n of NAV) {
      host.append(el('a', { href: n.href, class: 'nav-item', 'data-nav': n.name }, icon(n.icon), el('span', { text: t(n.key) })));
    }
    if (S.session.curator) {
      host.append(el('a', { href: '/curator/', class: 'nav-item' }, icon('sparkles'), el('span', { text: t('nav.curator') })));
    }
  }
  $('#side-nav').append(el('a', { href: '#/sync', class: 'nav-item', 'data-nav': 'sync' }, icon('sync'), el('span', { text: t('nav.sync') })));
  if (S.me.is_admin) {
    $('#side-nav').append(el('a', { href: '#/admin', class: 'nav-item', 'data-nav': 'admin' }, icon('shield'), el('span', { text: t('nav.admin') })));
  }
  markNav();
}

function markNav() {
  let active = S.route.name;
  if (active === 'files' && S.crumbs.length && S.crumbs[0].id !== S.me.root_id) active = 'shared';
  for (const a of document.querySelectorAll('[data-nav]')) {
    const on = a.dataset.nav === active;
    a.classList.toggle('active', on);
    if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  }
}

function accountMenu(ev) {
  const theme = store.get('theme', 'auto');
  const themes = [['auto', 'auto', 'theme.auto'], ['light', 'sun', 'theme.light'], ['dark', 'moon', 'theme.dark']];
  openMenu([
    ...LANGS.map(([code, label]) => ({ icon: 'globe', label, checked: getLang() === code, onclick: () => changeLang(code) })),
    'sep',
    ...themes.map(([v, ic, key]) => ({ icon: ic, label: t(key), checked: theme === v, onclick: () => { store.set('theme', v); applyTheme(v); } })),
    'sep',
    { icon: 'sync', label: t('nav.sync'), onclick: () => { location.hash = '#/sync'; } },
    S.me.is_admin && { icon: 'shield', label: t('nav.admin'), onclick: () => { location.hash = '#/admin'; } },
    { icon: 'logout', label: t('account.signout'), onclick: signOut },
  ], pointOf(ev, ev.currentTarget), S.me.email);
}

function changeLang(code) {
  setLang(code);
  applyI18n(document);
  renderNav();
  render();
  renderTray();
  loadUsage();
}

async function signOut() {
  try { await api('POST', '/api/logout'); } catch { /* the cookie is gone either way after reload */ }
  location.href = '/';
}

async function loadUsage() {
  try {
    const u = await api('GET', '/api/usage');
    const box = clear($('#usage'));
    box.append(el('div', { text: t('usage.used', { size: fmtSize(u.used_bytes) }) }));
    if (S.me.is_admin && u.disk_total_bytes) {
      const pct = Math.round(100 * (1 - u.disk_free_bytes / u.disk_total_bytes));
      box.append(el('progress', { max: 100, value: pct, 'aria-label': t('usage.disk', { size: fmtSize(u.disk_free_bytes) }) }),
        el('div', { class: 'muted', text: t('usage.disk', { size: fmtSize(u.disk_free_bytes) }) }));
    }
  } catch { /* not essential */ }
}

// ---------------------------------------------------------------- routing

function parseRoute() {
  const parts = location.hash.replace(/^#\/?/, '').split('/');
  const name = parts[0] || 'files';
  if (name === 'files') return { name, id: parts[1] || S.me.root_id };
  if (name === 'search') return { name, q: decodeURIComponent(parts.slice(1).join('/') || '') };
  if (['shared', 'trash', 'admin', 'sync'].includes(name)) return { name };
  return { name: 'files', id: S.me.root_id };
}

function onRoute() {
  S.route = parseRoute();
  S.sel.clear();
  S.anchor = null;
  freshDevice = null;
  if (S.route.name !== 'search') $('#search').value = '';
  else if (document.activeElement !== $('#search')) $('#search').value = S.route.q;
  load(true);
}

async function load(showSkeleton) {
  const seq = ++S.loadSeq;
  S.error = null;
  if (showSkeleton) { S.loading = true; S.items = []; render(); }
  try {
    const r = S.route;
    if (r.name === 'files') {
      const d = await api('GET', `/api/nodes/${r.id}?sort=${S.sort}&order=${S.order}`);
      if (seq !== S.loadSeq) return;
      if (!d.node.is_dir) { // a link to a file: show its folder, or just the file
        S.folder = null; S.items = [d.node]; S.role = d.role; S.crumbs = d.breadcrumbs.slice(0, -1);
      } else {
        S.folder = d.node; S.items = d.children; S.role = d.role; S.crumbs = d.breadcrumbs;
      }
    } else if (r.name === 'shared') {
      const d = await api('GET', '/api/shared');
      if (seq !== S.loadSeq) return;
      S.items = d.items; S.folder = null; S.crumbs = [];
    } else if (r.name === 'trash') {
      const d = await api('GET', '/api/trash');
      if (seq !== S.loadSeq) return;
      S.items = d.items; S.folder = null; S.crumbs = [];
    } else if (r.name === 'search') {
      const d = await api('GET', '/api/search?q=' + encodeURIComponent(r.q));
      if (seq !== S.loadSeq) return;
      S.items = d.items; S.folder = null; S.crumbs = [];
    } else if (r.name === 'sync') {
      S.sync = await api('GET', '/api/app-passwords');
      if (seq !== S.loadSeq) return;
      S.items = []; S.folder = null; S.crumbs = [];
    } else if (r.name === 'admin') {
      S.admin = await api('GET', '/api/admin/allowlist');
      if (seq !== S.loadSeq) return;
      S.items = []; S.folder = null; S.crumbs = [];
    }
  } catch (e) {
    if (seq !== S.loadSeq) return;
    if (e instanceof ApiError && e.status === 401) { location.href = '/'; return; }
    S.error = e;
  }
  S.loading = false;
  for (const id of [...S.sel]) if (!S.items.some((x) => x.id === id)) S.sel.delete(id);
  const paint = () => { render(); markNav(); };
  // SQLite only folds ASCII case; names are collated here for Vietnamese.
  const r = S.route;
  if ((r.name === 'files' && S.sort === 'name') || r.name === 'shared' || (r.name === 'search' && r.q)) sortByName(S.items);
  if (showSkeleton) transition(paint); else paint();
}

const refresh = () => load(false);

// ---------------------------------------------------------------- render

const canWrite = () => S.route.name === 'files' && S.folder && (S.role === 'owner' || S.role === 'editor');

function render() {
  renderHead();
  renderBulk();
  const box = clear($('#content'));
  const name = S.route.name;
  document.title = t('app.name');
  $('#fab').hidden = !(name === 'files' && canWrite());
  $('#new-btn').disabled = !(name !== 'files' || canWrite());

  if (S.loading) { box.append(skeleton(S.view), el('p', { class: 'sr', role: 'status', text: t('state.loading') })); return; }
  if (S.error) {
    const gone = S.error instanceof ApiError && S.error.status === 404;
    box.append(stateBox({
      icon: 'alert', title: gone ? t('state.gone.title') : errorText(S.error), text: gone ? t('state.gone.text') : t('state.error.text'),
      action: gone ? { label: t('nav.files'), onclick: () => { location.hash = '#/files'; } } : { label: t('action.retry'), onclick: () => load(true) },
    }));
    return;
  }
  if (name === 'admin') { renderAdmin(box); return; }
  if (name === 'sync') { renderSync(box); return; }
  if (name === 'trash' && S.items.length) {
    box.append(el('p', { class: 'hint-line', text: S.session.trash_days > 0 ? t('trash.hint', { days: S.session.trash_days }) : t('trash.hint.never') }));
  }
  if (!S.items.length) { box.append(emptyState()); return; }

  const list = el('div', { class: 'items ' + S.view, role: 'list', 'aria-label': t('items.label') });
  if (S.view === 'list') {
    list.append(el('div', { class: 'list-head', 'aria-hidden': 'true' },
      el('span', { text: t('col.name') }), el('span', { class: 'item-date', text: name === 'trash' ? t('col.deleted') : t('col.modified') }), el('span', { class: 'item-size', text: t('col.size') })));
  }
  for (const node of S.items) {
    list.append(renderItem(node, {
      view: S.view,
      selectable: true,
      selected: S.sel.has(node.id),
      thumbUrl: (n) => `/api/nodes/${n.id}/thumb`,
      meta: itemMeta,
      onopen: onItemClick,
      onmenu: (n, ev, anchor) => itemMenu(n, pointOf(ev, anchor)),
      oncheck: (n, ev) => toggleSelect(n, ev.shiftKey),
      onlongpress: (n) => toggleSelect(n, false),
    }));
  }
  box.append(list);
}

function itemMeta(node) {
  if (S.route.name === 'shared') return t('shared.by', { owner: node.owner });
  // The desktop list has its own date and size columns.
  if (S.view === 'list' && matchMedia('(min-width: 720px)').matches) return '';
  if (node.is_dir) return t('item.folder');
  return fmtSize(node.size) + ' · ' + fmtDate(node.trashed_at || node.updated_at);
}

function emptyState() {
  const n = S.route.name;
  if (n === 'files') {
    return stateBox({
      icon: 'folder', title: t('empty.folder.title'), text: canWrite() ? t('empty.folder.text') : '',
      action: canWrite() ? { label: t('action.upload'), onclick: () => $('#pick-files').click() } : null,
    });
  }
  if (n === 'shared') return stateBox({ icon: 'people', title: t('empty.shared.title'), text: t('empty.shared.text') });
  if (n === 'trash') return stateBox({ icon: 'trash', title: t('empty.trash.title'), text: t('empty.trash.text') });
  if (S.route.q) return stateBox({ icon: 'search', title: t('empty.search.title'), text: t('empty.search.text') });
  return stateBox({ icon: 'clock', title: t('empty.recent.title'), text: t('empty.recent.text') });
}

function renderHead() {
  const crumbs = clear($('#crumbs'));
  const name = S.route.name;
  if (name === 'files') {
    const shared = S.crumbs.length && S.crumbs[0].id !== S.me.root_id;
    if (shared) crumbs.append(el('a', { href: '#/shared', class: 'crumb', text: t('nav.shared') }), icon('chevron-right', 'sep'));
    S.crumbs.forEach((c, i) => {
      const last = i === S.crumbs.length - 1;
      const label = c.id === S.me.root_id ? t('nav.files') : c.name;
      if (i > 0) crumbs.append(icon('chevron-right', 'sep'));
      crumbs.append(last ? el('h1', { class: 'crumb current', text: label }) : el('a', { href: '#/files/' + c.id, class: 'crumb', text: label }));
    });
    if (!S.crumbs.length) crumbs.append(el('h1', { class: 'crumb current', text: t('nav.files') }));
  } else {
    const titles = { shared: 'nav.shared', trash: 'nav.trash', admin: 'nav.admin', sync: 'nav.sync' };
    const title = name === 'search' ? (S.route.q ? t('search.results', { q: S.route.q }) : t('search.recent')) : t(titles[name]);
    crumbs.append(el('h1', { class: 'crumb current', text: title }));
  }

  const tools = clear($('#tools'));
  if (name === 'admin' || name === 'sync') return;
  if (name === 'trash' && S.items.length) {
    tools.append(el('button', { type: 'button', class: 'btn danger-ghost', text: t('trash.empty'), onclick: emptyTrash }));
  }
  if (name === 'files' && S.folder && S.role === 'owner' && S.folder.id !== S.me.root_id) {
    tools.append(iconButton('share', t('action.share'), () => shareDialog(S.folder)));
  }
  if (name === 'files') tools.append(iconButton('sort', t('sort.label'), (ev) => sortMenu(pointOf(ev, ev.currentTarget))));
  const next = S.view === 'grid' ? 'list' : 'grid';
  tools.append(iconButton(next, t(next === 'grid' ? 'view.grid' : 'view.list'), () => { S.view = next; store.set('view', next); render(); }));
}

function sortMenu(point) {
  const opts = [['name', 'asc', 'sort.name'], ['date', 'desc', 'sort.newest'], ['date', 'asc', 'sort.oldest'], ['size', 'desc', 'sort.largest']];
  openMenu(opts.map(([s, o, key]) => ({
    icon: 'sort', label: t(key), checked: S.sort === s && S.order === o,
    onclick: () => { S.sort = s; S.order = o; store.set('sort', s); store.set('order', o); refresh(); },
  })), point, t('sort.label'));
}

// ---------------------------------------------------------------- selection

function toggleSelect(node, range) {
  if (range && S.anchor) {
    const ids = S.items.map((x) => x.id);
    const [a, b] = [ids.indexOf(S.anchor), ids.indexOf(node.id)].sort((x, y) => x - y);
    if (a >= 0) for (const id of ids.slice(a, b + 1)) S.sel.add(id);
  } else if (S.sel.has(node.id)) {
    S.sel.delete(node.id);
  } else {
    S.sel.add(node.id);
  }
  S.anchor = node.id;
  render();
  const again = document.querySelector(`.item[data-id="${node.id}"]`);
  if (again) again.focus({ preventScroll: true });
}

function selected() { return S.items.filter((x) => S.sel.has(x.id)); }

function onItemClick(node, ev) {
  if (ev.shiftKey || ev.ctrlKey || ev.metaKey || S.sel.size) { toggleSelect(node, ev.shiftKey); return; }
  openItem(node);
}

function renderBulk() {
  const bar = clear($('#bulk'));
  const nodes = selected();
  bar.hidden = !nodes.length;
  document.body.classList.toggle('selecting', nodes.length > 0);
  if (!nodes.length) return;
  bar.append(iconButton('close', t('select.clear'), () => { S.sel.clear(); render(); }),
    el('span', { class: 'bulk-count', role: 'status', text: tn('select.count', nodes.length) }), el('span', { class: 'grow' }));
  for (const a of actionsFor(nodes)) {
    if (a.bulk) bar.append(iconButton(a.icon, a.label, a.onclick, a.danger ? 'danger' : ''));
  }
}

function onKey(ev) {
  if (document.querySelector('dialog[open]') || ev.target.closest('input, textarea, select')) return;
  if (ev.key === 'Escape' && S.sel.size) { S.sel.clear(); render(); }
  else if (ev.key === '/') { ev.preventDefault(); $('#search').focus(); }
  else if ((ev.ctrlKey || ev.metaKey) && ev.key.toLowerCase() === 'a' && S.items.length) { ev.preventDefault(); for (const x of S.items) S.sel.add(x.id); render(); }
  else if (ev.key === 'Delete' && S.sel.size) {
    const del = actionsFor(selected()).find((a) => a.key === 'trash' || a.key === 'purge');
    if (del) del.onclick();
  }
}

// ---------------------------------------------------------------- actions

// The one place that decides which actions exist for a set of nodes.
function actionsFor(nodes) {
  const one = nodes.length === 1 ? nodes[0] : null;
  const name = S.route.name;
  if (name === 'trash') {
    return [
      { key: 'restore', icon: 'restore', label: t('action.restore'), bulk: true, onclick: () => restore(nodes) },
      { key: 'purge', icon: 'trash', label: t('action.purge'), bulk: true, danger: true, onclick: () => purge(nodes) },
    ];
  }
  const inFolder = name === 'files' && !!S.folder;
  const write = inFolder && canWrite();
  const owner = inFolder && S.role === 'owner';
  return [
    one && { key: 'open', icon: one.is_dir ? 'folder' : 'open', label: t('action.open'), onclick: () => openItem(one) },
    { key: 'download', icon: 'download', label: t('action.download'), bulk: true, onclick: () => download(nodes) },
    one && owner && { key: 'share', icon: 'share', label: t('action.share'), bulk: true, onclick: () => shareDialog(one) },
    one && write && { key: 'rename', icon: 'edit', label: t('action.rename'), onclick: () => rename(one) },
    write && { key: 'move', icon: 'move', label: t('action.move'), bulk: true, onclick: () => move(nodes) },
    one && !one.is_dir && { key: 'copy', icon: 'copy', label: t('action.copy'), onclick: () => copy(one) },
    write && { key: 'trash', icon: 'trash', label: t('action.trash'), bulk: true, onclick: () => trash(nodes) },
  ].filter(Boolean);
}

function itemMenu(node, point) {
  // Acting on an item that is part of the selection acts on the selection.
  const nodes = S.sel.has(node.id) && S.sel.size > 1 ? selected() : [node];
  const title = nodes.length === 1 ? node.name : tn('select.count', nodes.length);
  openMenu(actionsFor(nodes), point, title);
}

function openItem(node) {
  if (S.route.name === 'trash') { toast(t('trash.restore.first')); return; }
  if (node.is_dir) { location.hash = '#/files/' + node.id; return; }
  const files = S.items.filter((x) => !x.is_dir);
  openLightbox({
    items: files,
    index: files.indexOf(node),
    urlFor: (n, dl) => `/api/nodes/${n.id}/content${dl ? '?download=1' : ''}`,
    actions: (n, close) => actionsFor([n]).filter((a) => ['download', 'share', 'trash'].includes(a.key))
      .map((a) => ({ ...a, onclick: () => { if (a.key !== 'download') close(); a.onclick(); } })),
  });
}

function download(nodes) {
  // One click per item; the browser saves each (folders arrive as a zip).
  nodes.forEach((n, i) => setTimeout(() => {
    const a = el('a', { href: n.is_dir ? `/api/nodes/${n.id}/zip` : `/api/nodes/${n.id}/content?download=1`, download: '' });
    document.body.append(a);
    a.click();
    a.remove();
  }, i * 400));
  if (nodes.length > 1 || nodes[0].is_dir) toast(t('download.started'));
}

function newFolder() {
  const parent = S.folder.id;
  promptDialog({
    title: t('folder.new.title'), label: t('folder.new.label'), value: t('folder.new.default'), confirmLabel: t('action.create'),
    submit: async (name) => {
      await api('POST', `/api/nodes/${parent}/children`, { name });
      toast(t('folder.new.done', { name }));
      refresh();
    },
  });
}

function rename(node) {
  const old = node.name;
  promptDialog({
    title: t('rename.title'), label: t('rename.label'), value: old, confirmLabel: t('action.rename'), selectBase: !node.is_dir,
    submit: async (name) => {
      if (name === old) return;
      await api('PATCH', `/api/nodes/${node.id}`, { name });
      refresh();
      toast(t('rename.done', { name }), {
        action: t('action.undo'),
        onAction: () => api('PATCH', `/api/nodes/${node.id}`, { name: old }).then(refresh, toastError),
      });
    },
  });
}

async function trash(nodes) {
  // Reversible: happens immediately, with Undo. No confirmation dialog.
  const ids = new Set(nodes.map((n) => n.id));
  S.items = S.items.filter((x) => !ids.has(x.id));
  S.sel.clear();
  render();
  const done = [];
  let failed = null;
  for (const n of nodes) {
    try { await api('DELETE', `/api/nodes/${n.id}`); done.push(n); } catch (e) { failed = e; }
  }
  if (failed) { toastError(failed); refresh(); return; }
  toast(tn('trash.done', done.length, { name: done[0].name }), {
    action: t('action.undo'),
    onAction: async () => {
      try {
        for (const n of done) await api('POST', `/api/trash/${n.id}/restore`);
        toast(t('undo.done'));
      } catch (e) { toastError(e); }
      refresh();
    },
  });
  loadUsage();
}

async function move(nodes) {
  const from = S.folder.id;
  const blocked = new Set(nodes.filter((n) => n.is_dir).map((n) => n.id));
  const dest = await pickFolder({ title: tn('move.title', nodes.length, { name: nodes[0].name }), startId: from, confirmLabel: t('move.here'), blocked, notId: from });
  if (!dest) return;
  const moved = [];
  let failed = null;
  for (const n of nodes) {
    try { await api('PATCH', `/api/nodes/${n.id}`, { parent_id: dest.id }); moved.push(n); } catch (e) { failed = e; }
  }
  S.sel.clear();
  refresh();
  if (failed) toastError(failed);
  if (!moved.length) return;
  if (!failed) {
    toast(tn('move.done', moved.length, { name: moved[0].name, folder: dest.name }), {
      action: t('action.undo'),
      onAction: async () => {
        try { for (const n of moved) await api('PATCH', `/api/nodes/${n.id}`, { parent_id: from }); toast(t('undo.done')); } catch (e) { toastError(e); }
        refresh();
      },
    });
  }
}

async function copy(node) {
  const start = canWrite() ? S.folder.id : S.me.root_id;
  const dest = await pickFolder({ title: t('copy.title', { name: node.name }), startId: start, confirmLabel: t('copy.here'), blocked: new Set() });
  if (!dest) return;
  try {
    await api('POST', `/api/nodes/${node.id}/copy`, { parent_id: dest.id });
    toast(t('copy.done', { folder: dest.name }));
    refresh();
    loadUsage();
  } catch (e) { toastError(e); }
}

async function restore(nodes) {
  let failed = null, toRoot = false;
  for (const n of nodes) {
    try { const r = await api('POST', `/api/trash/${n.id}/restore`); toRoot = toRoot || r.moved_to_root; } catch (e) { failed = e; }
  }
  S.sel.clear();
  refresh();
  if (failed) toastError(failed);
  else toast(toRoot ? t('restore.done.root') : tn('restore.done', nodes.length, { name: nodes[0].name }));
}

async function purge(nodes) {
  // Irreversible: this is one of the two places that ask for confirmation.
  const ok = await confirmDanger({
    title: tn('purge.title', nodes.length, { name: nodes[0].name }),
    message: t('purge.text'),
    confirmLabel: t('action.purge'),
  });
  if (!ok) return;
  let failed = null;
  for (const n of nodes) {
    try { await api('DELETE', `/api/trash/${n.id}`); } catch (e) { failed = e; }
  }
  S.sel.clear();
  refresh();
  loadUsage();
  if (failed) toastError(failed); else toast(tn('purge.done', nodes.length));
}

async function emptyTrash() {
  const ok = await confirmDanger({
    title: t('trash.empty.title'),
    message: tn('trash.empty.text', S.items.length),
    confirmLabel: t('trash.empty.confirm'),
  });
  if (!ok) return;
  try {
    await api('DELETE', '/api/trash');
    toast(t('trash.empty.done'));
  } catch (e) { toastError(e); }
  refresh();
  loadUsage();
}

// Folder picker for move and copy. Resolves to {id, name} or null.
function pickFolder({ title, startId, confirmLabel, blocked, notId }) {
  return new Promise((resolve) => {
    let result = null, current = null;
    const path = el('nav', { class: 'crumbs small' });
    const list = el('div', { class: 'picker-list', role: 'list' });
    const body = el('div', { class: 'picker' }, path, list);
    const d = openDialog({
      title, body, onclose: () => resolve(result),
      actions: [
        { label: t('action.cancel'), kind: 'ghost', onclick: (close) => close() },
        { label: confirmLabel, kind: 'primary', onclick: (close) => { result = { id: current.node.id, name: nameOf(current.node) }; close(); } },
      ],
    });
    const okBtn = d.foot.lastChild;
    const nameOf = (n) => (n.id === S.me.root_id ? t('nav.files') : n.name);
    async function show(id) {
      okBtn.disabled = true;
      clear(list).append(el('p', { class: 'muted pad', text: t('state.loading') }));
      try {
        current = await api('GET', `/api/nodes/${id}`);
      } catch (e) {
        clear(list).append(el('p', { class: 'field-error pad', text: errorText(e) }));
        return;
      }
      clear(path);
      if (current.breadcrumbs[0].id !== S.me.root_id) {
        path.append(el('button', { type: 'button', class: 'crumb', text: t('nav.files'), onclick: () => show(S.me.root_id) }), icon('chevron-right', 'sep'));
      }
      current.breadcrumbs.forEach((c, i) => {
        if (i > 0) path.append(icon('chevron-right', 'sep'));
        const last = i === current.breadcrumbs.length - 1;
        path.append(last ? el('span', { class: 'crumb current', text: nameOf(c) }) : el('button', { type: 'button', class: 'crumb', text: nameOf(c), onclick: () => show(c.id) }));
      });
      clear(list);
      const dirs = current.children.filter((c) => c.is_dir && !blocked.has(c.id));
      for (const c of dirs) {
        list.append(el('button', { type: 'button', class: 'picker-row', role: 'listitem', onclick: () => show(c.id) }, icon('folder', 'kind folder'), el('span', { text: c.name }), icon('chevron-right', 'end')));
      }
      if (!dirs.length) list.append(el('p', { class: 'muted pad', text: t('picker.empty') }));
      okBtn.disabled = current.role === 'viewer' || current.node.id === notId;
    }
    show(startId);
  });
}

// ---------------------------------------------------------------- share dialog

async function shareDialog(node) {
  const summary = el('p', { class: 'share-summary', role: 'status' });
  const chips = el('div', { class: 'chips' });
  const emailInput = el('input', { type: 'email', class: 'chip-input', id: 'share-email', autocomplete: 'off', placeholder: t('share.email.placeholder'), 'aria-describedby': 'share-error' });
  const roleSel = roleSelect('viewer', 'share-role');
  const error = el('p', { class: 'field-error', id: 'share-error', role: 'alert', hidden: true });
  const addBtn = el('button', { type: 'submit', class: 'btn primary', text: t('share.send') });
  const people = el('div', { class: 'people', role: 'list' });
  const linkBox = el('div', { class: 'links' });
  const pending = [];

  const emailField = el('div', { class: 'chip-field', onclick: () => emailInput.focus() }, chips, emailInput);
  const form = el('form', { class: 'form share-form', novalidate: true },
    el('label', { for: 'share-email', text: t('share.email.label') }),
    emailField,
    el('div', { class: 'row' }, el('label', { class: 'sr', for: 'share-role', text: t('share.role.label') }), roleSel, addBtn),
    error);

  const expiry = el('select', { class: 'input', id: 'link-expiry' },
    ...[['0', 'link.never'], ['1', 'link.1day'], ['7', 'link.7days'], ['30', 'link.30days']].map(([v, k]) => el('option', { value: v, text: t(k) })));
  expiry.value = '7';
  const password = el('input', { type: 'text', class: 'input', id: 'link-pw', autocomplete: 'off', placeholder: t('link.password.placeholder') });
  const linkBtn = el('button', { type: 'button', class: 'btn tonal' }, icon('link'), el('span', { text: t('link.create') }));
  const linkSection = el('section', { class: 'share-section' },
    el('h3', { text: t('link.title') }),
    el('p', { class: 'muted', text: t('link.text') }),
    el('div', { class: 'row wrap' },
      el('div', { class: 'field' }, el('label', { for: 'link-expiry', text: t('link.expiry') }), expiry),
      el('div', { class: 'field grow' }, el('label', { for: 'link-pw', text: t('link.password') }), password)),
    linkBtn, linkBox);

  openDialog({
    title: t('share.title', { name: node.name }),
    cls: 'share',
    body: el('div', {}, summary, form, el('section', { class: 'share-section' }, el('h3', { text: t('share.people') }), people), linkSection),
    actions: [{ label: t('action.done'), kind: 'primary', onclick: (close) => close() }],
    onclose: refresh,
  });

  function showError(msg) { error.textContent = msg; error.hidden = !msg; }
  function addChip(raw) {
    const email = raw.trim().replace(/[,;]$/, '').toLowerCase();
    if (!email) return true;
    if (!/^[^\s@,;<>"]+@[^\s@,;<>"]+\.[^\s@,;<>"]+$/.test(email)) { showError(t('share.email.invalid', { email })); return false; }
    if (!pending.includes(email)) pending.push(email);
    showError('');
    paintChips();
    return true;
  }
  function paintChips() {
    clear(chips);
    for (const email of pending) {
      chips.append(el('span', { class: 'chip' }, el('span', { text: email }),
        iconButton('close', t('share.chip.remove', { email }), () => { pending.splice(pending.indexOf(email), 1); paintChips(); }, 'tiny')));
    }
  }
  emailInput.addEventListener('keydown', (ev) => {
    if ([',', ';', ' ', 'Enter'].includes(ev.key) && emailInput.value.trim()) {
      ev.preventDefault();
      if (addChip(emailInput.value)) emailInput.value = '';
    } else if (ev.key === 'Backspace' && !emailInput.value && pending.length) {
      pending.pop();
      paintChips();
    }
  });
  // Phone keyboards and autofill do not always send keydown: watch the text too.
  emailInput.addEventListener('input', () => {
    if (/[,;\s]$/.test(emailInput.value) && emailInput.value.trim() && addChip(emailInput.value)) emailInput.value = '';
  });
  emailInput.addEventListener('paste', (ev) => {
    const text = ev.clipboardData.getData('text');
    if (!/[,;\s]/.test(text.trim())) return;
    ev.preventDefault();
    text.split(/[,;\s]+/).forEach(addChip);
  });
  emailInput.addEventListener('blur', () => { if (emailInput.value.trim() && addChip(emailInput.value)) emailInput.value = ''; });
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (emailInput.value.trim()) { if (!addChip(emailInput.value)) return; emailInput.value = ''; }
    if (!pending.length) { showError(t('share.email.empty')); emailInput.focus(); return; }
    addBtn.disabled = true;
    try {
      await api('POST', `/api/nodes/${node.id}/shares`, { emails: pending, role: roleSel.value });
      toast(tn('share.done', pending.length, { email: pending[0] }));
      pending.length = 0;
      paintChips();
      await loadPeople();
    } catch (e) {
      showError(errorText(e));
    }
    addBtn.disabled = false;
  });

  let shares = [], links = [];
  function paintSummary() {
    const parts = [];
    if (!shares.length && !links.length) parts.push(t('share.summary.private'));
    if (shares.length) parts.push(tn('share.summary.people', shares.length));
    const live = links.filter((l) => !l.expired).length;
    if (live) parts.push(tn('share.summary.links', live));
    summary.textContent = parts.join(' ');
  }
  async function loadPeople() {
    try {
      shares = (await api('GET', `/api/nodes/${node.id}/shares`)).shares;
    } catch (e) { clear(people).append(el('p', { class: 'field-error', text: errorText(e) })); return; }
    clear(people);
    people.append(el('div', { class: 'person', role: 'listitem' }, el('span', { class: 'avatar small', text: S.me.email.charAt(0).toUpperCase() }),
      el('span', { class: 'person-email', text: S.me.email }), el('span', { class: 'muted', text: t('share.role.owner') })));
    for (const sh of shares) {
      const sel = roleSelect(sh.role, 'role-' + sh.id);
      sel.setAttribute('aria-label', t('share.role.for', { email: sh.email }));
      sel.addEventListener('change', async () => {
        try { await api('POST', `/api/nodes/${node.id}/shares`, { email: sh.email, role: sel.value }); toast(t('share.role.changed')); } catch (e) { toastError(e); loadPeople(); }
      });
      people.append(el('div', { class: 'person', role: 'listitem' }, el('span', { class: 'avatar small', text: sh.email.charAt(0).toUpperCase() }),
        el('span', { class: 'person-email', text: sh.email }), sel,
        iconButton('close', t('share.remove', { email: sh.email }), async () => {
          try {
            await api('DELETE', `/api/shares/${sh.id}`);
            toast(t('share.removed', { email: sh.email }), {
              action: t('action.undo'),
              onAction: () => api('POST', `/api/nodes/${node.id}/shares`, { email: sh.email, role: sh.role }).then(loadPeople, toastError),
            });
          } catch (e) { toastError(e); }
          loadPeople();
        })));
    }
    paintSummary();
  }
  async function loadLinks(fresh) {
    try {
      links = (await api('GET', `/api/nodes/${node.id}/links`)).links;
    } catch (e) { clear(linkBox).append(el('p', { class: 'field-error', text: errorText(e) })); return; }
    clear(linkBox);
    if (fresh) {
      const urlInput = el('input', { class: 'input', readonly: true, value: fresh.url, 'aria-label': t('link.title') });
      urlInput.addEventListener('focus', () => urlInput.select());
      linkBox.append(el('div', { class: 'fresh-link' }, urlInput,
        el('button', { type: 'button', class: 'btn primary', text: t('link.copy'), onclick: () => copyText(fresh.url, urlInput) })),
      el('p', { class: 'muted', text: t('link.once') }));
    }
    for (const l of links) {
      const bits = [t('link.created', { date: fmtDate(l.created_at) })];
      bits.push(l.expired ? t('link.expired') : l.expires_at ? t('link.expires', { date: fmtDate(l.expires_at) }) : t('link.noexpiry'));
      if (l.has_password) bits.push(t('link.haspw'));
      linkBox.append(el('div', { class: 'person' + (l.expired ? ' off' : ''), role: 'listitem' }, icon(l.has_password ? 'lock' : 'link'),
        el('span', { class: 'person-email', text: bits.join(' · ') }),
        el('button', { type: 'button', class: 'btn text', text: t('link.revoke'), onclick: async () => {
          try { await api('DELETE', `/api/links/${l.id}`); toast(t('link.revoked')); } catch (e) { toastError(e); }
          loadLinks();
        } })));
    }
    paintSummary();
  }
  linkBtn.addEventListener('click', async () => {
    linkBtn.disabled = true;
    try {
      const body = {};
      if (expiry.value !== '0') body.expires_in_days = Number(expiry.value);
      if (password.value) body.password = password.value;
      const r = await api('POST', `/api/nodes/${node.id}/links`, body);
      password.value = '';
      await loadLinks(r.link);
      copyText(r.link.url);
    } catch (e) { toastError(e); }
    linkBtn.disabled = false;
  });
  loadPeople();
  loadLinks();
  emailInput.focus();
}

function roleSelect(value, id) {
  const sel = el('select', { class: 'input role', id },
    el('option', { value: 'viewer', text: t('share.role.viewer') }), el('option', { value: 'editor', text: t('share.role.editor') }));
  sel.value = value;
  return sel;
}

async function copyText(text, input) {
  try {
    await navigator.clipboard.writeText(text);
    toast(t('link.copied'));
  } catch {
    if (input) { input.focus(); input.select(); }
    toast(t('link.copy.manual'));
  }
}

// ---------------------------------------------------------------- admin

function renderAdmin(box) {
  const input = el('input', { type: 'email', class: 'input', id: 'allow-email', required: true, autocomplete: 'off', placeholder: t('share.email.placeholder') });
  const error = el('p', { class: 'field-error', role: 'alert', hidden: true });
  const form = el('form', { class: 'form row wrap', novalidate: true }, el('label', { class: 'sr', for: 'allow-email', text: t('admin.add.label') }), input,
    el('button', { type: 'submit', class: 'btn primary', text: t('admin.add') }));
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    error.hidden = true;
    try {
      await api('POST', '/api/admin/allowlist', { email: input.value });
      toast(t('admin.added', { email: input.value.trim() }));
      load(false);
    } catch (e) { error.textContent = errorText(e); error.hidden = false; input.focus(); }
  });
  const list = el('div', { class: 'people', role: 'list' });
  for (const a of S.admin.admins) {
    list.append(el('div', { class: 'person', role: 'listitem' }, el('span', { class: 'avatar small', text: a.charAt(0).toUpperCase() }),
      el('span', { class: 'person-email', text: a }), el('span', { class: 'muted', text: t('admin.role.admin') })));
  }
  for (const it of S.admin.items) {
    list.append(el('div', { class: 'person', role: 'listitem' }, el('span', { class: 'avatar small', text: it.email.charAt(0).toUpperCase() }),
      el('span', { class: 'person-email', text: it.email }),
      iconButton('close', t('share.remove', { email: it.email }), async () => {
        try {
          await api('DELETE', '/api/admin/allowlist?email=' + encodeURIComponent(it.email));
          toast(t('admin.removed', { email: it.email }), {
            action: t('action.undo'),
            onAction: () => api('POST', '/api/admin/allowlist', { email: it.email }).then(() => load(false), toastError),
          });
        } catch (e) { toastError(e); }
        load(false);
      })));
  }
  box.append(el('div', { class: 'panel' }, el('h2', { text: t('admin.title') }), el('p', { class: 'muted', text: t('admin.text') }), form, error, list));
}

// ---------------------------------------------------------------- sync devices (WebDAV app passwords)

let freshDevice = null; // the password just created; shown once

function copyField(label, value, id) {
  const input = el('input', { class: 'input', id, readonly: true, value });
  input.addEventListener('focus', () => input.select());
  return el('div', { class: 'field' }, el('label', { for: id, text: label }),
    el('div', { class: 'fresh-link' }, input,
      el('button', { type: 'button', class: 'btn tonal', text: t('link.copy'), onclick: () => copyValue(value, input) })));
}

async function copyValue(text, input) {
  try {
    await navigator.clipboard.writeText(text);
    toast(t('sync.copied'));
  } catch {
    input.focus();
    input.select();
    toast(t('link.copy.manual'));
  }
}

function renderSync(box) {
  const d = S.sync;
  const name = el('input', { type: 'text', class: 'input', id: 'device-name', required: true, maxlength: 60, autocomplete: 'off', placeholder: t('sync.device.placeholder') });
  const error = el('p', { class: 'field-error', role: 'alert', hidden: true });
  const form = el('form', { class: 'form', novalidate: true },
    el('label', { for: 'device-name', text: t('sync.device.label') }),
    el('div', { class: 'row wrap' }, el('div', { class: 'grow' }, name), el('button', { type: 'submit', class: 'btn primary', text: t('sync.device.create') })),
    error);
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    error.hidden = true;
    if (!name.value.trim()) { error.textContent = t('sync.device.empty'); error.hidden = false; name.focus(); return; }
    try {
      freshDevice = (await api('POST', '/api/app-passwords', { name: name.value })).item;
      load(false);
    } catch (e) { error.textContent = errorText(e); error.hidden = false; name.focus(); }
  });

  const devices = el('div', { class: 'people', role: 'list' });
  for (const it of d.items) {
    const used = it.last_used_at ? t('sync.used', { date: fmtDate(it.last_used_at) }) : t('sync.unused');
    devices.append(el('div', { class: 'person', role: 'listitem' }, icon('sync'),
      el('span', { class: 'person-email' }, el('strong', { text: it.name }), el('br'), el('span', { class: 'muted', text: t('link.created', { date: fmtDate(it.created_at) }) + ' · ' + used })),
      el('button', { type: 'button', class: 'btn text', text: t('sync.revoke'), onclick: async () => {
        // Cannot be undone (the password is not stored), so this one asks.
        const ok = await confirmDanger({ title: t('sync.revoke.title', { name: it.name }), message: t('sync.revoke.text'), confirmLabel: t('sync.revoke') });
        if (!ok) return;
        try { await api('DELETE', `/api/app-passwords/${it.id}`); toast(t('sync.revoked', { name: it.name })); } catch (e) { toastError(e); }
        if (freshDevice && freshDevice.id === it.id) freshDevice = null;
        load(false);
      } })));
  }
  if (!d.items.length) devices.append(el('p', { class: 'muted', text: t('sync.none') }));

  const fresh = freshDevice ? el('div', { class: 'notice' },
    el('strong', { text: t('sync.fresh.title', { name: freshDevice.name }) }),
    copyField(t('sync.password'), freshDevice.password, 'fresh-password'),
    el('span', { class: 'muted', text: t('sync.fresh.once') })) : null;

  const how = (title, text) => el('details', { class: 'how' }, el('summary', { text: title }), el('p', { text }));
  box.append(
    el('div', { class: 'panel' },
      el('h2', { text: t('sync.title') }), el('p', { class: 'muted', text: t('sync.intro') }),
      copyField(t('sync.address'), d.dav_url, 'dav-url'),
      copyField(t('sync.username'), d.username, 'dav-user'),
      el('p', { class: 'muted', text: t('sync.password.hint') })),
    el('div', { class: 'panel' }, el('h2', { text: t('sync.devices') }), form, fresh, devices),
    el('div', { class: 'panel' }, el('h2', { text: t('sync.good.title') }),
      el('ul', { class: 'plain-list' },
        el('li', { text: t('sync.good.large') }),
        el('li', { text: t('sync.good.trash') }),
        el('li', { text: S.session.trash_days > 0 ? t('sync.good.versions', { days: S.session.trash_days }) : t('sync.good.versions.never') }),
        el('li', { text: t('sync.good.shared') }))),
    el('div', { class: 'panel' }, el('h2', { text: t('sync.how.title') }),
      how(t('sync.how.android'), t('sync.how.android.text')),
      how(t('sync.how.ios'), t('sync.how.ios.text')),
      how(t('sync.how.windows'), t('sync.how.windows.text')),
      how(t('sync.how.mac'), t('sync.how.mac.text')),
      how(t('sync.how.linux'), t('sync.how.linux.text'))));
  if (freshDevice) { const f = $('#fresh-password', box); if (f) f.focus(); }
}

// ---------------------------------------------------------------- new / upload

function newMenu(point) {
  if (!canWrite()) {
    if (S.route.name !== 'files') { location.hash = '#/files'; toast(t('upload.goto')); } else toast(t('err.forbidden'));
    return;
  }
  openMenu([
    { icon: 'upload', label: t('action.upload'), onclick: () => $('#pick-files').click() },
    'webkitdirectory' in HTMLInputElement.prototype && { icon: 'folder-up', label: t('action.upload.folder'), onclick: () => $('#pick-folder').click() },
    { icon: 'folder-plus', label: t('action.newfolder'), onclick: newFolder },
  ], point, t('action.new'));
}

function setupDrop() {
  const drop = $('#drop');
  let depth = 0;
  const hasFiles = (ev) => ev.dataTransfer && [...ev.dataTransfer.types].includes('Files');
  window.addEventListener('dragenter', (ev) => { if (!hasFiles(ev)) return; ev.preventDefault(); depth++; if (canWrite()) drop.hidden = false; });
  window.addEventListener('dragover', (ev) => { if (hasFiles(ev)) ev.preventDefault(); });
  window.addEventListener('dragleave', (ev) => { if (!hasFiles(ev)) return; depth = Math.max(0, depth - 1); if (!depth) drop.hidden = true; });
  window.addEventListener('drop', async (ev) => {
    if (!hasFiles(ev)) return;
    ev.preventDefault();
    depth = 0;
    drop.hidden = true;
    if (!canWrite()) { toast(t('upload.nowrite')); return; }
    const parent = S.folder.id;
    const entries = [...ev.dataTransfer.items].map((it) => (it.webkitGetAsEntry ? it.webkitGetAsEntry() : null));
    const plain = [...ev.dataTransfer.files];
    if (entries.some((e) => e && e.isDirectory)) {
      const out = [];
      for (const e of entries) if (e) await walkEntry(e, [], out);
      queueFiles(out, parent);
    } else {
      queueFiles(plain.map((f) => ({ file: f, path: [] })), parent);
    }
  });
}

async function walkEntry(entry, path, out) {
  if (entry.isFile) {
    const file = await new Promise((res, rej) => entry.file(res, rej)).catch(() => null);
    if (file) out.push({ file, path });
    return;
  }
  const reader = entry.createReader();
  for (;;) {
    const batch = await new Promise((res) => reader.readEntries(res, () => res([])));
    if (!batch.length) break;
    for (const child of batch) await walkEntry(child, [...path, entry.name], out);
  }
}

// ---- upload manager: chunked, resumable, survives navigation inside the app

const U = { tasks: [], active: 0, max: 2, collapsed: false, dirs: new Map(), raf: 0, refreshTimer: 0 };

function queueFiles(list, parentId) {
  if (!list.length) return;
  const parent = parentId || (S.folder && S.folder.id);
  if (!parent) return;
  for (const { file, path } of list) {
    U.tasks.push({ file, path, root: parent, name: file.name, size: file.size, sent: 0, state: 'queued', uploadId: null, chunk: 0, xhr: null, error: '' });
  }
  U.collapsed = false;
  renderTray();
  pump();
}

function pump() {
  while (U.active < U.max) {
    const task = U.tasks.find((x) => x.state === 'queued');
    if (!task) break;
    U.active++;
    runTask(task).finally(() => { U.active--; pump(); renderTray(); });
  }
}

// Creates (or finds) the folders of a dropped/picked directory tree.
function ensureDir(parentId, name) {
  const key = parentId + '/' + name;
  if (!U.dirs.has(key)) {
    U.dirs.set(key, (async () => {
      try {
        return (await api('POST', `/api/nodes/${parentId}/children`, { name })).node.id;
      } catch (e) {
        if (e.code !== 'name_conflict') throw e;
        const found = (await api('GET', `/api/nodes/${parentId}`)).children.find((c) => c.is_dir && c.name === name);
        if (!found) throw e;
        return found.id;
      }
    })());
    U.dirs.get(key).catch(() => U.dirs.delete(key));
  }
  return U.dirs.get(key);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function runTask(task) {
  task.state = 'uploading';
  task.error = '';
  renderTray();
  try {
    if (!task.parent) {
      let parent = task.root;
      for (const seg of task.path) parent = await ensureDir(parent, seg);
      task.parent = parent;
    }
    if (task.uploadId) {
      // Resume: ask the server how much it has.
      try {
        task.sent = (await api('GET', `/api/uploads/${task.uploadId}`)).offset;
      } catch (e) {
        if (e.status !== 404) throw e;
        task.uploadId = null; // session expired on the server: start over
      }
    }
    if (!task.uploadId) {
      const r = await api('POST', '/api/uploads', { parent_id: task.parent, name: task.name, size: task.size, on_conflict: 'rename' });
      task.uploadId = r.id;
      task.chunk = r.chunk_size;
      task.sent = 0;
    }
    let attempts = 0;
    while (task.sent < task.size) {
      if (task.state !== 'uploading') return;
      const end = Math.min(task.sent + task.chunk, task.size);
      try {
        await putChunk(task, task.sent, end);
        task.sent = end;
        task.progress = end;
        attempts = 0;
      } catch (e) {
        if (task.state !== 'uploading') return; // paused or cancelled
        if (e.status === 409 && e.data && typeof e.data.offset === 'number') { task.sent = e.data.offset; continue; }
        if ((e.status >= 400 && e.status < 500) || ++attempts > 4) throw e;
        await sleep(800 * 2 ** attempts);
        // After a failed chunk, continue from what the server really has.
        try { task.sent = (await api('GET', `/api/uploads/${task.uploadId}`)).offset; } catch { /* retry the same offset */ }
      }
      renderTray();
    }
    if (task.state !== 'uploading') return;
    await api('POST', `/api/uploads/${task.uploadId}/complete`);
    task.state = 'done';
    task.uploadId = null;
    clearTimeout(U.refreshTimer);
    U.refreshTimer = setTimeout(() => { if (S.route.name === 'files') refresh(); loadUsage(); }, 400);
  } catch (e) {
    if (task.state !== 'uploading') return;
    task.state = 'error';
    task.error = errorText(e instanceof ApiError ? e : new ApiError(e.status || 0, (e.data && e.data.error) || (e.status ? 'error' : 'network'), e.data));
    if (e.status === 404 || e.status === 422) task.uploadId = null;
  }
}

function putChunk(task, start, end) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    task.xhr = xhr;
    xhr.open('PUT', `/api/uploads/${task.uploadId}/chunk?offset=${start}`);
    xhr.setRequestHeader('X-Drive-Csrf', '1');
    xhr.setRequestHeader('Content-Type', 'application/octet-stream');
    xhr.upload.onprogress = (ev) => { task.progress = start + ev.loaded; scheduleTray(); };
    const fail = () => {
      let data = null;
      try { data = JSON.parse(xhr.responseText); } catch { /* not JSON */ }
      reject({ status: xhr.status, data });
    };
    xhr.onload = () => (xhr.status === 200 ? resolve() : fail());
    xhr.onerror = fail;
    xhr.onabort = fail;
    xhr.ontimeout = fail;
    xhr.send(task.file.slice(start, end));
  });
}

function pauseTask(task) { task.state = 'paused'; if (task.xhr) task.xhr.abort(); renderTray(); }
function resumeTask(task) { task.state = 'queued'; renderTray(); pump(); }
function cancelTask(task) {
  const id = task.uploadId;
  task.state = 'cancelled';
  if (task.xhr) task.xhr.abort();
  U.tasks.splice(U.tasks.indexOf(task), 1);
  if (id) setTimeout(() => api('DELETE', `/api/uploads/${id}`).catch(() => {}), 300);
  renderTray();
}

function scheduleTray() {
  if (U.raf) return;
  U.raf = requestAnimationFrame(() => { U.raf = 0; renderTray(); });
}

function renderTray() {
  const tray = $('#tray');
  if (!U.tasks.length) { tray.hidden = true; clear(tray); return; }
  tray.hidden = false;
  const done = U.tasks.filter((x) => x.state === 'done').length;
  const failed = U.tasks.filter((x) => x.state === 'error').length;
  const busy = U.tasks.some((x) => x.state === 'uploading' || x.state === 'queued');
  const total = U.tasks.reduce((a, x) => a + x.size, 0) || 1;
  const sent = U.tasks.reduce((a, x) => a + (x.state === 'done' ? x.size : Math.min(x.progress || x.sent, x.size)), 0);
  let title;
  if (busy) title = t('upload.progress', { done, total: U.tasks.length });
  else if (failed) title = tn('upload.failed', failed);
  else if (U.tasks.some((x) => x.state === 'paused')) title = t('upload.paused');
  else title = tn('upload.done', done);

  // Rebuild only when the set of rows changes; otherwise update in place so
  // focus and hover survive progress ticks.
  const sig = U.tasks.map((x) => x.name + x.state).join('|') + U.collapsed + getLang();
  if (tray.dataset.sig !== sig) {
    tray.dataset.sig = sig;
    clear(tray);
    const head = el('header', { class: 'tray-head' },
      el('div', { class: 'tray-title', role: 'status' }),
      iconButton(U.collapsed ? 'chevron-up' : 'chevron-down', t(U.collapsed ? 'upload.expand' : 'upload.collapse'), () => { U.collapsed = !U.collapsed; renderTray(); }),
      !busy ? iconButton('close', t('action.close'), () => { U.tasks = U.tasks.filter((x) => x.state === 'paused'); renderTray(); }) : null);
    tray.append(head, el('progress', { class: 'tray-total', max: 100, 'aria-label': t('upload.tray') }));
    if (!U.collapsed) {
      const rows = el('div', { class: 'tray-rows', role: 'list' });
      for (const task of U.tasks) {
        const row = el('div', { class: 'tray-row ' + task.state, role: 'listitem' }, icon(task.state === 'done' ? 'check' : task.state === 'error' ? 'alert' : 'file'),
          el('div', { class: 'tray-text' }, el('div', { class: 'tray-name', text: task.name, title: task.name }), el('div', { class: 'tray-status' }), el('progress', { max: 100 })));
        if (task.state === 'uploading' || task.state === 'queued') row.append(iconButton('pause', t('upload.pause'), () => pauseTask(task)));
        if (task.state === 'paused') row.append(iconButton('play', t('upload.resume'), () => resumeTask(task)));
        if (task.state === 'error') row.append(iconButton('retry', t('action.retry'), () => resumeTask(task)));
        if (task.state !== 'done') row.append(iconButton('close', t('upload.cancel'), () => cancelTask(task)));
        task.row = row;
        rows.append(row);
      }
      tray.append(rows);
    }
  }
  $('.tray-title', tray).textContent = title;
  $('.tray-total', tray).value = Math.round((100 * sent) / total);
  $('.tray-total', tray).hidden = !busy;
  if (U.collapsed) return;
  for (const task of U.tasks) {
    if (!task.row) continue;
    const pct = task.size ? Math.round((100 * Math.min(task.progress || task.sent, task.size)) / task.size) : 100;
    const bar = $('progress', task.row);
    bar.value = task.state === 'done' ? 100 : pct;
    bar.hidden = task.state === 'done' || task.state === 'error';
    const status = {
      queued: t('upload.state.queued'), uploading: t('upload.state.uploading', { pct, size: fmtSize(task.size) }), paused: t('upload.state.paused', { pct }),
      done: fmtSize(task.size), error: task.error || t('err.generic'),
    }[task.state];
    $('.tray-status', task.row).textContent = status || '';
  }
}

