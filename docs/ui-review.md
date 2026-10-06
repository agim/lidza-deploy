# UX review implementation

The application now pins Līdza v0.1.81. The live Signal design retains its existing released Līdza palette. The graphite/silver mockup remains separate and is not part of the application assets.

The accepted review changes are:

- Account actions live in a keyboard-accessible topbar disclosure. Sign out is no longer between integration cards. Settings cards use the same column width and a 24px gap.
- Navigation uses matching inline SVG icons. Mobile navigation is a disclosure with a visible current-section label; all destinations remain reachable. Escape closes the disclosure and returns focus.
- Important buttons and fields are at least 44px high. Labels, supporting text, placeholders, focus and control borders use existing palette tokens. Action arrows have their own icon box and an 8px gap.
- New-app empty states distinguish an available server, no server and no search matches, with permission-aware next-step actions.
- App creation keeps required repository, server, hostname and database choices visible, with optional branch/environment settings behind a disclosure. Named dialogs keep actions visible during scrolling and retain native Escape/focus behavior.
- Integrations gives app creation secondary emphasis. Local GitHub limitations lead with a short explanation; installation detail is expandable.
- Workspace status reports refresh age, unavailable data or a pause for unsaved edits. Background polling does not replace edited forms. Saving one settings form preserves other unsaved fields; section navigation asks before discarding them. Drafts remain only in the current page, not browser storage.
- Login returns to a bookmarked console section. Return targets are restricted to the same-origin console page.
- Deployment queues emphasize pending, retrying and failed jobs; completed deliveries are collapsed.
- Setup explains the ownership key and its installed path. It reports already-paired agents without returning their credentials, and groups optional extra-server fields behind a disclosure.

## Verification

`npm test --prefix tests/browser` runs console workflow checks and the UX regressions. The UX suite checks palette preservation, alignment, spacing, touch targets, draft survival, safe login return, empty states, queue grouping, mobile navigation and keyboard focus. Its provider calls use local browser fixtures.

The 200% zoom-equivalent check uses a 720 CSS-pixel viewport for a 1440-pixel desktop. This checks responsive reflow; it is not a complete accessibility certification or a claim about every native browser zoom implementation.

The first-run browser suite exercises managed PostgreSQL, operator login and restart durability, and checks the already-paired agent presentation. Its public HTTPS origin is mapped to a local test backend; it does not establish public DNS or ACME acceptance.

Broader application, database/backup, feature, team, console-error and GitHub fixture suites remain separate functional checks. Real provider acceptance and the product's documented operational limits remain unchanged by these UI fixes.

## Walnut preview and reinstall recovery

Walnut is available at `/console.html?demo=1&design=walnut` and `/setup.html?design=walnut`. It shares Signal’s layout and uses the proposed brown/silver tokens. Signal remains the live default. `tests/browser/walnut.cjs` checks contrast, desktop/mobile/setup rendering and the unchanged default; point `TEST_WEB_URL` at a running panel or a static server serving `web/static`.

Managed setup recovers a missing or rejected database password through the labelled container’s local socket, with SQL on stdin, and seals the replacement using the existing framework credentials API. Existing data is retained, including when the container was recreated on its previous volume. Unrelated containers are refused; inspect failures report Docker access separately. No destructive reset option was added. Real-Docker regression tests cover both recovery cases, retained data and ownership refusal; a fake-Docker test covers inspect access errors.

## Persistent notifications

Action errors and status notices remain visible until dismissed. Successful background polling clears only recovered refresh errors, preserving action errors. Setup retains the previous error while a retry is in progress and focuses/scrolls to new errors. Each notice has a keyboard-accessible Dismiss button, and message text is inserted as text rather than HTML. `tests/browser/notifications.cjs` covers polling, expiry, retry retention, focus and dismissal against the actual UI with fixture API responses.

GitHub Apps are the exclusive authorization method. Legacy OAuth reconnect/configuration routes and saved-account token fallback are removed. Public deploys remain available without authorization. API integration and browser checks cover removed routes, manifest registration, selected repositories, installation tokens and repository attachment.

## Live deployment build output

Details & build logs updates every two seconds while a deployment is queued or building, including status and elapsed time. Follow latest output can be disabled to retain a reading position; Pause updates stops requests. Closing the viewer or reaching a final status stops log refreshes. Offline agents leave the last output visible and retries recover automatically. The agent exposes bounded command output during execution and redacts secrets even across stdout/stderr chunk boundaries. Tests verify output arrives before a controlled subprocess exits, partial-secret redaction and the viewer lifecycle. Upgrade both agent and control panel to receive output during long commands.

Deployment and application log dialogs provide Copy logs and Download logs. Deployment exports include application/server/deployment IDs, status, branch, domain, commit, elapsed time, the explicit failure and retained agent build output. Exports use the already-redacted API data and omit environment/credential objects. Clipboard failures remain visible inside the dialog with a download alternative. Browser checks cover matching copied/downloaded content, failure metadata and denied clipboard permissions.
