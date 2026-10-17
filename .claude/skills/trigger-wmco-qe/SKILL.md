---
name: trigger-wmco-qe
description: Safely trigger and monitor temporary openshift/release Prow rehearsals for WMCO QE using an OCP minor release and z-stream or y-stream selection. Use for requests such as "trigger WMCO 4.21 z-stream" or "trigger WMCO 5.0 y-stream", including resuming an existing temporary rehearsal PR. Do not use WMCO version numbers or custom image overrides.
---

# Trigger WMCO QE

Use the bundled controller to discover WMCO QE periodics, verify that their
workflows configure the OCP-specific latest WMCO catalog, create a temporary
non-draft do-not-merge release PR, request Prow rehearsals, and close the PR
without merging only after every expected rehearsal succeeds.

This is an installable, reusable project skill. Its command-like phrases are
not active Slack shorthand until a separate registration is completed. Do not
claim registration or installation was performed by this repository change.

## Runtime prerequisites

Require all of the following before live execution:

- Python 3.10 or newer, with PyYAML importable by that interpreter.
- `git`, `gh`, `make`, and either `podman` (the default) or Docker on
  `PATH`. The selected engine must be running and able to pull every generator
  image shown by `make -n update`.
- Network access to the Git remotes, GitHub API, generator image registries,
  and Prow. Allow substantial time and at least 5 GiB of free disk for the
  full `make update`, generator images, and dedicated worktree.
- An existing full clone of `openshift/release` with `upstream` pointing to
  `openshift/release` and `origin` pointing to a writable fork. Its reviewed
  checkout must be clean.
- Authenticated `gh` access that can create, comment on, and close a temporary
  pull request in `openshift/release`.
- A clean understanding that the temporary PR must never merge.

Never request credentials, print remote URLs containing credentials, or accept
an operator/catalog image override.

Repository identity accepts only complete, credential-free Git remote forms:
`https://github.com/<owner>/<repo>.git`, `git@github.com:<owner>/<repo>.git`,
or `ssh://git@github.com/<owner>/<repo>.git` (standard SSH port only). The
local Git transport proxy used by isolated development environments is limited
to `http://localhost:<port>/github.com/<owner>/<repo>.git`. Query strings,
fragments, extra path components, and other hosts are rejected.

## Interpret the request

Treat the version as an OCP minor release. Accept `4.21` or `5.0`; reject WMCO
versions such as `10.21` and `11.0`, patch versions, and arbitrary release
strings.

Normalize the stream to `z` or `y`:

- For z-stream, select every discovered WMCO job on AWS, GCP, Azure, or
  vSphere whose topology is IPI or UPI. Include all matching variants when
  present. Never select Nutanix by default.
- For y-stream, select every discovered matching WMCO QE job, including
  Nutanix when present.

Insert `zstream` or `ystream` immediately after the `winc` token in every
selected `as` value. Never reuse a job already containing either marker.

## Plan before writing

Locate the controller relative to this file. From the WMCO repository root:

```bash
SKILL_DIR=.claude/skills/trigger-wmco-qe
python3 "$SKILL_DIR/scripts/trigger_wmco_qe.py" plan \
  <ocp-minor> <z-stream|y-stream> \
  --release-repo /path/to/release
```

Review the printed selected jobs, renamed values, expected `ci/rehearse/...`
checks, rehearsal command, reviewed base SHA, catalog paths, and plan digest.
Planning is read-only. The release checkout's `HEAD` must be the exact
`upstream/main` revision that `start` will use; any later upstream change
requires a new plan review.

The plan prints the versioned closure-manifest identity/schema, typed topology
digest, and complete normalized command-inventory digest for every selected
workflow, plus the identity and digest of each trusted catalog role. The
manifest was audited from read-only public `openshift/release` commit
`0f72497967354022420becb86928c655a2b117c9`, which is the current source for
the supported OCP 4.21 and 5.0 AWS, GCP, and Azure connected WMCO jobs. It also
contains the public disconnected vSphere workflow. Every executable ref in
those closures has one exact registry path and whole-script SHA-256 digest;
normalization is limited to line endings and trailing horizontal whitespace.

Dependency YAML is parsed structurally with a finite workflow/chain/ref schema.
The topology preserves pre/test/post phase, ancestry, list position, dependency
kind, exact path, and explicitly allowed companion values. Both catalog roles
must be in the pre/provision path with the producer before the consumer; no
trusted role is accepted in test or post. Unknown nodes, scalar or ambiguous
dependencies, paths, scripts, digests, and topology fail closed. No Bash parser
or mutation regex is used as a fallback.

Manifest maintenance is intentionally manual. When public registry YAML or any
closure command changes, review the complete 4.21 and 5.0 selected closures,
the disconnected closure, every changed command and embedded template, and the
producer/consumer phase and order. Then bump the manifest identity, replace the
literal path/digest inventory and topology digests from the audited source,
refresh the four catalog-role fixtures, and rerun all offline regressions plus
read-only plans for both supported releases. Never derive an accepted digest
from the checkout being planned or accept a partial closure inventory.

The plan must prove that `releases.latest` selects the requested OCP minor and
that each selected workflow resolves through exactly one supported registry
path. Connected jobs use `openshift-windows-setup-wmco-konflux`; vSphere
disconnected jobs use `openshift-windows-setup-wmco-konflux-disconnected`,
including tag-to-digest resolution, mirroring, and a digest-backed `wmco`
CatalogSource. Follow the OCP-derived `:latest` image into that CatalogSource
and then into a WMCO subscription with channel `stable`, package
`windows-machine-config-operator`, and source `wmco`. Reject job or closure
overrides and competing setup paths. Report this only as "configured latest
catalog verified"; do not claim registry freshness beyond configuration
evidence.

Stop if the plan is empty, ambiguous, already stream-renamed, contains an image
override, lacks the catalog proof, or exceeds 25 rehearsals.

## Start the live lifecycle

Run live mode only when the user explicitly asks to trigger QE and the plan is
accepted:

```bash
SKILL_DIR=.claude/skills/trigger-wmco-qe
python3 "$SKILL_DIR/scripts/trigger_wmco_qe.py" start \
  <ocp-minor> <z-stream|y-stream> \
  --release-repo /path/to/release \
  --approved-plan-digest <exact-digest-from-plan> \
  --confirm-live-writes CREATE_TEMPORARY_DO_NOT_MERGE_PR
```

Use `--container-engine docker` only when Docker was the reviewed runtime.
`start` generates a collision-resistant run ID and prints its per-run state
path. A previously unused `--run-id` is supported for deterministic
automation. Never reuse or delete an older run's state, branch, worktree, or
uncertain resources.

The controller must perform these steps without shortcuts:

1. Recompute the read-only plan, require the exact approved digest, run the
   Python/container-engine/image-pull/disk preflight, fetch current
   `upstream/main`, and refuse if its SHA changed. Enforce 1–25 jobs before any
   write.
2. Persist `initializing` state with the exact origin fork, authenticated
   author, reviewed base, plan digest, run ID, branch, and worktree before
   creating the dedicated worktree and branch.
   Never delete or force-reuse an existing worktree, branch, remote branch, PR,
   or lifecycle state.
3. Rename only the selected config `as` values and run `make update` with the
   preflighted container engine.
4. Inspect tracked and untracked status and require the exact semantic diff:
   only the planned `as`, generated name, and generated target substitutions
   in the two expected files.
5. Commit and push without force, then create a non-draft PR against `main`
   whose stored base is exactly `main` and whose exact stored full title is
   `DEBUG Do not merge: OCP <version> <stream> stream WMCO QE rehearsals`.
   Every create/readback through adoption, monitoring, close, and close
   reconciliation must match that exact base, title, origin fork repository,
   branch, head SHA, open
   state when required, and non-draft state.
6. Wait for exactly one strict `[REHEARSALNOTIFIER]` table from the public
   `openshift-merge-bot[bot]` account and `openshift-merge-bot` GitHub App whose
   job set exactly equals the expected generated jobs. Preserve that identity
   for every later readback.
7. Refuse every existing trigger-capable `pj-rehearse` form. Post exactly
   `/pj-rehearse` for up to five jobs or `/pj-rehearse max` for six through
   twenty-five jobs. Pin the successful POST response's ID, body, timestamp,
   authenticated author, PR, and URL. Never adopt a same-text comment after an
   uncertain POST.
8. Monitor only exact `ci/rehearse/<expected-job>` evidence published by the
   public `openshift-ci[bot]` account or `openshift-ci` GitHub App for the
   current head and request. Accept only HTTPS Prow
   `pr-logs/pull/openshift_release/<PR>/rehearse-<PR>-<job>/<build>` URLs on the
   maintained host list. Collapse transitions and check/status duplicates by
   numeric build identity; distinct builds remain ambiguous. Conflicting
   maximum-timestamp transitions for one logical build are also ambiguous;
   only semantically equivalent ties collapse. Print the PR link and each
   available job link.
9. Immediately before closing, refresh the PR, notifier, requests, and all job
   evidence. Close via PR state PATCH without merging only when every expected
   job still has one current logical build whose latest transition succeeded.

Unrelated PR checks never satisfy expected QE coverage.

## Fail closed and resume

Leave the PR open and report evidence when any expected job is pending,
missing, failed, cancelled, timed out, stale, skipped, neutral, duplicated,
unrecognized, from a foreign publisher, or has an invalid build URL. Also
leave it open on extra trigger comments, notifier gaps, changed PR heads or
forks, draft or closed state, uncertain writes, malformed API responses, or
monitoring timeout.
Never merge, acknowledge rehearsals, or retry an uncertain write blindly.

The controller stores lifecycle state under the release clone's Git common
directory and preserves the dedicated worktree for inspection. Resume a safe,
nonterminal state with:

```bash
SKILL_DIR=.claude/skills/trigger-wmco-qe
python3 "$SKILL_DIR/scripts/trigger_wmco_qe.py" resume \
  <ocp-minor> <z-stream|y-stream> \
  --release-repo /path/to/release \
  --run-id <run-id-from-state-path> \
  --confirm-live-writes CREATE_TEMPORARY_DO_NOT_MERGE_PR
```

For a `blocked` or `uncertain` state, inspect and report the state file, PR,
current head, notifier, request, and expected job links. Do not mutate external
state automatically.

## Validate changes to this skill

Use offline fixtures and mocked command responses only:

```bash
SKILL_DIR=.claude/skills/trigger-wmco-qe
python3 -m unittest discover -s "$SKILL_DIR/scripts/tests" -p 'test_*.py' -v
python3 -m py_compile "$SKILL_DIR/scripts/trigger_wmco_qe.py"
ruff check "$SKILL_DIR"
ruff format --check "$SKILL_DIR"
```

Never exercise live mode as a test. Do not create a real PR, post a Prow
command, launch a rehearsal, mutate a registry, or merge anything during skill
development or validation.
