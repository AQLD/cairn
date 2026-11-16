---
name: cairn
description: Run builds, tests, installs, servers and risky commands in a Cairn workspace (a private Linux sandbox that can be saved, rewound and copied) instead of on the user's machine. Use it whenever you would run code, install packages, start a dev server, or try something you might need to undo.
---

# Cairn workspaces

Cairn gives you a private Linux computer (gVisor sandbox) for this project. Its files, memory and running processes can be **saved**, **rewound** to any save, and **copied** to try several ideas at once. Nothing you run there can touch the user's machine.

The `cairn` command is on PATH. In a project set up with `cairn setup --project`, `.` names the project's workspace, so you never need an ID.

## Everyday loop

```sh
cairn run -- pytest -q              # push your local edits into the workspace, then run there (in the project folder)
cairn run -- npm install            # installs stay in the workspace, never on the user's machine
cairn exec . -- sh -c 'cd /root/PROJECT && make'   # run without pushing
```

Edit files locally as usual; `cairn run` copies them in before each command. `cairn pull . /root/PROJECT/dist ./dist` copies results back.

## Save before anything risky, rewind instead of repairing

```sh
cairn save . --full                 # ~1 s; files, memory and running servers; the workspace keeps running
cairn saves .                       # list saves (newest last), with the save each came from
cairn resume SAVE_ID                # rewind: files, memory and processes exactly as saved
```

- Save **before** dependency upgrades, migrations, deleting files, large refactors, or anything slow to redo.
- Save **after** each milestone that works (tests green, server up).
- If a step broke things or you went down a wrong path, **rewind** to the last good save rather than fixing by hand.

## Try several approaches at once

```sh
cairn copy SAVE_ID --count 3        # three independent workspaces starting from the save
cairn exec WS_ID -- sh -c '...'     # work in each copy by its ID
```

Compare the copies, keep the winner, `cairn rm` the others.

## Long-running servers and notebooks

```sh
cairn exec . -- sh -c 'cd /root/PROJECT && nohup npm run dev > /tmp/dev.log 2>&1 &'
cairn session new . python          # an interpreter whose variables survive between calls
cairn session run . SESSION_ID 'df = load(); df.describe()'
```

Processes keep running between commands and survive full saves and rewinds.

## Seeing what happened

```sh
cairn log . -n 30                   # every command with its exit code, every save and rewind
cairn status                        # is the engine up?  (`cairn doctor` explains any problem)
```

If `cairn` says the engine is not running, ask the user to run `cairn up` (or run it yourself if allowed).
