---
name: trigger-wmco-qe
description: Discover, confirm, trigger and monitor existing WMCO QE periodics through the OpenShift Gangway REST API using an OCP minor and z-stream or y-stream selection. Use for requests such as "trigger WMCO 4.21 z-stream" or "trigger WMCO 5.0 y-stream", or to resume polling recorded executions. Do not accept WMCO version numbers or image/payload overrides.
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
SKILL_DIR=.claude/skills/trigger-wmco-qe
python3 "$SKILL_DIR/scripts/trigger_wmco_qe.py" plan 5.0 z-stream \
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

## Submit once and monitor

Only after exact plan approval, have the approved provider pipe its token to:

```bash
python3 "$SKILL_DIR/scripts/trigger_wmco_qe.py" start 5.0 z-stream \
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
python3 "$SKILL_DIR/scripts/trigger_wmco_qe.py" resume \
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
python3 -m py_compile "$SKILL_DIR/scripts/trigger_wmco_qe.py"
ruff check "$SKILL_DIR"
ruff format --check "$SKILL_DIR"
```

Use mocked HTTP only. Do not launch real jobs, request live API authentication,
mutate clusters/registries, post Prow commands or write GitHub/Jira during tests.
Keep the development PR draft. Installation and live authorization are separate.
