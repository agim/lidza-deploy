# GUI design directions

All three options are implemented in HTML/CSS/JavaScript and use the same application workflows. The user selected Signal with Līdza’s released dark brand palette. Live mode uses this design; the selector is available only in demo previews.

| Direction | Best fit | Layout |
|---|---|---|
| Studio | Everyday application management | Warm neutrals, mulberry accents, sidebar, spacious app cards |
| Signal | Frequent deployment operations | Dark interface, compact application rows, release-history landing page |
| Fleet | Multiple servers | Horizontal navigation, server-first landing page, app distribution by host |

The gallery is at `/designs.html`; the root opens the live workspace. Preview addresses are listed in README.md. Every preview marks its data as simulated and supports navigation, search, app creation, deployment progress, runtime-log dialogs, and rollback after a simulated redeploy. Live mode uses the same views with real API data and never generates demo resources.

Browser checks save screenshots of each direction and the mobile layout in `.local/screenshots/`. Tests run against the served production binary, which embeds the assets.

Standalone previews are in `design-previews/studio.html`, `signal.html`, and `fleet.html`. Open them directly in a browser; all assets are embedded, no login or server is required, and actions use simulated data. Regenerate them with `python scripts/export-design-previews.py`.

Signal uses the exact dark-mode brand tokens from Līdza v0.1.66 `packs/admin/templates/theme.css`: accent `oklch(76% 0.1 345)`, page `#171214`, surface `#201a1d`, sidebar `#241821`, text `#eee7e3`, muted text `#a89a93`, and borders `#372d32`. Its compact layout is specific to this deployment app.
