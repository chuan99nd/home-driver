// Public share-link page: browse and download, nothing else. No session.
import { t, getLang, setLang, applyI18n } from './i18n.js';
import { $, el, clear, icon, api, ApiError, errorText, fmtSize, fmtDate, kindIcon, renderItem, skeleton, stateBox, openLightbox, sortByName } from './ui.js';

const token = location.pathname.split('/')[2] || '';
const base = '/api/public/' + encodeURIComponent(token);
let root = null;
let view = 'list';

applyI18n(document);
const lang = $('#lang');
const paintLang = () => { lang.textContent = getLang() === 'vi' ? 'English' : 'Tiếng Việt'; };
paintLang();
lang.addEventListener('click', () => { setLang(getLang() === 'vi' ? 'en' : 'vi'); applyI18n(document); paintLang(); start(); });
window.addEventListener('hashchange', () => { if (root) show(currentId()); });
start();

function currentId() {
  return location.hash.replace(/^#\/?/, '') || root.id;
}

async function start() {
  const box = clear($('#content'));
  clear($('#crumbs'));
  clear($('#tools'));
  box.append(skeleton('list', 4), el('p', { class: 'sr', role: 'status', text: t('state.loading') }));
  let info;
  try {
    info = await api('GET', base);
  } catch (e) {
    return fail(e);
  }
  if (info.needs_password) return askPassword();
  root = info.node;
  show(currentId());
}

function fail(e) {
  const gone = e instanceof ApiError && e.status === 404;
  clear($('#crumbs'));
  clear($('#tools'));
  clear($('#content')).append(stateBox({
    icon: gone ? 'link' : 'alert',
    title: gone ? t('public.gone.title') : errorText(e),
    text: gone ? t('public.gone.text') : t('state.error.text'),
    action: gone ? null : { label: t('action.retry'), onclick: start },
  }));
}

function askPassword() {
  const input = el('input', { type: 'password', class: 'input', id: 'pw', required: true, autocomplete: 'off' });
  const error = el('p', { class: 'field-error', role: 'alert', hidden: true });
  const btn = el('button', { type: 'submit', class: 'btn primary', text: t('public.password.open') });
  const form = el('form', { class: 'form panel narrow', novalidate: true },
    icon('lock', 'big'), el('h1', { text: t('public.password.title') }),
    el('label', { for: 'pw', text: t('public.password.label') }), input, error, btn);
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (!input.value) { input.focus(); return; }
    btn.disabled = true;
    try {
      await api('POST', base + '/unlock', { password: input.value });
      start();
    } catch (e) {
      error.textContent = errorText(e);
      error.hidden = false;
      input.setAttribute('aria-invalid', 'true');
      input.select();
    }
    btn.disabled = false;
  });
  clear($('#content')).append(form);
  input.focus();
}

const urlFor = (n, dl) => `${base}/nodes/${n.id}/content${dl ? '?download=1' : ''}`;

async function show(id) {
  let d;
  try {
    d = await api('GET', `${base}/nodes/${id}`);
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) return askPassword();
    return fail(e);
  }
  document.title = d.node.name + ' · ' + t('app.name');
  const crumbs = clear($('#crumbs'));
  d.breadcrumbs.forEach((c, i) => {
    const last = i === d.breadcrumbs.length - 1;
    if (i > 0) crumbs.append(icon('chevron-right', 'sep'));
    crumbs.append(last ? el('h1', { class: 'crumb current', text: c.name }) : el('a', { class: 'crumb', href: '#/' + c.id, text: c.name }));
  });
  const tools = clear($('#tools'));
  const box = clear($('#content'));

  if (!d.node.is_dir) {
    const n = d.node;
    const canPreview = ['image', 'video', 'audio', 'pdf', 'text'].includes(n.kind);
    box.append(el('div', { class: 'panel narrow file-card' },
      icon(kindIcon(n), 'big'),
      el('h2', { class: 'wrap-any', text: n.name }),
      el('p', { class: 'muted', text: fmtSize(n.size) + ' · ' + fmtDate(n.updated_at) }),
      el('a', { class: 'btn primary', href: urlFor(n, true) }, icon('download'), el('span', { text: t('action.download') })),
      canPreview ? el('button', { type: 'button', class: 'btn tonal', onclick: () => preview([n], 0) }, icon('open'), el('span', { text: t('action.open') })) : null));
    return;
  }

  tools.append(el('a', { class: 'btn tonal', href: `${base}/nodes/${d.node.id}/zip` }, icon('download'), el('span', { text: t('public.download.all') })));
  const next = view === 'grid' ? 'list' : 'grid';
  tools.append(el('button', { type: 'button', class: 'icon-btn', 'aria-label': t(next === 'grid' ? 'view.grid' : 'view.list'), onclick: () => { view = next; show(id); } }, icon(next)));
  if (!d.children.length) {
    box.append(stateBox({ icon: 'folder', title: t('empty.folder.title') }));
    return;
  }
  sortByName(d.children);
  const list = el('div', { class: 'items ' + view, role: 'list' });
  const files = d.children.filter((c) => !c.is_dir);
  for (const node of d.children) {
    list.append(renderItem(node, {
      view,
      thumbUrl: (n) => `${base}/nodes/${n.id}/thumb`,
      // The desktop list has its own size column.
      meta: (n) => (view === 'list' && matchMedia('(min-width: 720px)').matches ? '' : n.is_dir ? t('item.folder') : fmtSize(n.size)),
      onopen: (n) => { if (n.is_dir) location.hash = '#/' + n.id; else preview(files, files.indexOf(n)); },
    }));
  }
  box.append(list);
}

function preview(items, index) {
  openLightbox({
    items, index, urlFor,
    actions: (n) => [{ icon: 'download', label: t('action.download'), onclick: () => { location.href = urlFor(n, true); } }],
  });
}
