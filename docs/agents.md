# Working with several agents

How to run more than one coding agent on Cairn at once without them stepping on each other. The scenario catalog makes this easy: every task has an ID, a parent, a milestone and a test that says when it is done.

## The loop

1. **Plan (one agent or you):** pick the next scenarios from [test-plan.md](test-plan.md) for the current milestone, or split a big one into children in `spec/catalog.go`. Write each as a line under **In flight** in [status.md](status.md) with an owner.
2. **Build (one agent per scenario family, in parallel):** each agent works in its own git worktree and branch, implements its scenario test first, then the engine change, and proves the test fails without the change.
3. **Review (a fresh agent):** reviews the diff against the invariants in [CLAUDE.md](../CLAUDE.md). A reviewer that did not write the code catches what the author is blind to.
4. **Merge:** `make test && make plan`, update status.md, one PR per scenario family.

## Splitting work so agents don't collide

- **Split by goal, not by file.** G6 (saves), G7 (recovery) and G1.6 (exec) touch different parts of the engine. Two agents on the same goal will fight over the same functions.
- **Acceptance files are already split by goal** (`acceptance/g1_agent.go`, `g6_saves.go`…), so parallel agents rarely edit the same test file. The catalog (`spec/catalog.go`) is the one shared file: add your children in one small, separate commit first, or let the planning agent add them all up front.
- **Interfaces first.** When a task changes a shared boundary (`runtime.Runtime`, `client.Client`, the save manifest), land that change alone first, then fan out.
- **Claim before starting.** A line in status.md under **In flight** is the lock.

## Isolation: one worktree per agent

```sh
git worktree add ../cairn-g6 -b scenario/g6-retention   # agent 1
git worktree add ../cairn-g18 -b scenario/g1.6.8-stream  # agent 2
```

Each worktree has its own checkout and builds independently, and tests use temporary folders, so agents can run `make test` at the same time. In Claude Code you can also ask for a subagent with worktree isolation, which creates and cleans one up for you.

## Roles that work well

| Role | Does | Doesn't |
| --- | --- | --- |
| Planner | Breaks goals into scenarios, orders them, updates status.md | Write engine code |
| Builder (×N) | One scenario family: test → code → prove it bites | Touch other goals' files |
| Linux bench runner | Runs the suite with `runsc` on real droplets, reports numbers | Change code |
| Reviewer | Checks diffs against CLAUDE.md invariants, crash safety, error codes | Rubber-stamp its own work |

## Habits that keep quality up

- **Prove every test can fail.** Break the code it guards, watch it go red, restore. (The G6.1 corruption tests were checked this way.)
- **Skip, never fake.** If a scenario cannot run here, it skips with the reason. A pass on the `host` runtime does not count toward a gate; only the Linux bench with `runsc` does.
- **Small PRs, one scenario family each.** Easy to review, easy to revert.
- **Write decisions down.** Anything a future agent would otherwise re-litigate goes into the decisions log in status.md with the why.
- **Keep CLAUDE.md short and stable.** It loads every session; put fast-moving state in status.md instead.
