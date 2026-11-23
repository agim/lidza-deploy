# Claude notes

## Never burst an SSH server

Never open SSH connections in quick succession. Don't loop `ssh`/`scp`/`rsync`
once per file or command. Open one multiplexed connection
(`ssh -o ControlMaster=auto -o ControlPath=~/.ssh/cm-%r@%h:%p -o ControlPersist=60 …`)
and run every later command through it, or batch the work into a single
`ssh host 'script'` or one `rsync`/`tar` stream. If a connection is refused or
times out, don't retry: stop and tell the owner. (2026-11-23: about 20 quick
scp connections got a server auto-blocked by its router for ~10 minutes.)

## Ask questions interview-style, with a recommendation

When a decision is the owner's, ask it briefly and interactively: use the
question dialog (AskUserQuestion) with one short decision per question, a few
options at most, and your recommended option first, marked "(Recommended)" with
a one-line reason. Never end a reply with a list of options and no
recommendation. Where no question dialog exists (an unattended cloud session, a
peer message), still name the recommendation and ask one brief question.
