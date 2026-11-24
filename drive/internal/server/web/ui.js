// Shared UI building blocks for the app and the public link page.
// Rule: user-controlled strings only ever reach the DOM through textContent
// or attributes set via setAttribute. There is no innerHTML in this code base.
import { t, locale } from './i18n.js';

const SVG_NS = 'http://www.w3.org/2000/svg';

export const $ = (sel, root = document) => root.querySelector(sel);

export function icon(name, cls) {
  const s = document.createElementNS(SVG_NS, 'svg');
  s.setAttribute('class', 'icon' + (cls ? ' ' + cls : ''));
  s.setAttribute('viewBox', '0 0 24 24');
  s.setAttribute('aria-hidden', 'true');
  const u = document.createElementNS(SVG_NS, 'use');
  u.setAttribute('href', '/static/icons.svg#i-' + name);
  s.append(u);
  return s;
}

// el('div', {class: 'x', onclick: fn, hidden: true}, child, 'text', ...)
export function el(tag, attrs, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') n.className = v;
    else if (k === 'text') n.textContent = v;
    else if (k.startsWith('on')) n.addEventListener(k.slice(2), v);
    else if (v === true) n.setAttribute(k, '');
    else n.setAttribute(k, String(v));
  }
  for (const c of kids.flat(Infinity)) if (c != null && c !== false) n.append(c);
  return n;
}

// Runs a DOM update inside a view transition where supported (progressive
// enhancement). Falls back to a plain update; never leaves a rejected promise.
export function transition(update) {
  const ok = document.startViewTransition && document.visibilityState === 'visible' && !matchMedia('(prefers-reduced-motion: reduce)').matches;
  if (!ok) { update(); return; }
  let ran = false;
  const once = () => { if (!ran) { ran = true; update(); } };
  try {
    const vt = document.startViewTransition(once);
    for (const p of [vt.ready, vt.finished, vt.updateCallbackDone]) p.catch(once);
    // Content must never wait on animation: if the browser has not run the
    // update shortly (throttled or background rendering), do it directly.
    setTimeout(() => { if (!ran) { once(); try { vt.skipTransition(); } catch { /* already done */ } } }, 200);
  } catch {
    once();
  }
}

// Folders first, then by name in the active language's alphabet.
export function sortByName(items) {
  const coll = new Intl.Collator(locale(), { numeric: true, sensitivity: 'base' });
  return items.sort((a, b) => (b.is_dir - a.is_dir) || coll.compare(a.name, b.name));
}

export function clear(node) {
  while (node.firstChild) node.firstChild.remove();
  return node;
}

export function iconButton(name, label, onclick, cls) {
  return el('button', { type: 'button', class: 'icon-btn' + (cls ? ' ' + cls : ''), 'aria-label': label, title: label, onclick }, icon(name));
}

// ---- API ----

export class ApiError extends Error {
  constructor(status, code, data) {
    super(code);
    this.status = status;
    this.code = code;
    this.data = data || {};
  }
}

export async function api(method, path, body) {
  const opt = { method, headers: {}, credentials: 'same-origin' };
  if (method !== 'GET') opt.headers['X-Drive-Csrf'] = '1';
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  let resp;
  try {
    resp = await fetch(path, opt);
  } catch {
    throw new ApiError(0, 'network');
  }
  let data = null;
  try {
    data = await resp.json();
  } catch { /* empty or non-JSON body */ }
  if (!resp.ok) throw new ApiError(resp.status, (data && data.error) || 'error', data);
  return data;
}

// A human sentence for an error. Never a status code.
export function errorText(e) {
  const known = ['network', 'name_conflict', 'into_self', 'cross_owner', 'disk_full', 'too_many_attempts', 'wrong_password',
    'bad_name', 'bad_email', 'self_share', 'too_deep', 'forbidden', 'not_found', 'unauthenticated', 'hash_mismatch', 'is_root',
    'reserved_name', 'too_many'];
  if (e instanceof ApiError && known.includes(e.code)) return t('err.' + e.code);
  return t('err.generic');
}

// ---- formatting ----

export function fmtSize(bytes) {
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = bytes, i = 0;
  while (v >= 1000 && i < units.length - 1) { v /= 1024; i++; }
  const digits = i === 0 || v >= 100 ? 0 : 1;
  return new Intl.NumberFormat(locale(), { maximumFractionDigits: digits }).format(v) + ' ' + units[i];
}

export function fmtDate(sec) {
  const d = new Date(sec * 1000), now = new Date();
  const day = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const diff = Math.round((day(d) - day(now)) / 86400000);
  if (diff === 0) return new Intl.DateTimeFormat(locale(), { hour: '2-digit', minute: '2-digit' }).format(d);
  if (diff === -1) {
    const s = new Intl.RelativeTimeFormat(locale(), { numeric: 'auto' }).format(-1, 'day');
    return s.charAt(0).toLocaleUpperCase(locale()) + s.slice(1);
  }
  const opts = d.getFullYear() === now.getFullYear() ? { day: 'numeric', month: 'short' } : { day: 'numeric', month: 'short', year: 'numeric' };
  return new Intl.DateTimeFormat(locale(), opts).format(d);
}

export function kindIcon(node) {
  if (node.is_dir) return 'folder';
  return { image: 'image', 'image-file': 'image', video: 'video', 'video-file': 'video', audio: 'audio', pdf: 'pdf', text: 'text', doc: 'text', archive: 'archive' }[node.kind] || 'file';
}

// ---- toast (one at a time, 5 s, optional action) ----

let toastTimer = 0;
export function toast(message, opts = {}) {
  let host = $('#toast');
  if (!host) {
    host = el('div', { id: 'toast', class: 'toast', role: 'status', 'aria-live': 'polite' });
    document.body.append(host);
  }
  // Inside an open modal dialog the toast must live in the top layer too.
  const open = [...document.querySelectorAll('dialog[open]')].pop();
  (open || document.body).append(host);
  clear(host);
  host.classList.toggle('error', !!opts.error);
  host.append(el('span', { class: 'toast-text', text: message }));
  if (opts.action) {
    host.append(el('button', { type: 'button', class: 'btn text', text: opts.action, onclick: () => { hideToast(); opts.onAction(); } }));
  }
  host.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(hideToast, opts.action ? 7000 : 5000);
}
function hideToast() {
  const host = $('#toast');
  if (host) host.classList.remove('show');
}
export function toastError(e) { toast(errorText(e), { error: true }); }

// ---- dialogs ----

// openDialog({title, body, actions: [{label, kind, onclick(close, button)}], cls}) -> {dialog, close}
export function openDialog({ title, body, actions = [], cls = '', onclose }) {
  const dlg = el('dialog', { class: 'dialog ' + cls, 'aria-label': title });
  const close = () => dlg.close();
  const head = el('header', { class: 'dialog-head' },
    el('h2', { text: title }),
    iconButton('close', t('action.close'), close));
  const foot = el('footer', { class: 'dialog-foot' });
  for (const a of actions) {
    const b = el('button', { type: a.submit ? 'submit' : 'button', class: 'btn ' + (a.kind || 'ghost'), text: a.label });
    if (a.onclick) b.addEventListener('click', () => a.onclick(close, b));
    foot.append(b);
  }
  const content = el('div', { class: 'dialog-body' }, body);
  dlg.append(head, content, actions.length ? foot : null);
  dlg.addEventListener('click', (ev) => { if (ev.target === dlg) close(); });
  dlg.addEventListener('close', () => { dlg.remove(); if (onclose) onclose(); });
  document.body.append(dlg);
  dlg.showModal();
  return { dialog: dlg, close, content, foot };
}

// Only for irreversible actions (delete forever, empty trash).
export function confirmDanger({ title, message, confirmLabel }) {
  return new Promise((resolve) => {
    let answer = false;
    openDialog({
      title,
      cls: 'small',
      body: el('p', { text: message }),
      actions: [
        { label: t('action.cancel'), kind: 'ghost', onclick: (close) => close() },
        { label: confirmLabel, kind: 'danger', onclick: (close) => { answer = true; close(); } },
      ],
      onclose: () => resolve(answer),
    });
  });
}

// A one-field form. submit(value) may throw; the error is shown next to the field.
export function promptDialog({ title, label, value = '', confirmLabel, type = 'text', selectBase = false, submit }) {
  const input = el('input', { type, class: 'input', value, required: true, autocomplete: 'off', id: 'prompt-input' });
  const error = el('p', { class: 'field-error', role: 'alert', hidden: true });
  const ok = el('button', { type: 'submit', class: 'btn primary', text: confirmLabel });
  const form = el('form', { class: 'form', novalidate: true },
    el('label', { for: 'prompt-input', text: label }), input, error);
  const d = openDialog({ title, cls: 'small', body: form });
  const cancel = el('button', { type: 'button', class: 'btn ghost', text: t('action.cancel'), onclick: d.close });
  form.append(el('div', { class: 'dialog-foot inline' }, cancel, ok));
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const v = input.value.trim();
    if (!v) { input.focus(); return; }
    ok.disabled = true;
    try {
      await submit(v);
      d.close();
    } catch (e) {
      error.textContent = errorText(e);
      error.hidden = false;
      input.setAttribute('aria-invalid', 'true');
      input.focus();
    } finally {
      ok.disabled = false;
    }
  });
  input.focus();
  if (selectBase && value.lastIndexOf('.') > 0) input.setSelectionRange(0, value.lastIndexOf('.'));
  else input.select();
  return d;
}

// ---- menu: bottom sheet on phones, positioned menu on desktop ----

// items: [{icon, label, onclick, danger}] (falsy entries are skipped)
export function openMenu(items, point, title) {
  const dlg = el('dialog', { class: 'menu', 'aria-label': title || t('action.more') });
  if (title) dlg.append(el('div', { class: 'menu-title', text: title }));
  const list = el('div', { class: 'menu-list', role: 'menu' });
  for (const it of items) {
    if (!it) continue;
    if (it === 'sep') { list.append(el('hr')); continue; }
    list.append(el('button', {
      type: 'button', role: 'menuitem', class: 'menu-item' + (it.danger ? ' danger' : '') + (it.checked ? ' checked' : ''),
      onclick: () => { dlg.close(); it.onclick(); },
    }, icon(it.icon), el('span', { text: it.label }), it.checked ? icon('check', 'end') : null));
  }
  dlg.append(list);
  dlg.addEventListener('click', (ev) => { if (ev.target === dlg) dlg.close(); });
  dlg.addEventListener('close', () => dlg.remove());
  dlg.addEventListener('keydown', (ev) => {
    if (ev.key !== 'ArrowDown' && ev.key !== 'ArrowUp') return;
    ev.preventDefault();
    const btns = [...list.querySelectorAll('button')];
    const i = btns.indexOf(document.activeElement);
    btns[(i + (ev.key === 'ArrowDown' ? 1 : -1) + btns.length) % btns.length].focus();
  });
  document.body.append(dlg);
  dlg.showModal();
  if (point && window.matchMedia('(min-width: 720px)').matches) {
    const r = dlg.getBoundingClientRect();
    dlg.style.left = Math.max(8, Math.min(point.x, window.innerWidth - r.width - 8)) + 'px';
    dlg.style.top = Math.max(8, Math.min(point.y, window.innerHeight - r.height - 8)) + 'px';
    dlg.classList.add('placed');
  }
  return dlg;
}

export function pointOf(ev, anchor) {
  if (anchor) {
    const r = anchor.getBoundingClientRect();
    return { x: r.left, y: r.bottom + 4 };
  }
  return { x: ev.clientX, y: ev.clientY };
}

// ---- file items (grid card / list row) ----

// opts: {view, thumbUrl(node), meta(node), selected, selectable, onopen(node, ev), onmenu(node, ev, anchor), oncheck(node, ev)}
export function renderItem(node, opts) {
  const thumb = el('div', { class: 'thumb' });
  if (node.thumb && opts.thumbUrl) {
    const img = el('img', { alt: '', loading: 'lazy', decoding: 'async', src: opts.thumbUrl(node) });
    img.addEventListener('error', () => { img.remove(); thumb.append(icon(kindIcon(node), 'kind')); });
    thumb.append(img);
  } else {
    thumb.append(icon(kindIcon(node), 'kind ' + (node.is_dir ? 'folder' : '')));
  }
  const meta = opts.meta ? opts.meta(node) : (node.is_dir ? t('item.folder') : fmtSize(node.size));
  const item = el('div', {
    class: 'item' + (opts.selected ? ' selected' : ''), role: 'listitem', tabindex: 0, 'data-id': node.id,
    'aria-label': node.name, 'aria-selected': opts.selectable ? String(!!opts.selected) : null,
  });
  if (opts.selectable) {
    item.append(el('button', {
      type: 'button', class: 'check', 'aria-label': t('action.select'), 'aria-pressed': String(!!opts.selected),
      onclick: (ev) => { ev.stopPropagation(); opts.oncheck(node, ev); },
    }, icon('check')));
  }
  item.append(thumb,
    el('div', { class: 'item-text' },
      el('div', { class: 'item-name', text: node.name, title: node.name }),
      el('div', { class: 'item-meta' },
        node.shared ? el('span', { class: 'badge', title: t('item.shared') }, icon('people'), el('span', { class: 'sr', text: t('item.shared') })) : null,
        el('span', { text: meta }))),
    el('div', { class: 'item-date', text: fmtDate(node.trashed_at || node.updated_at) }),
    el('div', { class: 'item-size', text: node.is_dir ? '' : fmtSize(node.size) }));
  if (opts.onmenu) {
    const more = iconButton('more', t('action.more'), (ev) => { ev.stopPropagation(); opts.onmenu(node, ev, more); }, 'more');
    item.append(more);
    item.addEventListener('contextmenu', (ev) => { ev.preventDefault(); opts.onmenu(node, ev, null); });
  }
  item.addEventListener('click', (ev) => opts.onopen(node, ev));
  item.addEventListener('keydown', (ev) => {
    if (ev.target !== item) return;
    if (ev.key === 'Enter') { ev.preventDefault(); opts.onopen(node, ev); }
    if (ev.key === ' ' && opts.selectable) { ev.preventDefault(); opts.oncheck(node, ev); }
  });
  if (opts.onlongpress) {
    let timer = 0, fired = false;
    item.addEventListener('touchstart', () => { fired = false; timer = setTimeout(() => { fired = true; opts.onlongpress(node); }, 500); }, { passive: true });
    const cancel = () => clearTimeout(timer);
    item.addEventListener('touchmove', cancel, { passive: true });
    item.addEventListener('touchend', (ev) => { cancel(); if (fired) ev.preventDefault(); });
    item.addEventListener('touchcancel', cancel);
  }
  return item;
}

export function skeleton(view, n = 8) {
  const box = el('div', { class: 'items ' + view + ' skeleton', 'aria-hidden': 'true' });
  for (let i = 0; i < n; i++) box.append(el('div', { class: 'item' }, el('div', { class: 'thumb' }), el('div', { class: 'item-text' }, el('div', { class: 'bar' }), el('div', { class: 'bar short' }))));
  return box;
}

// stateBox({icon, title, text, action: {label, onclick}})
export function stateBox({ icon: ic, title, text, action }) {
  return el('div', { class: 'state' },
    icon(ic, 'big'),
    el('h2', { text: title }),
    text ? el('p', { text }) : null,
    action ? el('button', { type: 'button', class: 'btn primary', text: action.label, onclick: action.onclick }) : null);
}

// ---- lightbox ----

// openLightbox({items, index, urlFor(node, download), actions(node, close) -> [{icon,label,onclick}]})
export function openLightbox({ items, index, urlFor, actions }) {
  let i = index;
  const dlg = el('dialog', { class: 'lightbox', 'aria-label': t('preview.title') });
  const name = el('div', { class: 'lb-name' });
  const tools = el('div', { class: 'lb-tools' });
  const stage = el('div', { class: 'lb-stage' });
  const prevBtn = iconButton('chevron-left', t('preview.prev'), () => go(-1), 'lb-nav prev');
  const nextBtn = iconButton('chevron-right', t('preview.next'), () => go(1), 'lb-nav next');
  let closed = false;
  const close = () => { if (!closed) dlg.close(); };
  dlg.append(el('header', { class: 'lb-bar' }, iconButton('close', t('action.close'), close), name, tools), stage, prevBtn, nextBtn);

  function show() {
    const node = items[i];
    name.textContent = node.name;
    clear(tools);
    for (const a of actions(node, close)) tools.append(iconButton(a.icon, a.label, a.onclick));
    prevBtn.hidden = i <= 0;
    nextBtn.hidden = i >= items.length - 1;
    clear(stage);
    stage.classList.remove('zoomed');
    const src = urlFor(node, false);
    if (node.kind === 'image') {
      const img = el('img', { src, alt: node.name });
      img.addEventListener('dblclick', () => stage.classList.toggle('zoomed'));
      img.addEventListener('error', () => { clear(stage); stage.append(noPreview(node)); });
      stage.append(img);
    } else if (node.kind === 'video') {
      stage.append(el('video', { src, controls: true, autoplay: true, playsinline: true }));
    } else if (node.kind === 'audio') {
      stage.append(el('div', { class: 'lb-audio' }, icon('audio', 'big'), el('audio', { src, controls: true, autoplay: true })));
    } else if (node.kind === 'pdf') {
      stage.append(el('iframe', { src, title: node.name }));
    } else if (node.kind === 'text') {
      const pre = el('pre', { class: 'lb-text', tabindex: 0, text: t('state.loading') });
      stage.append(pre);
      fetch(src, { headers: { Range: 'bytes=0-524287' }, credentials: 'same-origin' })
        .then((r) => (r.ok ? r.text() : Promise.reject(new Error('failed'))))
        .then((text) => { pre.textContent = text + (node.size > 524288 ? '\n\n' + t('preview.truncated') : ''); })
        .catch(() => { clear(stage); stage.append(noPreview(node)); });
    } else {
      stage.append(noPreview(node));
    }
  }
  function noPreview(node) {
    return el('div', { class: 'state on-dark' },
      icon(kindIcon(node), 'big'),
      el('h2', { text: t('preview.none') }),
      el('a', { class: 'btn primary', href: urlFor(node, true), text: t('action.download') + ' · ' + fmtSize(node.size) }));
  }
  function go(d) {
    const n = i + d;
    if (n < 0 || n >= items.length) return;
    i = n;
    transition(show);
  }
  dlg.addEventListener('keydown', (ev) => {
    if (ev.target.closest('pre, video, audio, input')) return;
    if (ev.key === 'ArrowLeft') go(-1);
    if (ev.key === 'ArrowRight') go(1);
  });
  let startX = null;
  stage.addEventListener('touchstart', (ev) => { startX = ev.touches.length === 1 ? ev.touches[0].clientX : null; }, { passive: true });
  stage.addEventListener('touchend', (ev) => {
    if (startX == null || stage.classList.contains('zoomed') || (window.visualViewport && window.visualViewport.scale > 1.05)) return;
    const dx = ev.changedTouches[0].clientX - startX;
    if (Math.abs(dx) > 60) go(dx < 0 ? 1 : -1);
    startX = null;
  });
  stage.addEventListener('click', (ev) => { if (ev.target === stage) close(); });

  // The browser/phone Back button closes the preview instead of leaving.
  history.pushState({ lightbox: true }, '');
  const onPop = () => close();
  window.addEventListener('popstate', onPop);
  dlg.addEventListener('close', () => {
    closed = true;
    window.removeEventListener('popstate', onPop);
    clear(stage); // stops any playing media
    dlg.remove();
    if (history.state && history.state.lightbox) history.back();
  });
  document.body.append(dlg);
  dlg.showModal();
  show();
  return { close, current: () => items[i] };
}

