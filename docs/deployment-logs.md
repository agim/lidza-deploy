# Deployment logs

Deployment build logs include candidate startup diagnostics when `/readyz` fails: the latest health-check error and response excerpt, Docker container state (including exit and OOM information), and the last 200 runtime log lines. These are captured before candidate cleanup and use the deployment log's configured-secret redaction. The previous healthy release remains active. Open **Deployments → Details & build logs** to copy or download the diagnostic output. Runtime logs on the Applications page refer to the retained live release, not the discarded candidate.

A successful Docker build followed by `candidate failed /readyz; previous release retained` is a startup/readiness failure. The agent requires HTTP 200 from `/readyz` within 90 seconds before switching traffic. The saved health response and candidate logs distinguish configuration/database failures, crashes, memory limits, and missing readiness routes.

Older agent versions removed the failed candidate before collecting its startup logs. Update the hosting agent and redeploy to obtain the new diagnostics; discarded container logs cannot be recovered by this change.
