# Server root console

Open **Servers → More actions → Server console**. Only administrators with
two-factor authentication enabled can use it. Verify an authenticator or recovery
code to unlock one server for 15 minutes; verify again to extend access.
Submitting a command is recorded through the control panel's existing audit flow.
Neither command contents nor output are copied into audit metadata.

The browser submits a command ID and receives a short acknowledgement. The agent
runs the command independently of that HTTP request, with a ten-minute runtime
limit and an 8 MiB output limit. Output is read in chunks of up to 64 KiB while the
command runs. There is one active console command per server. Agent upgrades wait
for the active command to finish.

Connection failures preserve output and reconnect to the same job rather than
submitting the command again. Navigating away or reloading resumes it using an
opaque ID in browser session storage; commands and output are not stored there.
After the MFA unlock expires, verify again to resume reading output. Output is
scoped to the administrator who started the job and requires the same server's
MFA unlock on every read.

The agent keeps the latest eight command outputs in memory, for at most 30
minutes, and retains up to 256 command identities during that window to prevent
duplicate execution after a lost acknowledgement. Outputs are never written to
disk. Agent restart stops active console processes and loses console history.
If a result is missing or expired, the UI explicitly reports that its outcome
must be checked before rerunning it; it does not silently retry execution.

Upgrade both the control panel and hosting agent for background console support.
The old synchronous endpoint remains available for older control panels.

Use **Cancel command** or **Ctrl+C** (with no text selected) to interrupt the
active command. Ctrl+C retains its normal Copy behavior when text is selected.
Cancellation requires the same administrator and a current server MFA unlock,
and is audited. The agent sends SIGINT to the command's process group, then
force-stops processes that do not exit within three seconds. Output remains
available with a cancelled status and exit code 130. Repeating Cancel is safe;
it never starts a new command. Stopping a client command cannot undo operations
already applied or necessarily stop work delegated to Docker or other services.
