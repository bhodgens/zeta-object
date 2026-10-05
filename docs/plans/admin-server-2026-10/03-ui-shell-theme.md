# Admin Server: UI Shell and Theme - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Do NOT commit - the orchestrator handles all git operations after
> review. Explore with terminal cat; do not use read_file on existing
> source files. After completing, report what you built, what files you
> touched, and any deviations from the spec.

## Meta

- **Parent:** ./master.md
- **Scope:** the embedded asset set: HTML shell, the design tokens derived
  from the logo palette, the left tab rail, the login page, and the logo
  copies used by the rail and the favicon.
- **Dependencies:** none
- **Estimated Context:** 50K
- **Concurrency Group:** A

## Goal

A clean, smooth, single-page shell: a persistent left rail of tabs, a
content region that swaps without layout shift, a sign-in page, and a
theme whose colours come from the logo. The screen logic is leaf 04; this
leaf owns the structure, the styles, and the empty/loading/error states.

## Context

- `logo-icon.png` (560x560, square) and `logo.jpeg` (1408x768) are at the
  repo root and referenced by README. The palette extracted from them:
  bulk `#000018` / `#001818` / `#001830`, mid-tones `#003048` / `#186078`,
  glow accent `#2581b6` / `#1878a8`, highlights near `#f0f0f0`.
- Assets are embedded by leaf 01 from `internal/adminserver/web/`; this
  leaf writes the files in that directory.
- Contract 2 in the master pins the console's HTTP surface, including
  `/assets/{file}` and the login page.
- Repo frontend conventions: no framework, no bundler, no external
  requests. Everything must work offline from the embedded bytes.

## Interface Contracts (From Parent)

### Contract 4: theme tokens (binding)

Implement the exact tokens from the master's Contract 4 table as CSS
custom properties on `:root`, with a light variant under
`[data-theme="light"]`, and honour `prefers-reduced-motion`.

### Shell contract consumed by leaf 04 (produce exactly these hooks)

- `<main id="view">` is the single content region; screens render into
  `#view`.
- The left rail is `<nav id="rail">` containing one `<button class="tab"
  data-screen="...">` per screen: `dashboard`, `config`, `buckets`,
  `danger`.
- `<div id="toast">` is the transient message region (success and error).
- `<button id="theme-toggle">` switches `data-theme`.
- `<button id="signout">` calls the sign-out path.
- `<div id="modal">` hosts the confirmation dialog for the destructive
  action; it is initially hidden.
- `<html data-screen="dashboard">` reflects the current screen so CSS can
  react; leaf 04 owns the switching logic and calls the documented
  `window.Console.render(screen)` entry point.

## Tasks

### Task 1: HTML shell

**Files:** Create `internal/adminserver/web/index.html`
(and replace leaf 01's placeholder), `internal/adminserver/web/login.html`

**Requirements:** semantic structure (header, nav, main); a skip link; the
rail tabs in the required order with the required `data-screen` values;
the logo mark in the rail (from the embedded icon copy) with the product
name; a footer line carrying the console version; the login page with a
single token field, a submit button, and a message region. No inline
event handlers; no external URLs.

**Steps:** write the two files; assert with `grep` that neither contains
`http://` or `https://` (except the inert `xmlns` on any SVG, which is
allowed and must be documented if used).

### Task 2: styles and theme

**Files:** Create `internal/adminserver/web/app.css`

**Requirements:** the Contract 4 tokens plus the light variant; a
two-column layout (rail about 232px, content fluid); a focus ring on every
interactive element; transitions of about 140ms on colour and transform;
`prefers-reduced-motion` disables them; the rail marks the active tab
clearly; a card/panel style for content blocks; table styling for buckets
and configuration; a badge style for states (ok/warn/danger) using the
semantic tokens; the modal and toast styles; and a responsive rule that
collapses the rail to icons under a narrow width. No CSS framework, no
web font (system font stack only).

### Task 3: shell behaviour JavaScript

**Files:** Create `internal/adminserver/web/app.js`

**Requirements:** define `window.Console` with: `render(screen)` (screen
switching plus `data-screen` and the active-tab state), `toast(kind,
message)`, `modal(title, body)` returning a promise resolved by the
confirm/cancel buttons, `api(method, path, body)` (fetch with the CSRF
header, JSON handling, and the shared 401 behaviour that returns the user
to the sign-in page), `theme` get/set with `localStorage`, and
`escapeHtml`. Register the theme toggle and sign-out handlers. Every
screen module from leaf 04 attaches itself to `window.Console.screens`.

**Steps:** write the file; keep it dependency-free; ensure no stray
`console.log` remains.

### Task 4: logo copies and favicon

**Files:** Create `internal/adminserver/web/logo-icon.png` (copy of the
repo-root icon), Create `internal/adminserver/web/favicon.ico` or use the
PNG as the icon.

**Steps:** copy the file (do not modify or move the originals); reference
it from the shell and the rail. Confirm the embedded path loads under
`/assets/logo-icon.png`.

## Self-Verification Checklist

- [ ] The shell, login page, CSS, and JS exist under `internal/adminserver/web/`
- [ ] Every hook leaf 04 needs is present exactly as specified
- [ ] All Contract 4 tokens present, including the light variant
- [ ] Zero external requests in the assets (grep for `http://`, `https://`)
- [ ] `prefers-reduced-motion` handled; focus rings on all controls
- [ ] The repo-root logo files are unmodified (`git status` must not list them)
- [ ] DO-NOT-TOUCH: every `.go` file, `Makefile`, `go.sum`

**DO NOT COMMIT.**

**Deviations from spec:** [none / list with rationale]

## Review Checklist (For Review Agent)

- [ ] The DOM hooks match the master's shell contract exactly (ids and the
      four `data-screen` values)
- [ ] The palette matches the extracted logo values, not invented ones
- [ ] Clean and smooth: no layout shift on tab change, visible focus
      states, no dependency on a network font
- [ ] Accessibility: the rail is keyboard navigable and the skip link works

Output: APPROVED or specific gaps with file:line.

## Notes

- Leaf 04 writes the screen modules; this leaf must not implement screen
  logic beyond the shell and the empty state.
- Keep the icon copy inside the embed directory so the console has no
  runtime file dependency.
