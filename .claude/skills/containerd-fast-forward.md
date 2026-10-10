# Containerd 1.7 Fast-Forward Skill

## Description

Update `openshift/containerd` branch `release-5.1` to the latest stable
upstream `v1.7.x` release. Preserve every justified downstream patch, prove the
resulting history, run the validations defined by the public repositories, and
open a pull request without merging it.

This is a full upstream version update. Do not use this workflow to cherry-pick
an individual fix.

## When to Use

- A user asks to update or fast-forward the OpenShift containerd 1.7 fork.
- A user asks to move `openshift/containerd:release-5.1` to the latest stable
  `v1.7.x` tag.

## Prerequisites

- Explicit authorization to push a branch to a GitHub fork and open a pull
  request against `openshift/containerd:release-5.1`.
- A full, clean clone of a writable fork of `openshift/containerd`. Never use a
  shallow, sparse, or partial clone for this workflow.
- GitHub authentication configured through the environment's credential helper
  or authorized SCM integration. Never put credentials in a remote URL.
- Bash (which must execute the fenced shell commands), Git, the Go version
  selected by the target tree, GNU Make, and the tools required by the target
  tree's `Makefile` and public CI configuration.
- An authenticated, authorized GitHub integration or GitHub UI for searching
  pull requests. Opening the PR may be handed off after a successful push when
  no authorized creation mechanism is available.

Stop before changing anything if repository access, the required toolchain, or
an authorized pull-request search mechanism is unavailable.

## Critical Rules

- Treat `https://github.com/openshift/containerd.git` as the downstream
  repository and `https://github.com/containerd/containerd.git` as upstream.
- Target only `openshift/containerd:release-5.1` and stable tags matching
  exactly `v1.7.<numeric>`. Exclude release candidates, beta tags, and other
  minor lines.
- Record immutable object IDs before creating the update branch. Never rely on
  a mutable branch name in the final evidence.
- Inventory downstream-only commits before choosing the history operation.
  Never infer the operation from the phrase "fast forward".
- Never drop, squash, or rewrite a downstream patch silently. Stop on unclear
  patch origin, ambiguous merge history, or a conflict whose correct resolution
  cannot be proved.
- Never force-push over an unknown branch. Never merge the pull request.
- Do not update the WMCO containerd submodule, WMCO version variables, or any
  WMCO/release CI configuration. Those are separate follow-on work.

### Bash Execution and Failure Contract

Every `bash` fence below is a complete safety gate. Run the clone fence from its
empty parent directory and every later fence from the same `containerd-update`
working directory; never copy individual commands past a failure. When running
a fence in a new Bash process, first restore every variable it references from
earlier phases using the recorded values. Each fence repeats `set -e` because a
separately invoked shell does not inherit that option. A fence must finish with
status zero before the workflow may continue.

Do not globally enable `pipefail`. The exact tag-selection filters intentionally
stop reading after the newest match, which can close their input early. Instead,
the blocks capture and guard every fallible Git producer before filtering its
output, so a producer failure cannot be hidden by a successful consumer.

## Historical Pattern

Use [openshift/containerd#10](https://github.com/openshift/containerd/pull/10)
as the public structural precedent, not as a command transcript.

The public graph proves that PR #10:

- started from the peeled commit `v1.7.27^{commit}` at
  `05044ec0a9a75232cad458027ca83437aae3f4da`;
- proposed the peeled commit `v1.7.33^{commit}` at
  `e8b1a9bc270f9952197c470b8bad573b03a3a608`;
- contained 193 commits because the old tag is an ancestor of the new tag;
- used a fork branch whose head equaled the `v1.7.33` commit;
- was integrated by the two-parent merge commit
  `3027a2d87a58f173aa3de07c7c1ee6de72266452`; and
- used those two peeled commits as the merge parents, with the merge tree equal
  to the `v1.7.33^{commit}` tree.

Therefore, PR #10 did not replay downstream carry patches. Use its fast-forward
description only when the current downstream base is an ancestor of the
selected upstream tag and no downstream-only patch must be carried.

## Phase 1: Check for Existing Work

Before cloning, use the authorized GitHub integration or GitHub UI to search
open pull requests in `openshift/containerd` with base `release-5.1`. Look for
generic 1.7 update or fast-forward PRs. After selecting the target tag, repeat
the search for that exact version and target commit before creating a branch.

If an open update PR exists:

1. Record its URL, base, head repository, head branch, and head SHA.
2. Compare its target tag and carry-patch decisions with this workflow.
3. Stop and report the existing work for its owner to continue. Do not reuse
   its branch or open a duplicate PR.

Treat a PR with the same target commit as overlapping even if its title does
not name the version. Do not rely on title search alone.

## Phase 2: Clone and Verify Remotes

Use neutral local names. Set `FORK_REPOSITORY` to the writable public fork's
`owner/repository` name, use the sanitized HTTPS URL below, and choose an empty
working directory. Authentication must come from the configured credential
helper or SCM integration, not URL userinfo.

```bash
set -e
FORK_REPOSITORY=${FORK_REPOSITORY:?set the writable owner/repository}
printf '%s\n' "$FORK_REPOSITORY" | \
  grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' || exit 1
git clone "https://github.com/$FORK_REPOSITORY.git" containerd-update
cd containerd-update
SHALLOW_STATE=$(git rev-parse --is-shallow-repository) || exit 1
test "$SHALLOW_STATE" = false
CLONE_STATUS=$(git status --short) || exit 1
test -z "$CLONE_STATUS"

git remote add openshift https://github.com/openshift/containerd.git
git remote add upstream https://github.com/containerd/containerd.git
ORIGIN_URL=$(git remote get-url origin) || exit 1
OPENSHIFT_URL=$(git remote get-url openshift) || exit 1
UPSTREAM_URL=$(git remote get-url upstream) || exit 1
test "$ORIGIN_URL" = "https://github.com/$FORK_REPOSITORY.git"
test "$OPENSHIFT_URL" = \
  "https://github.com/openshift/containerd.git"
test "$UPSTREAM_URL" = \
  "https://github.com/containerd/containerd.git"
```

If an organization requires an equivalent remote form, first normalize
`origin` to a credential-free URL for the same public repository, then update
the exact comparison without printing the URL. Verify that `origin` is the
intended writable fork.

Fetch the downstream base and namespace upstream tags so same-named fork tags
cannot hide an upstream object:

```bash
set -e
git fetch --prune openshift \
  refs/heads/release-5.1:refs/remotes/openshift/release-5.1 || exit 1
git fetch --prune upstream \
  'refs/tags/v1.7.*:refs/tags/upstream/v1.7.*' || exit 1

BASE_SHA=$(git rev-parse 'refs/remotes/openshift/release-5.1^{commit}')
test -n "$BASE_SHA"
```

Stop if the branch is missing, the remotes are not the expected public
repositories, or the working tree is not clean.

## Phase 3: Select and Verify the Latest Stable Tag

Apply an exact numeric filter, then select by descending version-aware order:

```bash
set -e
TAG_REFS=$(
  git for-each-ref --format='%(refname:strip=3)' \
    'refs/tags/upstream/v1.7.*'
) || exit 1
STABLE_TAGS=$(
  printf '%s\n' "$TAG_REFS" |
    awk '/^v1\.7\.[0-9]+$/ { print }'
)
ORDERED_STABLE_TAGS=$(
  printf '%s\n' "$STABLE_TAGS" |
    LC_ALL=C sort -V -r
)
LATEST_TAG=$(
  printf '%s\n' "$ORDERED_STABLE_TAGS" |
    awk 'NR == 1 { print; exit }'
)
test -n "$LATEST_TAG"

TAG_REF="refs/tags/upstream/$LATEST_TAG"
TAG_OBJECT=$(git rev-parse "$TAG_REF")
TARGET_SHA=$(git rev-parse "$TAG_REF^{commit}")
git cat-file -e "$TAG_OBJECT^{object}"
git cat-file -e "$TARGET_SHA^{commit}"
```

Verify the local object IDs against a fresh read of the exact public upstream
tag. For an annotated tag, the peeled `^{}` ref is the commit; for a lightweight
tag, the tag object and commit are the same object.

```bash
set -e
REMOTE_TAG_LINES=$(
  git ls-remote --tags upstream \
    "refs/tags/$LATEST_TAG" "refs/tags/$LATEST_TAG^{}"
) || exit 1
printf '%s\n' "$REMOTE_TAG_LINES"

REMOTE_TAG_OBJECT=$(
  printf '%s\n' "$REMOTE_TAG_LINES" |
    awk '$2 !~ /\^\{\}$/ { print $1 }'
)
REMOTE_TARGET_SHA=$(
  printf '%s\n' "$REMOTE_TAG_LINES" |
    awk '$2 ~ /\^\{\}$/ { print $1 }'
)
test -n "$REMOTE_TAG_OBJECT"
test "$TAG_OBJECT" = "$REMOTE_TAG_OBJECT"
if test -n "$REMOTE_TARGET_SHA"; then
  test "$TARGET_SHA" = "$REMOTE_TARGET_SHA"
else
  test "$TARGET_SHA" = "$REMOTE_TAG_OBJECT"
fi
```

Record `LATEST_TAG`, `TAG_OBJECT`, `TARGET_SHA`, and `BASE_SHA` in the work
notes. Stop on any mismatch; refetch and investigate instead of proceeding.

## Phase 4: Detect No-Op and Establish the Old Upstream Anchor

If the selected tag is already contained in the downstream branch, report a
no-op and stop without creating a branch or PR:

```bash
set -e
if git merge-base --is-ancestor "$TARGET_SHA" "$BASE_SHA"; then
  echo "release-5.1 already contains $LATEST_TAG at $TARGET_SHA"
  exit 0
else
  status=$?
  test "$status" -eq 1 || exit "$status"
fi
```

Find the newest stable upstream 1.7 tag already contained in the downstream
base. Review the result; do not accept an empty anchor.

```bash
set -e
TAG_REFS=$(
  git for-each-ref --format='%(refname:strip=3)' \
    'refs/tags/upstream/v1.7.*'
) || exit 1
STABLE_TAGS=$(
  printf '%s\n' "$TAG_REFS" |
    awk '/^v1\.7\.[0-9]+$/ { print }'
)
ORDERED_STABLE_TAGS=$(
  printf '%s\n' "$STABLE_TAGS" |
    LC_ALL=C sort -V -r
)
CURRENT_TAG=
CURRENT_UPSTREAM_SHA=
while read -r tag; do
  sha=$(git rev-parse "refs/tags/upstream/$tag^{commit}")
  if git merge-base --is-ancestor "$sha" "$BASE_SHA"; then
    CURRENT_TAG=$tag
    CURRENT_UPSTREAM_SHA=$sha
    break
  else
    status=$?
    test "$status" -eq 1 || exit "$status"
  fi
done <<< "$ORDERED_STABLE_TAGS"

test -n "$CURRENT_TAG"
test -n "$CURRENT_UPSTREAM_SHA"
git merge-base --is-ancestor "$CURRENT_UPSTREAM_SHA" "$BASE_SHA"
```

Require the old upstream anchor to be an ancestor of the target tag:

```bash
set -e
git merge-base --is-ancestor "$CURRENT_UPSTREAM_SHA" "$TARGET_SHA"
```

If it is not, stop. A diverged or rewritten upstream release line needs a
human-approved history plan.

Require exactly one best merge base before using triple-dot comparisons. Record
it and prove that the old upstream anchor is its ancestor:

```bash
set -e
MERGE_BASE_OUTPUT=$(git merge-base --all "$TARGET_SHA" "$BASE_SHA") || exit 1
MERGE_BASE_COUNT=$(printf '%s\n' "$MERGE_BASE_OUTPUT" |
  awk 'NF { count++ } END { print count + 0 }')
test "$MERGE_BASE_COUNT" -eq 1
MERGE_BASE_SHA=$(printf '%s\n' "$MERGE_BASE_OUTPUT" |
  awk 'NF { print; exit }')
git merge-base --is-ancestor "$CURRENT_UPSTREAM_SHA" "$MERGE_BASE_SHA"
```

Record `CURRENT_TAG`, `CURRENT_UPSTREAM_SHA`, and `MERGE_BASE_SHA` in the work
notes. Stop on an empty result, multiple best merge bases, or an unexpected
relationship; triple-dot output is unsafe evidence for ambiguous topology.

## Phase 5: Inventory Downstream Carry Patches

Inspect topology, commits, patch equivalence, and net tree differences. Keep
the command output in the PR preparation notes.

```bash
set -e
git log --graph --oneline --decorate --boundary \
  "$TARGET_SHA...$BASE_SHA"
git log --right-only --no-merges --reverse \
  --format='%H %s' "$TARGET_SHA...$BASE_SHA"
git rev-list --right-only --merges "$TARGET_SHA...$BASE_SHA"
git cherry "$TARGET_SHA" "$BASE_SHA"
git diff --stat "$TARGET_SHA...$BASE_SHA"
git diff "$TARGET_SHA...$BASE_SHA"
```

Classify every downstream-only commit as one of:

1. already upstream by ancestry or patch equivalence;
2. still justified and must be replayed;
3. intentionally obsolete, with explicit reviewer-approved justification; or
4. a tree-neutral integration merge, proved as described below; or
5. ambiguous, which is a stop condition.

Record every downstream-only commit in a disposition manifest, including the
evidence for an upstream-equivalent or intentionally obsolete classification.
`git cherry` is supporting patch-equivalence evidence; it is not sufficient on
its own when the patch context or intent changed.

`git cherry` does not classify merge commits. For every downstream-only merge,
inspect and record every parent. A merge may be classified as integration-only
only when it has exactly two parents, one parent is the peeled commit of a
specific fetched stable upstream tag, and the merge tree equals that upstream
parent's tree:

```bash
set -e
MERGE_SHA=${MERGE_SHA:?set the downstream-only merge SHA}
MERGE_PARENT_LINE=$(git show -s --format='%P' "$MERGE_SHA") || exit 1
read -r -a MERGE_PARENTS <<< "$MERGE_PARENT_LINE"
test "${#MERGE_PARENTS[@]}" -eq 2 || exit 1
git show -s --format='%H %P %T %s' \
  "$MERGE_SHA" "${MERGE_PARENTS[@]}" || exit 1

UPSTREAM_PARENT=${UPSTREAM_PARENT:?set the proved upstream parent SHA}
OTHER_PARENT=${OTHER_PARENT:?set the other recorded parent SHA}
MERGED_UPSTREAM_TAG=${MERGED_UPSTREAM_TAG:?set the stable upstream tag}
printf '%s\n' "$MERGED_UPSTREAM_TAG" | \
  grep -Eq '^v1\.7\.[0-9]+$' || exit 1
test "$UPSTREAM_PARENT" != "$OTHER_PARENT" || exit 1
test "$UPSTREAM_PARENT" = "${MERGE_PARENTS[0]}" || \
  test "$UPSTREAM_PARENT" = "${MERGE_PARENTS[1]}" || exit 1
MERGED_TAG_SHA=$(git rev-parse \
  "refs/tags/upstream/$MERGED_UPSTREAM_TAG^{commit}") || exit 1
MERGE_TREE=$(git rev-parse "$MERGE_SHA^{tree}") || exit 1
UPSTREAM_TREE=$(git rev-parse "$UPSTREAM_PARENT^{tree}") || exit 1
OTHER_TREE=$(git rev-parse "$OTHER_PARENT^{tree}") || exit 1
test "$MERGED_TAG_SHA" = "$UPSTREAM_PARENT" || exit 1
test "$MERGE_TREE" = "$UPSTREAM_TREE" || exit 1
test "$MERGE_TREE" != "$OTHER_TREE" || exit 1
test "$OTHER_PARENT" = "${MERGE_PARENTS[0]}" || \
  test "$OTHER_PARENT" = "${MERGE_PARENTS[1]}" || exit 1
```

The disposition manifest must name both parents, their roles, the verified
upstream tag, and the tree IDs. Any octopus merge, unknown parent, tree change,
or ambiguous parent role is a stop condition. Do not flatten such a merge or
choose a mainline parent without an explicit, reviewed maintainer decision.

## Phase 6: Construct the Update Branch

Choose a new descriptive branch name that includes `release-5.1` and the target
version. Repeat the open-PR search now that `LATEST_TAG` and `TARGET_SHA` are
known. Confirm the name does not collide with an unknown fork branch:

```bash
set -e
test -n "$LATEST_TAG" || exit 1
BRANCH="release-5.1-${LATEST_TAG#v}-update"
test -n "$BRANCH" || exit 1
git check-ref-format --branch "$BRANCH" >/dev/null || exit 1
REMOTE_BRANCH_REF="refs/heads/$BRANCH"
REMOTE_BRANCH_LINE=$(
  git ls-remote --heads origin "$REMOTE_BRANCH_REF"
) || exit 1
test -z "$REMOTE_BRANCH_LINE"

if git show-ref --verify --quiet "refs/heads/$BRANCH"; then
  echo "local branch already exists: $BRANCH"
  exit 1
else
  status=$?
  test "$status" -eq 1 || exit "$status"
fi
```

Stop rather than reusing or overwriting an unexpected branch.

For the PR #10 exact-tag pattern, require both conditions:

```bash
set -e
git merge-base --is-ancestor "$BASE_SHA" "$TARGET_SHA"
DOWNSTREAM_NON_MERGES=$(git log --right-only --cherry-pick --no-merges \
  --format='%H' "$TARGET_SHA...$BASE_SHA") || exit 1
DOWNSTREAM_MERGES=$(git rev-list --right-only --merges \
  "$TARGET_SHA...$BASE_SHA") || exit 1
test -z "$DOWNSTREAM_NON_MERGES"
test -z "$DOWNSTREAM_MERGES"
```

Then create the branch directly at the immutable tag commit:

```bash
set -e
git switch --create "$BRANCH" "$TARGET_SHA"
NEW_HEAD=$(git rev-parse HEAD) || exit 1
test "$NEW_HEAD" = "$TARGET_SHA"
```

This is the only path that may be described as a fast-forward. The branch head
is an exact upstream tag commit, and the old downstream base is its ancestor.

If the base is not an ancestor of the target but every downstream-only commit
is proven upstream-equivalent, explicitly approved as obsolete, or a
tree-neutral integration merge proved by Phase 5, create the branch directly
at `TARGET_SHA` only after completing the disposition manifest:

```bash
set -e
git switch --create "$BRANCH" "$TARGET_SHA"
NEW_HEAD=$(git rev-parse HEAD) || exit 1
test "$NEW_HEAD" = "$TARGET_SHA"
```

Describe this as an exact-tag update, not a fast-forward. List every commit not
replayed and its reviewed disposition in the PR body.

If reviewed carry patches remain, create the branch at `TARGET_SHA` and replay
only the approved commits, oldest first:

```bash
set -e
git switch --create "$BRANCH" "$TARGET_SHA"
git cherry-pick -x <oldest-approved-carry-sha> [...]
```

For each replay, record the old SHA, new SHA, reason it remains necessary, and
any conflict resolution. The resulting head will not equal the upstream tag;
describe it as an update with carry patches, not as exact tag ancestry.

### Conflict Handling

On a cherry-pick conflict:

1. Save `git status --short`, `git diff --name-only --diff-filter=U`, and the
   conflicting patch context.
2. Compare the old downstream version, the target-tag version, and the original
   patch intent.
3. Resolve only when the intended behavior is provable, then stage the files
   and run `git cherry-pick --continue`.
4. Re-run the entire validation phase after any resolution.

If intent is ambiguous, run `git cherry-pick --abort`, return to the recorded
immutable SHAs, and stop for maintainer direction. Never use `ours`, `theirs`,
`skip`, or an empty commit merely to make the operation finish.

## Phase 7: Validate the Result

First inspect the target tree's current `go.mod`, `Makefile`, and
`Makefile.windows`. Enumerate and inspect every workflow under
`.github/workflows/` (including `release.yml`, when present), every local action
under `.github/actions/`, and every reusable workflow or action referenced by a
pull-request-applicable job. Also inspect the current public `openshift/release`
CI configuration for `openshift/containerd:release-5.1`. Stop if any required
file or referenced local definition cannot be inspected.

Build an invocation-time inventory of every workflow and reusable action in the
selected target tree before running validation. For each workflow, record
whether its triggers and job conditions apply to a pull request targeting
`release-5.1`; give an explicit trigger or condition-based reason for every
exclusion. For every applicable job, record its source file and job name and map
each required command either to a local command that will be run or to an
explicit runner/PR-only disposition. The disposition must identify the source
file, job name, required runner or platform, reason local execution is not
applicable, and the authorized PR check or runner that will execute it. Run
every locally applicable command. Stop if one needs unavailable tools.

At the PR #10 revision, the public containerd build and CI files included the
following locally applicable commands. This historical list is non-exhaustive;
it does not replace the invocation-time inventory. The public Prow
configuration used for PR #10 invoked `GOOS=windows make` for both its binary
build and its `build` presubmit. This was Windows cross-build coverage, not
native Windows integration testing. Use these commands only if the current
checked-in files still define them:

```bash
set -e
make check
make test
make check-protos check-api-descriptors
make verify-vendor
make man
GOOS=windows make
```

Do not substitute remembered Prow commands or claim Windows integration
coverage. A capability, dependency, network, or platform failure is not a pass:
record the exact failure and stop unless the current public configuration and a
maintainer provide a documented disposition.

If the current `openshift/release` tree has no `release-5.1` presubmit, record
that fact and stop before publication for maintainer direction. Do not add or
change CI configuration in this workflow. If native Windows validation is
required, use the current upstream workflow on an authorized Windows runner;
do not translate selected workflow lines into an unverified local command.

After validation, prove the history and tree:

```bash
set -e
HEAD_SHA=$(git rev-parse HEAD) || exit 1
WORKTREE_STATUS=$(git status --short) || exit 1
printf '%s\n' "$WORKTREE_STATUS"
test -z "$WORKTREE_STATUS"
git log --graph --oneline --decorate --boundary "$BASE_SHA...$HEAD_SHA"
git diff --check "$BASE_SHA...$HEAD_SHA"
git diff --stat "$BASE_SHA...$HEAD_SHA"
git diff --name-status "$BASE_SHA...$HEAD_SHA"
```

Require a clean working tree. For either exact-tag path, require
`HEAD_SHA == TARGET_SHA`. For the carry path, require every extra commit to
appear in the approved carry manifest and no other commit to appear. Reconcile
the final graph against every entry in the downstream disposition manifest.

## Phase 8: Prepare and Open the Pull Request

Repeat the open-PR search immediately before pushing. Refetch the target base
and stop if it moved; restart the inventory from the new immutable base instead
of omitting newly landed downstream changes:

```bash
set -e
git fetch --prune openshift \
  refs/heads/release-5.1:refs/remotes/openshift/release-5.1 || exit 1
PUBLISH_BASE_SHA=$(
  git rev-parse 'refs/remotes/openshift/release-5.1^{commit}'
) || exit 1
test "$PUBLISH_BASE_SHA" = "$BASE_SHA"
```

Immediately before publication, freshly enumerate all upstream `v1.7.*` tags,
apply the same exact numeric filter and version ordering used in Phase 3, and
require the newest result to still equal `LATEST_TAG`. Then repeat the exact-tag
remote verification from Phase 3. Require the freshly observed tag object and
peeled commit to equal `TAG_OBJECT` and `TARGET_SHA`; stop if the selected tag
is no longer latest or either object changed.

```bash
set -e
PUBLISH_ALL_TAG_LINES=$(git ls-remote --tags upstream \
  'refs/tags/v1.7.*') || exit 1
PUBLISH_TAG_CANDIDATES=$(
  printf '%s\n' "$PUBLISH_ALL_TAG_LINES" |
    awk '$2 !~ /\^\{\}$/ {
      sub("refs/tags/", "", $2)
      if ($2 ~ /^v1\.7\.[0-9]+$/) print $2
    }'
)
PUBLISH_ORDERED_STABLE_TAGS=$(
  printf '%s\n' "$PUBLISH_TAG_CANDIDATES" |
    LC_ALL=C sort -V -r
)
PUBLISH_LATEST_TAG=$(
  printf '%s\n' "$PUBLISH_ORDERED_STABLE_TAGS" |
    awk 'NR == 1 { print; exit }'
)
test -n "$PUBLISH_LATEST_TAG"
test "$PUBLISH_LATEST_TAG" = "$LATEST_TAG"

PUBLISH_TAG_LINES=$(
  git ls-remote --tags upstream \
    "refs/tags/$LATEST_TAG" "refs/tags/$LATEST_TAG^{}"
) || exit 1
PUBLISH_TAG_OBJECT=$(
  printf '%s\n' "$PUBLISH_TAG_LINES" |
    awk '$2 !~ /\^\{\}$/ { print $1 }'
)
PUBLISH_TARGET_SHA=$(
  printf '%s\n' "$PUBLISH_TAG_LINES" |
    awk '$2 ~ /\^\{\}$/ { print $1 }'
)
test "$PUBLISH_TAG_OBJECT" = "$TAG_OBJECT"
if test -n "$PUBLISH_TARGET_SHA"; then
  test "$PUBLISH_TARGET_SHA" = "$TARGET_SHA"
else
  test "$PUBLISH_TAG_OBJECT" = "$TARGET_SHA"
fi
```

For a new branch, query the remote again without masking transport errors, then
make creation atomic with an empty expected-object lease:

```bash
set -e
REMOTE_BRANCH_LINE=$(
  git ls-remote --heads origin "$REMOTE_BRANCH_REF"
) || exit 1
test -z "$REMOTE_BRANCH_LINE"
git push --set-upstream \
  --force-with-lease="$REMOTE_BRANCH_REF:" \
  origin "$BRANCH:$REMOTE_BRANCH_REF"
```

The empty lease permits creation only while the remote ref does not exist. If a
competing branch appears, stop and repeat the PR search; do not overwrite it.
Never use a bare `--force-with-lease` or `--force`.

Before preparing commit messages or pull-request text, inspect the selected
target tree for its current repository-local contributor documents and links;
do not assume a root `CONTRIBUTING.md` exists. In the current `release-5.1`
tree, the `README.md` "Project details" section delegates contribution rules to
`containerd/project`'s `CONTRIBUTING.md`. Read the current linked guide and any
current repository-local pull-request template, follow their contribution,
sign-off, title, and PR-body rules, and record the source paths and immutable
revisions consulted. Stop if an applicable source cannot be read or if the
instructions conflict or leave the required format ambiguous. Historical pull
requests are evidence about prior updates, not contribution-policy authority.

Use this title only for the ancestry-proven fast-forward path, subject to the
current contributor guidance:

```text
[release-5.1] Fast forward to <LATEST_TAG>
```

For a converged exact-tag path, use:

```text
[release-5.1] Update to <LATEST_TAG>
```

For a carry path, use a truthful title such as:

```text
[release-5.1] Update to <LATEST_TAG> with downstream carries
```

Follow the current applicable contributor guidance for PR format. In addition,
include this workflow's required immutable update evidence:

```markdown
## Summary

Update the OpenShift containerd `release-5.1` line from `<CURRENT_TAG>` to the
upstream containerd `<LATEST_TAG>` tag.

- Downstream base: `<BASE_SHA>`
- Current upstream tag commit: `<CURRENT_UPSTREAM_SHA>`
- Single merge base: `<MERGE_BASE_SHA>`
- Upstream tag object: `<TAG_OBJECT>`
- Upstream tag commit: `<TARGET_SHA>`
- Proposed head: `<HEAD_SHA>`
- History method: `<ancestry fast-forward | converged exact upstream tag |
  upstream tag plus reviewed carries>`
- Downstream disposition manifest: `<each old SHA, classification, evidence,
  and replayed SHA when applicable>`
- Diff: `<commit count, files, additions, deletions>`

## Validation

- `<command>`: `<result>`
- Required pull-request checks: `<pending until the PR runs | result>`
- History and clean-tree checks: passed

## Upstream reference

- `https://github.com/containerd/containerd/releases/tag/<LATEST_TAG>`
- `<relevant public upstream release PR or notes>`
```

Open the PR against `openshift/containerd:release-5.1` through the authorized
SCM integration available in the environment, or through the authenticated
GitHub UI. Verify the returned PR URL, base branch, head branch, title, and body.
If no authorized creation mechanism is available, stop after the push and
report exactly who must open the prepared comparison; do not attempt raw API
calls or credential workarounds.

Wait for all currently required checks and report their URLs and conclusions.
Do not issue merge commands, enable auto-merge, or update WMCO as part of this
workflow.

## Stop Conditions

Stop and report the recorded SHAs plus the action needed to continue when:

- the latest stable tag cannot be selected or verified against upstream;
- `release-5.1` already contains the target tag (successful no-op);
- an overlapping open PR exists;
- the old upstream anchor or carry-patch set is ambiguous;
- a downstream merge or conflict cannot be resolved without guessing;
- current public build/CI requirements cannot be inspected or inventoried;
- a locally applicable command fails, or required runner/PR-only work lacks an
  authorized execution and disposition path;
- any required validation or PR check fails; or
- push or PR-opening authorization is unavailable.

## Public References

- Target-tree contributor-document pointer:
  https://github.com/openshift/containerd/blob/release-5.1/README.md#project-details
- Current containerd contributor guide:
  https://github.com/containerd/project/blob/main/CONTRIBUTING.md
- Historical PR #10 update precedent:
  https://github.com/openshift/containerd/pull/10
- OpenShift containerd fork: https://github.com/openshift/containerd
- Upstream 1.7 releases: https://github.com/containerd/containerd/releases
- Historical PR #10 Prow configuration:
  https://github.com/openshift/release/pull/85232/files
- Pinned PR #10 target Makefile:
  https://github.com/containerd/containerd/blob/e8b1a9bc270f9952197c470b8bad573b03a3a608/Makefile
- Pinned PR #10 target upstream CI workflow:
  https://github.com/containerd/containerd/blob/e8b1a9bc270f9952197c470b8bad573b03a3a608/.github/workflows/ci.yml
- Containerd build definitions:
  https://github.com/containerd/containerd/blob/release/1.7/Makefile
- Containerd Windows build definitions:
  https://github.com/containerd/containerd/blob/release/1.7/Makefile.windows
- Current OpenShift release-5.1 CI configuration:
  https://github.com/openshift/release/blob/main/ci-operator/config/openshift/containerd/openshift-containerd-release-5.1.yaml
