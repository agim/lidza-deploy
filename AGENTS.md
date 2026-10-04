# Līdza Deploy development

- This repository contains the deployment agent and a control-panel app built with Līdza. Do not modify the original MonolithCMS agent or put product app code into the Līdza framework repository.
- Before implementing infrastructure, check the released Līdza framework API and documentation. Reuse existing capabilities.
- If a missing capability is reusable across applications, open an issue in agim/lidza describing the gap, proposed behavior, acceptance tests, and required release. Pause dependent application work until the framework change is deployed/released. Verify the release and upgrade the dependency before resuming. Do not implement a local substitute to bypass that gate.
- Product-specific deployment orchestration, host inventory, domain assignments, and repository webhook behavior belong here.
- Each host must support multiple apps, each with its own FQDN and automatically renewed HTTPS certificate.
- The user approved Signal with the exact released Līdza dark brand palette. Use it as the live default; retain alternatives only as previews.
- Continue authorized implementation and validation autonomously; leave nonblocking questions until the end.
- Use existing isolated cloud checkouts; do not create Git worktrees unless asked.
