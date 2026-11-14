# OTE Presubmits

## Purpose

WMCO OTE presubmits validate source changes before merge. They are not a replacement for FBC postsubmits, periodics, or on-demand runs.

The job definitions live in `openshift/release`; this document records the intended WMCO presubmit policy.

The presubmit set is intentionally small:

1. One broad OTE canary runs on every `ote/` change.
2. Focused presubmits run only when their relevant OTE files change.
3. Broader platform coverage is handled by FBC postsubmits, periodics, and on-demand runs.

## Default presubmits

| Job | Trigger | Suite | Purpose |
|---|---|---|---|
| `aws-e2e-ote` | `^ote/` | `windows-machine-config-operator/all` | Broad OTE canary |
| `vsphere-proxy-e2e-ote` | Proxy-specific OTE files | `windows-machine-config-operator/proxy` | Validate proxy propagation |
| `aws-e2e-ote-tls` | TLS-relevant OTE files | TLS-focused selection | Validate TLS-sensitive behavior |

### Trigger policy

For the default WMCO presubmit set, only `aws-e2e-ote` should use the broad
`^ote/` trigger.

The disconnected vSphere smoke release job from
[openshift/release PR #88504](https://github.com/openshift/release/pull/88504)
is a separate release-validation lane and currently keeps its own broad
`^ote/` trigger.

The focused jobs should use narrower path-based triggers. Proxy checks and
their helpers are isolated to `proxy.go` and `proxy_utils.go`, so the proxy
trigger should target only those files (including the proxy helper unit test):

```yaml
run_if_changed: ^ote/test/e2e/(proxy|proxy_utils)(_test)?\.go$
```

The TLS job should use its own relevant path pattern once those files are separated.

When implementing this in `openshift/release`, replace the proxy job's existing `run_if_changed: ^ote/` field with the narrow path. Do not add `pipeline_run_if_changed` while leaving the broader `run_if_changed` field active.

### Disconnected smoke job

The disconnected vSphere OTE smoke job is being introduced in [openshift/release PR #88504](https://github.com/openshift/release/pull/88504).

That job uses `windows-machine-config-operator/smokerun` with job-local `TEST_SKIPS`, and its infrastructure is the reference implementation for disconnected OTE execution:

- Mirror the resolved extension image.
- Mirror required workload images.
- Preserve registry authentication and CA trust.
- Wait for the expected Windows MachineSet and Ready-node state.
- Run a narrow, stable smoke selection.

## Manual triggers

### WMCO presubmits

Use these commands on a WMCO pull request:

```text
/test ote-aws-e2e-ote
/test ote-vsphere-proxy-e2e-ote
/test ote-aws-e2e-ote-tls
```

Use these pipeline commands when applicable:

```text
/pipeline required
/pipeline remaining
```

### Release rehearsals

Use `/pj-rehearse` on an `openshift/release` pull request to rehearse a release job:

```text
/pj-rehearse pull-ci-openshift-windows-machine-config-operator-master-ote-vsphere-disconnected-e2e-ote
```

FBC postsubmits run after merge and are not triggered with `/test`.

### On-demand execution

For targeted or release-milestone validation, use the supported OpenShift CI Gangway API or the existing trigger-jobs step. Do not create temporary release PRs for manual execution.

## Suite policy

- `aws-e2e-ote` remains the broad OTE canary and uses `windows-machine-config-operator/all`.
- `windows-machine-config-operator/release-verification` is a Smokerun-focused suite that excludes `ChkUpgrade`, `ConnectedOnly`, `[Disruptive]`, and `[Serial]` for stable release-validation lanes.
- Focused presubmits use the smallest suite that covers their risk.
- Disconnected jobs should use the smoke selection and infrastructure from PR #88504.
- Broader platform and full-suite coverage belongs in periodics or on-demand runs, not every OTE pull request.
