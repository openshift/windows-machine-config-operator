---
name: trigger-ote
description: Resolve a reviewed WMCO OTE coverage alias (release-verification, non-disruptive, smokerun, tls, byoh-lifecycle, proxy, ...) into existing openshift/release jobs, require approval of the exact plan and digest, and submit once through the OpenShift Gangway REST API only for ordinary periodic plans. Alias requests stop at the plan and digest until refs support lands. Use for /trigger-ote requests such as "trigger non-disruptive on gcp-ipi" or "plan full-regression", or to resume polling recorded executions. Never accept job names, image or payload overrides invented by the caller.
---

# Trigger WMCO QE

Trigger existing, trusted public `openshift/release` periodics, not new jobs.
Require approval of the exact job list before any POST. Keep installation and
Slack command registration separate; this package does not register itself.

## Prerequisites and authentication

Require Python 3.11+, PyYAML, a POSIX filesystem supporting `flock` and atomic
rename, read access to public GitHub source, and HTTPS access to Gangway for
live operation. Planning requires no token, cluster access or writable clone.
Do not require GitHub write access, a container engine or a generator.

For runtime only, use an **already approved app.ci OAuth token provider**.
`--token-stdin` consumes its output in memory, without prompting or storing it.
Never ask someone to paste a token into chat, put tokens in arguments/files,
enable shell tracing, or assume development-pod credentials authorize Gangway.
For example, outside development/tests, pipe an existing approved provider into
the documented command. If using `oc whoami -t`, explicitly choose the authorized
app.ci context, not whichever context happens to be current. This helper does
not log in, locate credentials, mint permanent tokens or implement token renewal.

Bearer requests go only to
`https://gangway-ci.apps.ci.l2s4.p1.openshiftapps.com/v1/executions`.
Do not change this to an arbitrary host, use HTTP, disable certificate validation,
follow redirects or add request/response tracing. Environment proxy settings are
not used. Do not send a bearer token when opening returned Prow links.

## Discover and approve

Interpret inputs as canonical **OCP major.minor**, not WMCO versions or patches.
Normalize z/z-stream and y/y-stream:

- z: all available AWS/GCP/Azure/vSphere IPI/UPI WMCO ordinary-nightly jobs,
  including suffix variants and disconnected vSphere; exclude Nutanix.
- y: every matching ordinary-nightly WMCO job, including other platforms when
  their trusted configuration satisfies the supported catalog contract.

Use the ordinary `amd64-nightly` configuration. Separate upgrade-from-stable
variants are not part of this request, even when their names contain `nightly`.
Unavailable platforms are not invented. An impossible automatic cron date does
not exclude an existing manually triggerable periodic.

Choose a reviewed full commit SHA from public `openshift/release` main:

```bash
SKILL_DIR=.claude/skills/trigger-ote
python3 "$SKILL_DIR/scripts/trigger_ote.py" plan 5.0 z-stream \
  --source-sha <reviewed-full-release-commit-sha>
```

Review every printed existing job name, OCP minor, stream, catalog refs, source
SHA, source-content fingerprint and approval digest. Confirm this exact list
with the requester. Discovery cross-checks config tests against actual generated
periodics, cron, target and variant, rather than assuming constructed names exist.
At public source `b0cb7d2854bb16b43bb855433869a11420693457`, 4.21 has three
jobs (AWS IPI, GCP IPI, Azure IPI, `winc-f14`), while 5.0 has five (also AWS
UPI and disconnected vSphere IPI, `winc-f7`). Discover again for other revisions.

Trust upstream configuration, not arbitrary user-supplied shell. Support the
`cucushift-installer-rehearse-<platform>-<ipi|upi>[-disconnected]-ovn-winc`
workflow family with its direct first provision chain. Require configured
connected `openshift-windows-setup-wmco-konflux` before `operatorhub-subscribe`,
or disconnected `openshift-windows-setup-wmco-konflux-disconnected` before
`openshift-windows-install-wmco`. Connected subscription must use package
`windows-machine-config-operator`, source `wmco`, channel `stable`. Reject
unsupported shapes and job-level image/catalog/subscription overrides.
Permit configured infrastructure leases, including the disconnected vSphere
bastion lease; leases do not change the catalog contract.

The trusted producer scripts read the **actual cluster OCP minor** and select
the release-specific WMCO FBC `:latest`; disconnected setup resolves and mirrors
that tag by digest. Planning fingerprints these scripts for review; it does not
prove arbitrary Bash dataflow or maintain a whole-workflow digest allowlist.
This contract was source-reviewed at the revision above. Review producer changes
when approving another revision. Latest is resolved at job runtime, not a digest
pinned by this skill. Cluster profiles and CI build clusters do not establish
payload/catalog version. Preserve configured OCP nightly payload selection;
never inject image, environment, refs or payload overrides.

## Coverage aliases

An alias names a coverage intent, not a job. The reviewed registry lives in
`references/coverage-aliases.yaml`; resolving an alias against a reviewed
`openshift/release` SHA produces the exact job list to approve, and nothing is
submitted until that list is confirmed.

The registry targets the OTE job family in
`openshift/windows-machine-config-operator` (`_master__ote.yaml` plus its
generated presubmits and periodics). The legacy `openshift-tests-private`
periodics are being archived and are not valid targets.

Aliases select by suite, so the resolver requires the job's `TEST_SUITE` env to
equal the alias suite, and requires the job to pin the requested OS. A job that
declares neither cannot be selected: an alias that cannot prove which suite and
image ran is worse than no alias.

Platform scope follows the release configuration. `platforms: any` accepts every
discovered OTE job that proves the suite and OS; an explicit list
(`byoh-lifecycle`, `proxy`, `disconnected`, `csi-storage`, `multi-os`) rejects
anything outside it. Defaults keep a bare invocation predictable:

```bash
python3 "$SKILL_DIR/scripts/trigger_ote.py" aliases --json          # reviewed catalogue
python3 "$SKILL_DIR/scripts/trigger_ote.py" alias-plan smokerun \
  --source-sha <sha> --json                                        # defaults: aws-ipi, os 2025
python3 "$SKILL_DIR/scripts/trigger_ote.py" alias-plan smokerun \
  --source-sha <sha> --platform azure-ipi --platform vsphere-ipi   # wider, still capped
python3 "$SKILL_DIR/scripts/trigger_ote.py" alias-plan csi-storage \
  --source-sha <sha> --os "Windows Server 2022"                    # OS spelling is normalized
```

Rules the resolver enforces, and refuses loudly otherwise:

- `--platform` selects; omitting it uses `default_platforms`. A platform outside
  an explicit alias list is rejected, never widened silently.
- `--os` accepts `2019`, `2022`, `2025` or human spellings such as
  `Windows Server 2022`; omitting it uses `default_os`. The job must pin it.
- `jobs` pins an alias to reviewed job targets (`tls` -> `aws-e2e-ote-tls`), so a
  suite match on a different job is not accepted.
- `requires_steps` proves the job declares the setup step the suite depends on
  (`enable-tls-adherence`, `set-wmco-byoh-ote-environment`).
- `max_platforms` caps the plan (`smokerun` 3, `non-disruptive` 4, `tls` 1,
  `disruptive` 1, `full-regression` 2); `not_for` rejects a trigger kind.
- `--json` prints only JSON, including `digest` and `warnings`; exit status is 0
  for a plan and 2 for a refusal, with the reason on stderr.

Two limits are deliberate. `smokerun` and `non-disruptive` include proxy cases
that only pass on a proxy cluster, so they carry `requires_skips`, whose
alternatives must be provable from the job's `TEST_SKIPS` expression; the plan
prints the resulting skip set for approval. Tag-selected aliases (`csi-storage`,
`multi-os`) resolve only when the job exposes a matching selector; until then the
plan fails instead of approximating the selection.

## Bug-fix coverage advice

Choosing tests for a bug fix is a reasoning task, not a planning task: read the
fix diff, map it to WMCO areas, list the existing tests and their tags, then ask
this skill to prove that a candidate alias actually selects the jobs that run
them. `references/bug-fix-coverage.md` records the division of labour, the
area-to-alias index that `aliases --json` exposes through `areas`, and the rule
that per-bug validation is **not** `release-verification` - that alias is the
release-path canary for promoted FBC catalogs.

`advise` turns a bug-fix diff into candidates without guessing:

```bash
python3 "$SKILL_DIR/scripts/trigger_ote.py" advise --json \
  --path pkg/services/services.go --path pkg/csr/csr.go
# or a saved list of changed files
git diff --name-only origin/master... > /tmp/changed.txt
python3 "$SKILL_DIR/scripts/trigger_ote.py" advise --paths-from /tmp/changed.txt --json
```

It maps paths to areas through `path_patterns` in the registry, returns the
aliases whose `areas` overlap, ranks narrow selections before broad suites, lists
`disruptive`/`full-regression` separately as requiring justification, and prints
a ready `alias-plan` command per candidate with the alias defaults. Paths that
match nothing are reported, never guessed; absolute and parent-relative paths are
flagged as invalid instead of being reinterpreted, and only a literal leading
`./` is stripped so dot-directories keep their real name. A supplied
`--source-sha` is validated as a full 40-character SHA before any command text is
printed. Aliases that cannot be proven from the
job configuration - currently the tag-selected `csi-storage` and `multi-os` - are
reported as `manual` with their reason and get no plan command, because printing
a command that is guaranteed to be refused would be misleading. Feed the chosen command back through
`alias-plan` for the real digest.

Two consequences worth stating plainly:

- If no existing test can fail on the bug, say that first. A green run of an
  unrelated suite is worse than an admitted gap, and the proposal for new
  coverage is the useful output.
- Planning is implemented; submitting a plan against a fix PR is not. Presubmit
  execution needs `refs` support, so the workflow currently ends at a recommended
  alias, a plan-only command and a digest.

## Submit once and monitor

Only after exact plan approval, have the approved provider pipe its token to:

```bash
python3 "$SKILL_DIR/scripts/trigger_ote.py" start 5.0 z-stream \
  --source-sha <same-reviewed-full-sha> \
  --approved-plan-digest <exact-reviewed-digest> \
  --confirm-live TRIGGER_EXISTING_PERIODICS \
  --state /private/run-directory/wmco-run.json --token-stdin
```

The helper recomputes the plan before submission. The SHA/fingerprint identify
reviewed source; they cannot transactionally pin Gangway's asynchronously synced
server configuration. Do not claim otherwise. Each POST contains **only**
`job_name` and `job_execution_type: "1"`. Persist submitting intent before POST
and its returned execution ID before advancing. State is bounded, mode 0600,
atomically replaced/fsynced and protected by an exclusive per-run lock. Keep the
run directory private; preserve its state and lock file for inspection.

Before start, create a dedicated run directory with `mkdir -m 700` as the current
user. `--state` must be an absolute path in that existing private directory, with
an alphanumeric-leading `.json` basename (up to 101 characters, using letters,
digits, dots, underscores or hyphens). Relative paths, `..` traversal and symlinked
directory ancestors are rejected. Existing state/lock files must be writable,
owned by the current user, private, single-link regular files; symlinks, hardlinks
and shared directories are rejected. Start never overwrites existing state;
resume requires a valid recorded run. A missing directory is not created implicitly.
This deliberately restricts CLI file output, not a network-provided state path.
Ancestors must be owned by this user or root and not group/world-writable unless
sticky (such as `/tmp`). Other processes running as the same user are outside
this filesystem isolation boundary.

A timeout, HTTP error, malformed reply or interrupted POST is **uncertain**.
Halt submission; never retry automatically, create replacement state to retry,
or adopt another execution by name/time. A crash leaving `submitting` has the
same meaning. Report unsubmitted jobs as missing. An operator must reconcile
uncertain creation separately before explicitly approving any new request.

Resume **polling only**, using the same provider and original state:

```bash
python3 "$SKILL_DIR/scripts/trigger_ote.py" resume \
  --state /private/run-directory/wmco-run.json --token-stdin \
  --wait-seconds 21600 --poll-seconds 30
```

Default monitoring performs one poll. Resume never submits, even for missing or
uncertain jobs. GET errors report a safe diagnostic and never trigger POST.
Check exact ID, job name and PERIODIC type. TRIGGERED/SCHEDULING/PENDING mean
not complete. SUCCESS/FAILURE/ABORTED/ERROR are Prow outcomes. Retain validated
terminal outcome, run URL and completion timestamp independently, even when other
fields are missing. Require all three for successful completion. Contradictory
evidence persists as CONFLICT and cannot be cleared by polling or resume; transport
POLL_ERROR can recover through matching GET evidence. Missing/unknown/conflicting
evidence never establishes success. Print every requested job and its Prow link
when available; report overall SUCCESS only for complete, unambiguous success
of **every** requested job. Return 2 for any incomplete/unsuccessful result.

## API evidence and integration limits

Use the [OpenShift REST guide](https://docs.ci.openshift.org/how-tos/triggering-prowjobs-via-rest/)
and [upstream Gangway documentation](https://docs.prow.k8s.io/docs/components/optional/gangway/).
The deployed Prow revision `c06373a3e` defines the
[proto](https://github.com/kubernetes-sigs/prow/blob/c06373a3e/pkg/gangway/gangway.proto)
and [handler](https://github.com/kubernetes-sigs/prow/blob/c06373a3e/pkg/gangway/gangway.go).
POST returns a JobExecution object with `id`, `job_name`, `job_type`,
`job_status: "TRIGGERED"`; acceptance is not test success. `id` is the ProwJob
metadata name, **not** a numeric build ID. GET `/v1/executions/{id}` returns
snake_case `job_name`, `job_type`, `job_status`, `job_url`, and optional
`create_time`/`completion_time`. GET does **not** populate `gcs_path`.
`create_time` maps to Prow status startTime, and completion_time to completionTime.

The [public OpenShift Envoy recipe](https://github.com/openshift/release/blob/b0cb7d2854bb16b43bb855433869a11420693457/clusters/app.ci/supplemental-ci-images/envoyproxy/envoyproxy.yaml)
has a path-rewrite/body-handling discrepancy with documented GET. A source recipe
is not proof of running-image behavior or production failure. Use documented GET,
report unsupported/auth/proxy/malformed responses, and do not invent a POST-based
GET workaround. Authenticated live GET/POST and end-to-end acceptance are **untested**.

## Offline validation

```bash
python3 -m unittest discover -s "$SKILL_DIR/scripts/tests" -p 'test_*.py' -v
python3 -m py_compile "$SKILL_DIR/scripts/trigger_ote.py"
ruff check "$SKILL_DIR"
ruff format --check "$SKILL_DIR"
```

Use mocked HTTP only. Do not launch real jobs, request live API authentication,
mutate clusters/registries, post Prow commands or write GitHub/Jira during tests.
Keep the development PR draft. Installation and live authorization are separate.
