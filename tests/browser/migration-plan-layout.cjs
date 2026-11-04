const assert = require('node:assert/strict');
const { chromium } = require('playwright');

const base = process.env.TEST_WEB_URL || 'http://127.0.0.1:8880';

async function main() {
  const browser = await chromium.launch({
    executablePath: process.env.TEST_CHROMIUM_PATH || '/usr/bin/chromium',
    args: ['--no-sandbox'],
  });
  try {
    for (const width of [390, 1010, 1440]) {
      const page = await browser.newPage({ viewport: { width, height: 900 } });
      await page.route('**/api/control/apps/*/migration-plan', route => route.fulfill({ json: {
        checked: '2026-11-04T17:00:00Z', commit: '4c0c6f9ee962', domains: ['portal.example.com'],
        inventory_limited: true, databases: [], tasks: [],
        blockers: ['Verify database and file backup ownership before moving.'],
        steps: ['Check destination capacity.', 'Review the traffic switch and rollback.'],
      } }));
      await page.goto(base + '/console.html?demo=1#applications');
      for (const view of ['Grid view', 'List view']) {
        await page.getByRole('button', { name: view }).click();
        for (const app of ['customer-portal', 'documentation']) {
          const card = page.locator(`[data-app-card="${app}"]`);
          const panel = card.locator('.migration-plan');
          await panel.locator('summary').click();
          await panel.getByText('Review the traffic switch and rollback.').waitFor();
          const layout = await card.evaluate(element => {
            const rect = selector => {
              const box = element.querySelector(selector)?.getBoundingClientRect();
              return box && { left: box.left, right: box.right, top: box.top, bottom: box.bottom };
            };
            const card = element.getBoundingClientRect();
            return { left: card.left, right: card.right, content: rect('.application-card-content'),
              footer: rect('.app-card-footer'), error: rect('.app-deployment-error'), panel: rect('.migration-plan') };
          });
          assert(layout.panel.top >= Math.max(layout.content.bottom, layout.footer.bottom, layout.error?.bottom || 0) - 1,
            `${view} ${width}px ${app} plan overlaps the application row: ${JSON.stringify(layout)}`);
          assert(layout.panel.left <= layout.left + 2 && layout.panel.right >= layout.right - 2,
            `${view} ${width}px plan does not span the application card`);
          await panel.locator('summary').click();
        }
      }
      await page.close();
    }
  } finally {
    await browser.close();
  }
  console.log('PASS: expanded move plans stay below app details, controls, and errors in grid and list views.');
}

main().catch(error => { console.error(error); process.exitCode = 1; });
