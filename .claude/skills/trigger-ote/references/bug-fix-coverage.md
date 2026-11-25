# Bug-fix coverage workflow

This skill does not decide which tests a bug fix needs. That is a reasoning task
about a diff and a defect, and a planner that guesses coverage would be worse
than no planner. What this skill provides is the reviewed envelope an answer has
to fit into, plus a strict check of it.

## Division of labour

| Step | Owner |
|---|---|
| Read the bug, read the fix diff, summarise the behaviour change | reasoning agent |
| Map changed files to WMCO areas | reasoning agent |
| List existing tests and their tags | reasoning agent, from `ote/test/e2e/*.go` |
| Pick candidate aliases for those areas | reasoning agent, filtered by `areas` in `aliases --json` |
| Prove suites, OS, platforms, caps, skips and steps | this skill |
| Print the exact job list and digest | this skill |
| Execute | a human, after approving the digest |

## Turning a diff into candidates

```bash
git diff --name-only origin/master... | \
  .claude/skills/trigger-ote/scripts/trigger_ote.py advise --paths-from - --json
```

`--json` returns candidate metadata only (alias, areas, defaults, caps, reasons);
it contains no command templates. The text output is what prints a ready
`alias-plan` command, and only for runnable candidates. Either way the operator
must supply a reviewed SHA and run `alias-plan` to learn whether a matching job
exists at that revision.

`advise` maps each path to areas through `path_patterns` in the registry, returns
the aliases whose `areas` overlap, ranks explicit/narrow selections ahead of
`platforms: any` suites, keeps `disruptive` and `full-regression` in a separate
escalation list, and emits a ready `alias-plan` command per candidate. Aliases whose selection cannot
be proven from the job configuration (`csi-storage`, `multi-os` - tag-based
today) are returned in a `manual` list with the reason and without a plan
command, so no one is handed a command that is certain to be refused. Unmatched
paths are reported rather than guessed. The output is advice; the digest comes
from `alias-plan`.

## Area index

Every alias declares `areas` in `references/coverage-aliases.yaml`, and
`aliases --json` exposes them, so a front end can map a diff to candidates
without hardcoding anything:

| Area | Candidate aliases |
|---|---|
| `proxy`, `network` | proxy |
| `byoh`, `windows-instances` | byoh-lifecycle |
| `storage`, `csi`, `volume` | csi-storage |
| `multi-os`, `windows-version` | multi-os |
| `tls`, `metrics` | tls |
| `disconnected`, `registry`, `mirroring` | disconnected |
| `node-lifecycle`, `services`, `csr` | non-disruptive, smokerun |
| `release-path`, `catalog`, `olm` | release-verification (release-path only) |
| `serial`, `disruptive` | disruptive (justification required) |
| `breadth`, `platform` | platform-matrix |
| `regression` | full-regression (2 platforms, never scheduled) |

## What "validate this fix" means

- Per-bug validation is **not** `release-verification`. That alias is the release
  canary for promoted FBC catalogs and images; it answers "is the promoted
  release path healthy", not "did this fix work".
- Use the narrowest alias whose suite contains a test that can fail on the bug.
  If none does, say so - that is the most valuable output of the exercise.
- `disruptive` and `full-regression` need a written justification and a human
  decision; they are not routine fix validation.
- Prefer the smallest platform set. One platform that exercises the changed code
  beats four that do not.

## Status

Planning is implemented. **Submitting a plan against a fix PR is not**: the skill
executes periodic plans today, and presubmit execution needs `refs` support
(`base_ref`, `base_sha`, `pulls`) that is tracked separately. Until then, the
output of this workflow is: recommended alias, plan-only command, digest, plus a
proposal for new coverage.
