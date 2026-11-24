# UI/UX standard

Applies to every screen in `drive/` and `curator/`. The users are one family:
parents, grandparents, children, mostly on phones, mostly not technical, mostly
Vietnamese speakers. Design for the least technical person in the family.

Direction: content-first, calm, like current Google Drive/Photos (Material 3) and
Apple Photos — the user's files and photos are the interface; chrome stays quiet.
Built with modern plain CSS and JS (no framework, no build step, no CDN).

## Principles (in priority order)

1. **The user's goal first.** Every screen answers "what can I do here?" with one
   obvious primary action (Upload, Share, Keep/Remove). Secondary actions live in
   a menu.
2. **Phone first.** Design at 360 px wide, then scale up. Everything reachable
   with one thumb. Touch targets ≥ 44×44 px.
3. **Plain words.** No jargon: "Thùng rác", not "Soft-deleted items". No IDs,
   hashes, byte counts or error codes in the main UI. Sizes and dates are
   human ("3,2 MB", "Hôm qua").
4. **Nothing scary, nothing lost.** Reversible actions (move to trash, move,
   rename) happen immediately and show a toast with **Hoàn tác / Undo**.
   Confirmation dialogs are only for irreversible actions (delete forever, empty
   trash), and they name the consequence and the count.
5. **Always show what is happening.** Skeletons while loading, optimistic updates,
   a persistent upload tray with per-file progress, clear success and failure
   states with a "Thử lại / Retry" button. Never a blank screen or a spinner
   without text for more than a second.
6. **Helpful empty states.** An empty folder shows an illustration-free, friendly
   message and the primary action ("Kéo tệp vào đây hoặc bấm Tải lên").
7. **Accessible by default.** WCAG AA contrast, visible focus ring, full keyboard
   use, `prefers-reduced-motion` respected, text scales with the browser font
   size (use `rem`), all icons have labels, form errors are next to the field.

## Language

- Vietnamese and English. Pick from `navigator.language` (`vi*` → Vietnamese, else
  English); the user can switch in the account menu; the choice is remembered.
- All strings live in one dictionary per language (`i18n.js` / a Python dict for
  server-rendered pages) keyed by id. No string literals in markup or logic.
- Dates and numbers through `Intl.DateTimeFormat` / `Intl.NumberFormat` /
  `Intl.RelativeTimeFormat` with the active locale.

## Layout

- **Phone (< 720 px):** top app bar (title or search, account avatar), content,
  bottom navigation (max 4 items with icon + label), a floating primary action
  button above the nav. Actions on an item open a **bottom sheet**.
- **Desktop (≥ 720 px):** left navigation rail/sidebar with the primary button at
  the top, search centered in the top bar, content area max-width fluid. Actions
  on an item open a **context menu** (right-click and a "⋮" button).
- Drive navigation: Tệp của tôi / Được chia sẻ / Thùng rác (+ Dọn ảnh for admins).
- Content respects safe areas (`env(safe-area-inset-*)`) and uses `100dvh`.

## Visual language

- **Type:** system font stack (renders Vietnamese diacritics correctly, zero
  download): `system-ui, -apple-system, "Segoe UI", Roboto, "Noto Sans", sans-serif`.
  Scale: 0.8125 / 0.875 / 1 / 1.125 / 1.375 / 1.75 rem. Body 1rem, line-height 1.5.
  Weights 400 and 600 only.
- **Colour:** neutral surfaces plus ONE accent. Tokens in OKLCH, light and dark
  from `prefers-color-scheme`, with a manual override via `data-theme` on `<html>`.
  Red is reserved for destructive/irreversible actions and errors.
- **Shape:** radius 8 (controls), 12 (cards, thumbnails), 20 (sheets, dialogs),
  full (chips, FAB, avatars). Borders 1 px hairline; elevation only for things
  that float (menus, sheets, dialogs, FAB, toasts).
- **Spacing:** 4 px grid — 4, 8, 12, 16, 24, 32, 48.
- **Motion:** 120–240 ms, ease-out for entering, ease-in for leaving. Use the View
  Transitions API for folder navigation and opening previews where supported
  (progressive enhancement). No decorative animation.
- **Icons:** one inline SVG sprite, 24 px, 1.75 px stroke, rounded caps. No emoji
  as icons, no icon fonts.

### Tokens (keep this block identical in `drive` and `curator` as `tokens.css`)

```css
:root {
  color-scheme: light dark;
  --font: system-ui, -apple-system, "Segoe UI", Roboto, "Noto Sans", sans-serif;
  --bg: oklch(0.985 0.003 250);
  --surface: oklch(1 0 0);
  --surface-2: oklch(0.965 0.005 250);
  --border: oklch(0.9 0.006 250);
  --text: oklch(0.24 0.012 250);
  --text-muted: oklch(0.5 0.012 250);
  --accent: oklch(0.56 0.17 255);
  --accent-hover: oklch(0.5 0.18 255);
  --accent-soft: oklch(0.94 0.035 255);
  --on-accent: oklch(1 0 0);
  --danger: oklch(0.56 0.2 27);
  --danger-soft: oklch(0.95 0.03 27);
  --success: oklch(0.6 0.14 150);
  --warning: oklch(0.75 0.14 80);
  --focus: oklch(0.62 0.19 255);
  --r-s: 8px; --r-m: 12px; --r-l: 20px; --r-full: 999px;
  --shadow-1: 0 1px 2px oklch(0 0 0 / 0.06), 0 2px 8px oklch(0 0 0 / 0.06);
  --shadow-2: 0 4px 12px oklch(0 0 0 / 0.1), 0 12px 32px oklch(0 0 0 / 0.12);
  --t-fast: 120ms; --t-med: 200ms;
}
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
    --bg: oklch(0.17 0.008 250);
    --surface: oklch(0.21 0.009 250);
    --surface-2: oklch(0.26 0.01 250);
    --border: oklch(0.33 0.012 250);
    --text: oklch(0.94 0.005 250);
    --text-muted: oklch(0.72 0.01 250);
    --accent: oklch(0.74 0.13 255);
    --accent-hover: oklch(0.8 0.11 255);
    --accent-soft: oklch(0.32 0.06 255);
    --on-accent: oklch(0.18 0.02 255);
    --danger: oklch(0.72 0.16 27);
    --danger-soft: oklch(0.3 0.06 27);
    --focus: oklch(0.78 0.13 255);
    --shadow-1: 0 1px 2px oklch(0 0 0 / 0.4);
    --shadow-2: 0 8px 28px oklch(0 0 0 / 0.55);
  }
}
:root[data-theme="dark"] { /* same values as the dark block above */ }
```

## Components

Use native elements first: `<dialog>` for dialogs and sheets, `popover` for menus,
`<button>`, `<input>`, `<select>`. One implementation of each, reused everywhere.

- **File grid / list** with a toggle (remembered). Grid cards: large thumbnail or
  type icon, name (2 lines max, ellipsis), small meta line. List rows: icon, name,
  modified, size, shared badge. Folders first.
- **Selection mode:** long-press on touch, checkbox on hover / Ctrl-click / Shift
  range on desktop → a bulk action bar replaces the top bar (count, Download,
  Move, Share, Delete).
- **Upload:** button, FAB, drag-and-drop anywhere (full-window drop overlay), and
  on phones the native picker incl. camera roll. Upload tray docked bottom-right
  (desktop) / above the nav (phone): per-file progress, pause/retry, collapses to
  a single line, never blocks browsing.
- **Preview / lightbox:** full-screen, swipe and arrow keys between siblings,
  pinch/zoom for images, Esc/back to close, actions (Download, Share, Delete) in
  the top bar.
- **Share dialog** (the feature people use most with others — make it effortless):
  email input that turns addresses into chips, role picker in plain words ("Chỉ
  xem" / "Được chỉnh sửa"), list of people with access and one-tap remove, a
  "Sao chép liên kết" section with expiry and optional password, and a confirmation
  toast. State who can see the item right now in one sentence.
- **Toast / snackbar:** bottom, one at a time, 5 s, with an optional Undo action,
  announced via `aria-live`.
- **Search:** instant results as you type (debounced), recent items when empty.
- **Sign-in page:** product name, one sentence, one "Đăng nhập bằng Google" button
  following Google's branding rules. A friendly page for accounts that are not
  allowed ("Hãy nhờ <chủ nhà> chia sẻ với email này").
- **Errors:** human sentence + what to do next. Never a raw status code or stack.

### Curator review (photo clean-up)

The task is emotional — these are family memories. The UI must feel safe.

- One decision at a time: a large photo, the reason in plain words ("Ảnh bị mờ",
  "Ảnh chụp màn hình", "5 ảnh gần giống nhau"), and two big buttons: **Giữ lại**
  and **Bỏ vào thùng rác**. Keyboard: K / X, arrows to move. Swipe on phones.
- Burst groups: frames side by side, the suggested keeper marked "Đẹp nhất", tap
  to change, then "Giữ ảnh đã chọn, bỏ phần còn lại" or "Gom thành một chồng"
  (stacks, removes nothing).
- A grid mode for fast bulk review of a category (screenshots), with select-all.
- Every removal shows Undo and the standing reassurance that items stay in the
  Immich trash for 30 days. Progress is visible ("Còn 42 ảnh").
- Never use red on the Keep path; never preselect a destructive option.

## Installable app

Ship a web app manifest, icons (SVG + 192/512 PNG, maskable), `theme-color` for
both schemes, and a minimal service worker that caches the app shell only (never
user files), so the family can "Add to Home Screen" and open Drive like an app.

## Definition of done for any UI change

- Works at 360 px, 768 px and 1440 px; light and dark; Vietnamese and English.
- Usable with keyboard only; focus is always visible; no console errors.
- Every user-controlled string is escaped (`textContent`, never `innerHTML`).
- Loading, empty, error and success states all exist for the screen.
- Checked in a real browser, not only by tests.
