# Web UI GitHub-parity — verification

The bleephub web SPA (`web/src`, served at `/ui/`) has verified github.com parity
across four dimensions. Each dimension below states what it means and the exact
command that proves it green. This closes out the multi-round parity campaign
(ledger prefix `WEB-` in [`../BUGS.md`](../BUGS.md)).

## The four dimensions

1. **Nav / structural parity** — every github.com surface (repo tabs + Settings
   sections, org pages + settings, user profile tabs, user Settings, enterprise,
   global create/user menus) has a place in the UI, and every write control the
   server backs is reachable. No dead controls: every listed affordance works.
2. **Light / dark with zero hardcoded-color leaks** — all color comes from
   theme-aware CSS custom properties; no `var(--x)` resolves undefined; no non-brand
   hex leaks across themes.
3. **WCAG 2.2 AA** — 0 axe violations on every route in both themes, plus the
   behaviours axe cannot see: focus trap / focus-in / focus-restore / Escape on
   overlays, roving tabindex, no keyboard traps, `prefers-reduced-motion` honored,
   and screen-reader announcements for async outcomes (errors, loading, result counts).
4. **ARIA** — controls are labeled; dialog/combobox/listbox/menu/tablist roles are
   correct; live regions convey dynamic state.

## Reference-quality primitives

Reuse these instead of re-rolling — each implements the full a11y contract:

- `web/src/components/ui.tsx` `Modal` — focus trap (`trapTab`), focus-in on open,
  focus-restore on close, Escape (topmost-guard), `role=dialog`/`aria-modal`/labelled.
- `CommandPalette`, `GoToFile`, `KeyboardShortcuts` — combobox + listbox with
  `aria-activedescendant`; focus-in/restore; Escape.
- `AppHeader` `HeaderMenu` — roving focus, Escape-restores-trigger.
- `web/src/hooks/useDismiss.ts` — outside-click + Escape dismissal for any popup.
- `ui.tsx` Tabs — roving-tabindex `role=tablist`.
- `ErrorBanner` (`role=alert`), core `Toast` (`aria-live`), core `Spinner`
  (`role=status aria-live=polite aria-busy`) — the announcement surfaces.
- Global `prefers-reduced-motion` guard in `web/core/src/styles/tokens.css`.

## How to verify (all currently green)

```sh
# Types + unit tests
cd web && bun run typecheck && bun run test

# Bundle under the 160 KB entry budget; then rebuild the server so the a11y
# sweep serves the fresh embed (verify the embedded chunk hash matches web/dist)
cd web && bun run build
cd .. && rm -f bleephub-server && make build
strings bleephub-server | grep "$(ls web/dist/assets/index-*.js | xargs -n1 basename)"

# 0 axe violations across every route in BOTH themes, 0 theme leaks
cd web && SERVER_BIN=../bleephub-server bun run test:e2e -- a11y-theme-sweep

# Dead-code, duplication, dependency audit
./scripts/knip.sh && ./scripts/jscpd.sh && (cd web && bun audit)

# Ledger + parity inventory consistency (Go static-analysis gate)
./scripts/check-bugs-ledger.sh && ./scripts/parity_inventory.py --check
```

Deep-link integrity: the SPA handler (`internal/server/ui_embed.go`) serves
`index.html` for every `/ui/*` path, so hard refreshes and deep links load on all
routes. The a11y sweep exercises this by navigating each route with a fresh load.

## Known non-parity items (owner decisions, not open gaps)

- **ARCH-001** — the single flat `internal/server` package was split into a
  compiler-enforced data layer (`internal/store` on `internal/gitstore`) with the
  application layer importing it and never the reverse. Done.
- **WEB-016 / WEB-017** — `@bleephub/ui-core`'s operator-shell exports (~1,500 lines)
  are a maintained, tested library surface the SPA does not mount. Owner decision
  (2026-08-10): keep the library; do not prune.
