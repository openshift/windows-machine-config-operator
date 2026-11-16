---
name: wmco-local-debug
description: Get a live WMCO cluster bound to a pull request and work against it - an opt-in debug presubmit from openshift/release on any platform it supports, requested on the pull request and followed to its hold, or cluster-bot, finding the run, kubeconfig and access, installing the operator, iterating on it, building and running OTE or legacy go test e2e tests, releasing the hold, telling CI environment limits apart from code failures, and adding another opt-in debug variant. Use for any live WMCO development or debugging, not only OTE.
---

# WMCO local debugging

Use this skill when you need a live WMCO cluster tied to a pull request: to develop operator,
controller, daemon or Windows node changes; to debug Windows Containers tests; or to tell a real
failure apart from a broken environment.

Two shells are used throughout. Keep them apart, because they speak to different clusters:

- **build-cluster shell**: holds the `oc` context that reaches the CI build cluster and the run's
  `ci-op-*` namespace. Every command marked `--context=<ctx>` belongs here.
- **debug shell**: has `KUBECONFIG` pointed at the provisioned cluster. Every `oc` command that is
  not marked `--context=<ctx>` belongs here.

## Getting a cluster

Pick one acquisition path and stay on it. The debug job provisions the operator and a Windows
node and holds them; cluster-bot provisions an empty cluster and you install WMCO yourself.
Starting both for one session leaves two environments to confuse and wastes a cluster.

### Path A - the opt-in debug job (preferred)

The job is configuration in `openshift/release`, so it exists in Prow only after that pull request
merges. Check before requesting, from a shell with `gh`:

```bash
gh api repos/openshift/release/contents/ci-operator/config/openshift/windows-machine-config-operator/openshift-windows-machine-config-operator-master__ote.yaml \
  --jq '.content' | base64 -d | grep -c 'as: azure-e2e-ote-debug'
```

`0` means the job is not in Prow: use Path B, or rehearse the `openshift/release` pull
request (`/pj-rehearse <job-name>`) for a one-off run. Do not ask an agent to follow this
section and Path B at the same time.

Otherwise comment on the WMCO pull request under test:

```
/test ote-azure-e2e-ote-debug
```

The job builds the operator and test image from that pull request's branch, so the cluster
carries the code under test. It appears under the pull request's checks as
`ci/prow/ote-azure-e2e-ote-debug`; the image build takes minutes and setup takes minutes more.
Go to Step 1 to follow it to the hold.

### Path B - cluster-bot

`cluster-bot`: `launch openshift/windows-machine-config-operator#<pr> <platform> ovn-hybrid` in
Slack gives a cluster from the pull request. The `ovn-hybrid` modifier matters: WMCO refuses a
cluster whose `HybridOverlayConfig` is absent, and the bot's default `ovn` has none. Wait until
the network operator reports hybrid overlay. The bot installs nothing, so pick one of the next
two install paths; both install the operator, so running both installs it twice.

The bot gives you a cluster and no settings for it. When the cluster is up its reply says
`Your cluster is ready, it will be shut down automatically in ~N minutes` and attaches
`cluster-bot-<timestamp>.kubeconfig`. That attachment is the CLI credential: download it, then in
the debug shell

```bash
umask 077
mv ~/Downloads/cluster-bot-*.kubeconfig /tmp/wmco-debug-kubeconfig
chmod 600 /tmp/wmco-debug-kubeconfig
export KUBECONFIG=/tmp/wmco-debug-kubeconfig
oc whoami --show-server
oc get nodes
```

The same message prints a console URL and a `kubeadmin` password. That is browser access, not a CLI
login, and it creates no context. If the attachment has scrolled out of the channel, send `auth`
there and the bot re-uploads it. The message states how long the cluster lasts, about three hours by
default, so install and test in one sitting.

Install 1 - install an operator image that already exists, using the setup test. Run this in the debug
shell, against the cluster-bot cluster. The script reads two environment variables and builds nothing:

- `OPERATOR_IMAGE`: the pullspec of an already-built WMCO operator image that this cluster can pull -
  for example the operator image CI built for the pull request, or one you built and pushed with
  Install 2. It is read only when the operator is not deployed yet: when the deploy namespace already
  holds a `windows-machine-config-operator` deployment, the script skips installation and uses that.
- `KUBE_SSH_KEY_PATH`: absolute path to the SSH private-key file used for the Windows node. The setup
  test creates the `cloud-private-key` secret in the operator and test namespaces from this file and
  uses the key to reach the instance. The script exits with `env KUBE_SSH_KEY_PATH not found` when it
  is unset, before it installs anything.

```bash
# Replace both example values before running.
export OPERATOR_IMAGE='<already-built-WMCO-image-pullspec>'
export KUBE_SSH_KEY_PATH='/absolute/path/to/private-key.pem'

hack/run-ci-e2e-test.sh -t setup-only
```

`-t setup-only` runs only `TestWMCO/create/Creation`: it installs the operator when the cluster does
not have it, creates a Windows MachineSet, waits for the node to join, and exits with
`Setup complete with Windows nodes provisioned. Skipping remaining e2e tests (-t setup-only).`

Install 2 - build the operator from the checkout, then install it. `hack/olm.sh build` builds
`build/Dockerfile` and pushes the result to `OPERATOR_IMAGE`; `hack/olm.sh run` writes the image
`OPERATOR_IMAGE` holds at that moment into the CSV and installs it. The push target has to be
reachable from the workstation, accept your push, and be pullable by the cluster.

A registry namespace you control is the simplest target. Log in to that registry first, replace
`<your-namespace>`, and use a fresh tag per iteration if the namespace is shared:

```bash
export OPERATOR_IMAGE='quay.io/<your-namespace>/wmco:local-debug'
hack/olm.sh build
hack/olm.sh run -k '/absolute/path/to/private-key.pem'
```

The target cluster's integrated registry also works and avoids a personal registry and a pull-secret
edit. It needs a running registry with an externally reachable `default-route`, an image project you
can push to, and pull permission for the operator's service account. Check those instead of assuming
them, and do not enable or reconfigure the registry on a cluster you are debugging:

```bash
IMAGE_PROJECT=openshift-windows-machine-config-operator
TAG=local-debug
HOST=$(oc -n openshift-image-registry get route default-route -o jsonpath='{.spec.host}')

# Log in once, with credentials allowed to push to that project.
podman login "$HOST"

oc get ns "$IMAGE_PROJECT" >/dev/null || oc create ns "$IMAGE_PROJECT"
oc -n "$IMAGE_PROJECT" get is wmco >/dev/null || oc -n "$IMAGE_PROJECT" create imagestream wmco

# The workstation pushes through the route.
OPERATOR_IMAGE="$HOST/$IMAGE_PROJECT/wmco:$TAG" hack/olm.sh build

# The operator pulls the same image through the internal service, so install with that reference.
export OPERATOR_IMAGE="image-registry.openshift-image-registry.svc:5000/$IMAGE_PROJECT/wmco:$TAG"
hack/olm.sh run -k '/absolute/path/to/private-key.pem'
```

The two references differ on purpose: `build_WMCO()` pushes its `OPERATOR_IMAGE` unchanged, while
`run_WMCO()` substitutes the value current at that moment into the CSV, so only the run step needs the
internal pullspec. A `port-forward` to `svc/image-registry` replaces the route but not the login;
`hack/olm.sh build` pushes without TLS flags, so that variant needs a manual `podman build` and
`podman push --tls-verify=false`, followed by the same run step with the internal pullspec.

`hack/olm.sh run -k` creates the `cloud-private-key` secret from that file when it does not exist yet.
`hack/olm.sh` reads the image from `OPERATOR_IMAGE`; its usage line still shows `-c`, but that option
has no handler and fails with `Unknown option`. Create a Windows MachineSet with `hack/machineset.sh`.
Steps 3 and 4 apply to both paths. Steps 1, 2 and 5 do not: they belong to Path A, because a
cluster-bot cluster has no run, no setup pod and no hold.

## Who owns what

The debug job is shared, every run is individual.

| Layer | Ownership | Consequence |
|---|---|---|
| Job definition | team, in `openshift/release` | merged once, then anyone can request it on any pull request of the repository |
| Trigger | per pull request, `/test ote-azure-e2e-ote-debug` | builds the operator and test image from that pull request |
| Run | per trigger, disposable | own `ci-op-*` namespace, own cluster, own cloud quota |
| Access to a run | build-cluster RBAC on that namespace | whoever triggers should be whoever needs the cluster |

To give a teammate a cluster, have them request the job on their own pull request instead of
transferring a run: the cluster then carries their code and the namespace access follows them. Two
people debugging at once means two runs and two clusters. The job definition is changed once by
whoever owns the configuration, not per developer.

## Job reference: one concrete debug job (Path A)

`azure-e2e-ote-debug` is the one the repository defines; the job exists in Prow only once its
`openshift/release` pull request merges, so check availability as shown in Path A before relying on it.
Its shape is what another variant copies; the platform-specific values are the workflow string and the
cluster profile in the configuration file.

```
test: azure-e2e-ote-debug            workflow: ipi-azure-ovn-hybrid
cluster: build13                     cluster profile: openshift-org-azure
step: wmco-setup-and-test            setup: hack/run-ci-e2e-test.sh -t setup-only -s
hold: only when PULL_NUMBER is set, released by /tmp/debug.done or TIMEOUT (+4 hours)
step timeout: 6h45m                  job timeout: 20h
suite: windows-machine-config-operator/all   TEST_SKIPS: \[node-proxy\]\|\[tls-adherence\]
always_run: false  optional: true    context: ci/prow/ote-azure-e2e-ote-debug
```

The hold lives in the setup pod rather than the shared `wait` step. On the first rehearsal the hold
pod was created with a required pod anti-affinity against pods labelled
`pod-scaler.openshift.io/measured=false`, every eligible node already ran such a pod from another job,
and it stayed `Pending` for 97 minutes. The pod-scaler adds that constraint to the pods it samples, so
it belongs to a newly created pod rather than to the step name; the setup pod is already scheduled and
holding there adds no new scheduling decision.

## Prerequisites

- Two logins, not one: the build cluster that runs the job, covered below, and the provisioned
  cluster, whose kubeconfig the bot attaches in Path B. Keep each in its own file with
  `export KUBECONFIG`, not `~/.kube/config`, so the two shells cannot swap targets.
- A Windows SSH key pair of your own, with the private key on the workstation: both install paths put
  it in the `cloud-private-key` secret, and the Windows node authorises its public half.
- The pull request pushed: the job builds from the branch it runs on, not from your workstation, and
  cluster-bot builds the platform from that branch.
- The cluster is disposable; the job deprovisions it when it finishes, and the bot deletes it at the
  lifetime it reports.

### Log in to the build cluster

Take the cluster id from the job's `cluster:` field and resolve it through `hack/config-kubeconfig.sh`
in `openshift/release`; never build the API host from the platform name, because the two do not follow
one pattern. Every entry has the shape
`[<cluster-id>]='<auth-method> ; <context-name> ; <api-url>'`. Verify the context before Step 1,
because everything below depends on it:

```bash
# 1. cluster id -> API URL, the entry the job's `cluster:` field names
grep -n "\[<cluster-id>\]" <release-checkout>/hack/config-kubeconfig.sh
#    [<cluster-id>]='<auth-method> ; <context-name> ; https://<api-host>:6443'
api=https://<api-host>:6443

# 2. pick a stored context by its server, not by its name: several contexts can point at the same
#    build cluster, and their names are not derived from the cluster id in one consistent way
ctx=$(oc config get-contexts -o name | while read -r c; do
  [ "$(oc --context="$c" whoami --show-server)" = "$api" ] && { echo "$c"; break; }
done)
[ -n "$ctx" ] || echo "no stored context points at $api"

# 3. reuse it while its token is valid; "Unauthorized" means the token expired
oc --context="$ctx" whoami
```

When step 2 prints no context, or step 3 prints `Unauthorized`, log in to a file you name explicitly:

```bash
export KUBECONFIG=~/.kube/build-cluster.kubeconfig
oc login --web --server="$api"
ctx=$(oc config current-context)
oc --context="$ctx" whoami --show-server     # must print $api
```

`Unauthorized` after a successful handshake is an expired token, not a permission problem, and it can
surface days after the last use. `oc login` writes to the file named by `KUBECONFIG`, so naming the
file first is what keeps the build cluster apart from the provisioned cluster's kubeconfig. The login
refreshes the context it authenticates instead of adding another, and it prints the OAuth URL and
opens the browser; with an existing SSO session it completes without interaction, so it can be part of
a scripted session. The SSO token helper in `openshift/release` is the alternative when no browser is
available.

## Step 1 - find the held run (Path A only)

A cluster-bot cluster has no run to find, no `ci-op-*` namespace and no setup pod. Skip to the
verification block at the end of Step 2, then continue from Step 3.

Prow does not publish the run's namespace, so derive it from the build cluster in the build-cluster
shell:

```bash
oc config get-contexts -o name | grep build
oc --context=<ctx> projects | grep -i <job-name-fragment>
#   ci-op-wwlqhj88  ...  rehearse-88887-...-ote-azure-e2e-ote-debug

oc --context=<ctx> -n <ci-op-namespace> get pods
```

Before the hold, the same container runs the WMCO e2e setup and prints `Running tests with
arguments: ...`; that is the setup suite, not the job's suite, and it takes minutes to provision the
Windows node. The hold is active when the setup pod (`wmco-setup-and-test`) is `Running` with all
containers ready and its log prints
`Debug hold: touch /tmp/debug.done in this pod to release it early`.

Wait for that line instead of guessing. The loop below is bounded: it returns 0 only when the hold
line appears, and exits non-zero with a diagnosis when the pod is gone (2), when the `test` container
has terminated or the pod ended before the hold (3), or when the deadline passes (4). `HOLD_TIMEOUT`
is in seconds and defaults to 90 minutes, because setup alone takes 30 to 45.

```bash
hold_timeout=${HOLD_TIMEOUT:-5400}
deadline=$(( $(date +%s) + hold_timeout ))
while true; do
  state=$(oc --context=<ctx> -n <ns> get pod <setup-pod> -o jsonpath='{.status.phase}{"|"}{.status.containerStatuses[?(@.name=="test")].state.terminated.reason}' 2>&1) || {
    echo "cannot read the setup pod, treat the run as failed: $state"; exit 2; }
  phase=${state%%|*}
  term=${state#*|}
  [ -n "$phase" ] || phase=Unknown   # pod created, status not populated yet
  if oc --context=<ctx> -n <ns> logs <setup-pod> -c test --tail=200 2>/dev/null \
    | grep -q 'Debug hold'; then
    echo "hold reached"; exit 0
  fi
  if [ -n "$term" ]; then
    echo "test container exited before the hold: $term (phase $phase)"; exit 3
  fi
  case "$phase" in
    Failed|Succeeded) echo "setup ended before the hold: $phase"; exit 3 ;;
  esac
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "no hold after ${hold_timeout}s (phase $phase)"
    oc --context=<ctx> -n <ns> logs <setup-pod> -c test --tail=20
    exit 4
  fi
  sleep 60
done
```

This job holds inside the setup pod rather than creating a separate `ref: wait` pod. Identify a
pod's job and step from its metadata before attributing a scheduling failure to another run.
Pod-scaler can add required anti-affinity to measured pods regardless of the step name, so
`didn't match pod anti-affinity rules` alone does not distinguish this job from another variant.
Inspect the affected pod's events, measured label and affinity; do not delete other jobs' pods.

On exit 2, 3 or 4 the run is unusable: keep the failing step and the `wmco-setup-and-test` log tail,
and start a new run once the cause recorded below is addressed.

## Step 2 - get the cluster kubeconfig (Path A only)

On a cluster-bot cluster this step is already done: the kubeconfig is the attachment from Path B. The
checks at the end of the step still apply, and there they run in the debug shell.

Do this from the build-cluster shell, write the file with restrictive permissions, and never print it.
Both ways produce the same kubeconfig.

Preferred - copy it out of the setup pod:

```bash
umask 077
oc --context=<ctx> -n <ns> exec <setup-pod> -c test -- \
  cat /var/run/secrets/ci.openshift.io/multi-stage/kubeconfig > /tmp/wmco-debug-kubeconfig
chmod 600 /tmp/wmco-debug-kubeconfig
```

Alternative - the job also stores the same kubeconfig in a secret named after the job:

```bash
umask 077
oc --context=<ctx> -n <ns> get secret azure-e2e-ote-debug -o jsonpath='{.data.kubeconfig}' \
  | base64 --decode > /tmp/wmco-debug-kubeconfig
chmod 600 /tmp/wmco-debug-kubeconfig
```

Before trusting any result, confirm the environment in the debug shell:

```bash
export KUBECONFIG=/tmp/wmco-debug-kubeconfig
oc whoami --show-server
oc get nodes -l kubernetes.io/os=windows -L node.kubernetes.io/windows-build
oc -n openshift-windows-machine-config-operator get csv
oc get csr | grep -v Approved
```

On Path A the cluster is often reachable only from the build farm, and then `oc get nodes` times out
from the workstation. Run the same checks in the hold pod, which has the kubeconfig, the SSH key and
an `oc` binary:

```bash
oc --context=<ctx> -n <ns> exec <setup-pod> -c test -- bash -c '
  export KUBECONFIG=/var/run/secrets/ci.openshift.io/multi-stage/kubeconfig
  /cli/oc whoami --show-server
  /cli/oc get nodes -l kubernetes.io/os=windows -L node.kubernetes.io/windows-build
  /cli/oc -n openshift-windows-machine-config-operator get csv
  /cli/oc get csr | grep -v Approved'
```

`oc` is not on `PATH` in the step container; it is `/cli/oc`. The SSH key is
`${CLUSTER_PROFILE_DIR}/ssh-privatekey` inside the same pod, which is also the right
`--private-key-path` for a legacy `go test` suite.

The Windows node is `Ready`, WMCO's CSV is `Succeeded`, and no `node-bootstrapper` CSR is unapproved.
Do not export this `KUBECONFIG` in the build-cluster shell: that shell needs its own contexts for the
`--context=<ctx>` commands in Step 4 and Step 5.

## Step 3 - develop against the cluster

All commands here run in the debug shell. On Path A the operator from the pull request is already
installed. On Path B you installed it, and until you compare the running image with the one you built
you do not know whose code the cluster is running:

```bash
oc -n openshift-windows-machine-config-operator get deploy windows-machine-config-operator \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
```

That must be the image you pushed. For a change you are still iterating on, build and push a new
image and point the deployment at it:

```bash
oc -n openshift-windows-machine-config-operator logs deploy/windows-machine-config-operator -f
oc -n openshift-windows-machine-config-operator set image \
  deployment/windows-machine-config-operator '*=<registry>/<image>:<tag>'
oc -n openshift-windows-machine-config-operator rollout status deploy/windows-machine-config-operator
```

Useful handles on this environment:

- Windows node access: the SSH key is the `private-key.pem` key of the `cloud-private-key` secret,
  the node is the Windows node's `InternalIP`, and the SSH user is the platform's existing mapping:
  `capi` on Azure, `Administrator` on vSphere and every other platform. WMCO applies that mapping in
  `getDefaultUsername()` in `controllers/windowsmachine_controller.go`, and `test/e2e/main_test.go`
  uses the same one, so use those instead of guessing a username.
- Service definitions: the per-version services ConfigMaps are named `windows-services-<version>` and
  carry no services label, so list the names first and read the current one:
  `oc -n openshift-windows-machine-config-operator get cm --no-headers | grep windows-services-`.
- Reconfiguration: changing the `version` and `desired-version` node annotations drains and
  reconfigures one node at a time; expect minutes, not seconds.
- Private key rotation is not a reconfiguration. Replacing `cloud-private-key` makes WMCO destroy and
  recreate Machine-backed Windows instances one at a time, and BYOH nodes need the new public key in
  `authorized_keys` before WMCO can reach them again. Do not rotate the key to force a restart on the
  cluster you are debugging.
- Repository scripts: `hack/olm.sh`, `hack/machineset.sh`, `hack/run-ci-e2e-test.sh`, and
  `docs/HACKING.md` describe the same steps for a cluster you own.

## Step 4 - build, unit test, and run tests

Build from the repository root: `build-tests-ext` is a root Makefile target that writes to the root
`build/` directory, and the native and Linux binaries must not overwrite each other.

```bash
# native binary for the workstation
make build-tests-ext
# build/_output/bin/wmco-tests-ext

# Linux binary for the hold pod, kept in a separate output directory
mkdir -p build/_output-linux/bin
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 make build-tests-ext OUTPUT_DIR=build/_output-linux
# build/_output-linux/bin/wmco-tests-ext
```

The extension is an Origin compatibility harness: with `KUBECONFIG` unset it exits before any test
runs. `go test` then still prints `ok` while running nothing, and the built binary fails with
`Please set KUBECONFIG first!`. Any value works, `/dev/null` is enough, and `list tests` needs it too.

```bash
(cd ote && KUBECONFIG=/dev/null GOWORK=off go test ./test/e2e -count=1)
(cd ote && GOWORK=off go vet ./... && gofmt -s -l test/e2e cmd)
```

List and run tests from the repository root:

```bash
KUBECONFIG=/dev/null ./build/_output/bin/wmco-tests-ext list tests -o names | grep <OCP-ID>

# from the workstation, in the debug shell
./build/_output/bin/wmco-tests-ext run-test -n "<full test name>" --timeout 45m
```

On Path A, run a test inside the hold pod, which is what the job does and what you want when the
cluster is only reachable from the build farm. A cluster-bot cluster has no hold pod: run from the
workstation, and skip to the `go test` recipe below if the suite is legacy. On Path A, pass the Linux
binary from the build-cluster shell:

```bash
oc --context=<ctx> -n <ns> cp build/_output-linux/bin/wmco-tests-ext <setup-pod>:/tmp/wmco-tests-ext -c test

oc --context=<ctx> -n <ns> exec <setup-pod> -c test -- bash -c \
  'chmod 700 /tmp/wmco-tests-ext;
   export KUBECONFIG=/var/run/secrets/ci.openshift.io/multi-stage/kubeconfig;
   /tmp/wmco-tests-ext run-test -n "<full test name>" --timeout 45m'
```

The binary carries debug information and is a few hundred MB, so the copy takes a while; copy once and
reuse it. The containers of the setup pod are listed by
`oc --context=<ctx> -n <ns> get pod <setup-pod> -o jsonpath='{.spec.containers[*].name}'` if `test`
is not the one holding `SHARED_DIR`.

Read the JSON tail: `"result": "passed"` means the assertions ran, `"skipped"` carries the reason,
`"failed"` carries the Ginkgo failure line. A failure before the first `STEP:` is usually the
environment, not the test.

The legacy suites that have not moved to the OTE module still run through `go test`, and the same
hold-pod rule applies. Build a Linux test binary once, copy it in, and start it detached, because a
suite that takes 20 to 40 minutes outlives one `oc exec`:

```bash
# workstation
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOWORK=off go test -c -o /tmp/e2e.test ./test/e2e
oc --context=<ctx> -n <ns> cp /tmp/e2e.test <setup-pod>:/tmp/e2e.test -c test

# start in the pod, then follow the log from a separate exec
oc --context=<ctx> -n <ns> exec <setup-pod> -c test -- bash -c '
  chmod 700 /tmp/e2e.test
  nohup env KUBECONFIG=/var/run/secrets/ci.openshift.io/multi-stage/kubeconfig \
    /tmp/e2e.test -test.v -test.run TestWMCO/network -test.timeout=40m \
    --private-key-path="${CLUSTER_PROFILE_DIR}/ssh-privatekey" \
    --wmco-namespace=openshift-windows-machine-config-operator \
    --windows-server-version=2025 > /tmp/network-test.log 2>&1 &
  sleep 10; tail -3 /tmp/network-test.log'
```

`KUBECONFIG` is passed explicitly because the step container does not export it; `ARTIFACT_DIR` is
already set there, and `${CLUSTER_PROFILE_DIR}/ssh-privatekey` is both the pod-side key and the right
`--private-key-path`.

Where the cluster does answer from the workstation, run the same suite there instead. There
`ARTIFACT_DIR` has to be set or test-context initialization fails:

```bash
export KUBECONFIG=/tmp/wmco-debug-kubeconfig
export ARTIFACT_DIR=/tmp/wmco-artifacts
go test ./test/e2e -run 'TestWMCO/create/Metrics_validation/Windows_Exporter_configuration_validation' \
  -v -timeout=90m -args \
  --private-key-path=/path/to/private-key.pem \
  --wmco-namespace=openshift-windows-machine-config-operator \
  --windows-server-version=2022
```

Subtest names contain spaces in the source and underscores in `-run`; the binary started with
`-test.run` uses the same names. Tests that scale a MachineSet also need the provider credentials the
CI step exports, for example `AWS_SHARED_CREDENTIALS_FILE`; tests that only read an existing node do
not.

## Step 5 - release the hold (Path A only)

A cluster-bot cluster has no hold: end it with `done` in the bot's channel. The notes below are the
job's post steps and teardown.

From the build-cluster shell:

```bash
oc --context=<ctx> -n <ns> exec <setup-pod> -c test -- touch /tmp/debug.done
```

The hold ends and the job then runs its configured suite, gathers diagnostics and deprovisions the
cluster. Let it finish: cancelling skips the remaining tests while the post steps are still attempted,
so teardown is not guaranteed either way. Check the run's post steps instead of assuming it.

## Environment limits that look like failures

- A machine-backed Windows node joins only if the cluster machine approver approves its
  `node-bootstrapper` CSR; WMCO approves BYOH instances only. The WMCO e2e suite takes the approver
  down itself when it provisions BYOH instances through MachineSets: `testBYOHConfiguration()` in
  `test/e2e/create_test.go` adds a ClusterVersion override with `Unmanaged: true` for
  `Deployment/machine-approver` and scales it to zero so that it cannot approve the BYOH CSRs. A
  cluster left in that state brings up no new machine-backed node: the Machine stays `Provisioned`,
  no node joins, and the MachineSet never reports a ready replica. Read the override before touching
  the deployment with `oc get clusterversion version -o jsonpath='{.spec.overrides}'`; when it is
  there a BYOH test set it, and re-enabling the approver during that phase defeats its prerequisite,
  so restore it only afterwards or let the suite do it. With no override and no replica,
  `oc -n openshift-cluster-machine-approver scale deploy machine-approver --replicas=1` brings it
  back. In the WINC-1975 test work (#4810) the multi-OS test checks for a ready approver before
  creating its MachineSet and fails at once with `the machine approver ... has no ready replica`; the
  tests on master wait out the MachineSet timeout instead. A hold inside the setup step inherits
  whatever that step left, override included, so restore the approver before releasing the hold when
  the suite that runs afterwards creates Machines.
- A second WMCO installation competes for the operator pod's host port. The CSV runs the operator
  with `hostNetwork: true` on control-plane nodes and requests host port 9182, and the scheduler's
  NodePorts filter is per node: the new pod stays `Pending` with `didn't have free ports for the
  requested pod ports` while every eligible control-plane node still has that port taken by the first
  installation. With a single control-plane node the two always collide. Confirm which installation
  holds the port before removing anything; the two are not supported side by side. Removing an OLM
  install means deleting the Subscription before the CSV, otherwise OLM recreates the CSV, and
  waiting for the Deployment to disappear.
- An aborted multi-OS run can leave its secondary MachineSet behind. It keeps billing cloud quota and
  gets in the way of the next attempt, so delete the MachineSet before rerunning.
- Reinstalling the operator through OLM needs a catalog the target Subscription can see. WMCO CI
  installs with `operator-sdk run packagemanifests`, whose CatalogSource is grpc and address based in
  the operator namespace and is not visible to a Subscription elsewhere. In the WINC-1975 test work
  (#4810) the reinstall test copies that address into a CatalogSource in the target namespace so the
  package resolves there; the tests currently on master skip when the catalog is not cluster-wide.
- A test namespace can stay `Terminating` for hours when Windows workloads were removed while their
  CSI volume was already gone: the pods never terminate, so the namespace never disappears. Read the
  Pods that reference the PVC and the storage-controller events, delete the stuck Pods, and let the
  controllers finish. Clearing a `pvc-protection` finalizer is a last resort that needs a stated
  reason: only once the volume is known to be gone and no controller can clean it up.
- A fixed test namespace left behind by a crashed attempt is not a clean starting point. The helper
  on master reuses a namespace that already exists, and waits five minutes only when that namespace
  is already terminating, so a rerun can start from the previous attempt's state. The WINC-1975 test
  work (#4810) changes the helper to delete the namespace it owns and recreate it, so the behaviour
  depends on the revision of the binary you built. Deleting a leftover namespace yourself is correct
  either way.
- Log timestamps that jump by minutes mean the workstation slept and stretched `wait.Poll` wall-clock
  deadlines; re-run instead of trusting elapsed time.
- The hold occupies cloud quota until `TIMEOUT` or `/tmp/debug.done`, and the job keeps its build pod
  the whole time. Release it when the session ends.

## Adding another opt-in debug variant

Add a test block to the relevant `ci-operator/config/openshift/<repo>/...` file in
`openshift/release`:

1. `always_run: false` and `optional: true`; do not add `pipeline_run_if_changed`, or a pull request
   that does not touch that path cannot request the job.
2. Declare every test-level env var on a step in the chain; checkconfig fails with
   `parameter "X" is overridden ... but not declared in any step` otherwise.
3. Hold inside an already-scheduled step instead of adding a new one: a new pod is subject to the
   pod-scaler anti-affinity above, an existing pod is not, so a hold in the setup pod adds no new
   scheduling decision.
4. Budget the step timeout for setup plus hold, and the job timeout for everything plus gathering and
   deprovisioning.
5. Run `make update` after any edit to the configuration. It runs `ci-operator-checkconfig` and
   regenerates `ci-operator/jobs/...-presubmits.yaml`; commit the configuration and the regenerated
   job file together, or the generated-file check fails. A partial run can rewrite unrelated job
   files, so check `git status` afterwards; if prowgen dies with `cannot connect to Podman`, restart
   the container engine and re-run.
6. Rehearse with `/pj-rehearse pull-ci-openshift-<repo>-master-<job>` before merging; afterwards the
   `/test <job>` command works on any pull request of that repository.

## Traps proven on real rehearsals

| Symptom | Cause | Action |
|---|---|---|
| Newly created hold pod `Pending`, `didn't match pod anti-affinity rules` | pod-scaler anti-affinity against `pod-scaler.openshift.io/measured=false` pods already on the ci-test workers | hold in the already-scheduled setup pod; never delete other jobs' pods |
| Machine `Provisioned`, `machineset ... did not reach 1 ready replicas` | unapproved `node-bootstrapper` CSR, usually because `machine-approver` sits at 0 replicas | check `oc get csr`, the approver deployment and the ClusterVersion override; a BYOH test may have taken it down on purpose |
| Reinstall test skips with "not cluster-wide" | packagemanifests CatalogSource is namespace-local | run it on a cluster with a catalog in `openshift-marketplace` |
| Test namespace `Terminating` for hours | workloads removed while their CSI volume was gone, so Windows pods never terminate | inspect the PVC and storage events, delete the stuck pods; clear `pvc-protection` only as a justified last resort |
| Namespace of a re-run does not disappear | finalizer left by workloads removed together with their storage or network | read what is still in the namespace and its events; the test fails after five minutes rather than reusing it |
| Second WMCO install `Pending`, `didn't have free ports for the requested pod ports` | host port 9182 is taken on every eligible control-plane node by the first install | find the pod holding the port, then remove the competing Subscription and CSV; the two are not supported side by side |
| `go test` or the test binary reports success without running anything | `KUBECONFIG` unset | `KUBECONFIG=/dev/null` |
| `oc` commands hit the wrong cluster | debug kubeconfig exported in the build-cluster shell | keep the two shells separate |
| Linux binary will not run on the workstation | the Linux build overwrote the native binary | build into `OUTPUT_DIR=build/_output-linux` |

## Worked example: the shape of a Path A session

This is the debug-job order. A cluster-bot session is shorter, and it is listed at the end of this
section; a reader on Path B should follow that list rather than these steps.

The order that works, with what each stage looks like rather than what one run happened to print:

1. **Pick the run.** From the pull request's `/test` command or the rehearsal link. While the job is
   `pending` its artifact directory holds only `clone-log.txt` and `prowjob.json`; the build log is
   published when the job ends, so live work needs the build cluster, not GCS.
2. **Log in to the build cluster** that runs the job, resolving the API from the job's `cluster:` field
   rather than the platform name, and confirming `oc --context=... whoami` before anything else. An
   `Unauthorized` there is an expired token; `oc login --web` refreshes the context in place. An SSO
   token issued for one build cluster is not accepted by another.
3. **Find the namespace.** `oc projects` lists the run's `ci-op-*` project, and `oc -n <ns> get pods`
   shows the step chain and the age of each step, which tells you whether the job is provisioning or
   already holding.
4. **Read the stage from the log.** The setup step prints
   `Setup complete with Windows nodes provisioned. Skipping remaining e2e tests (-t setup-only).`
   and then `Debug hold: touch /tmp/debug.done in this pod to release it early`. Until the second
   line appears the cluster is still being provisioned.
5. **Extract the kubeconfig while setup runs.** The run secret exists early, so Step 2 can be finished
   before the hold and local tests can start the moment it appears.
6. **Check the environment before trusting a result.** A `Ready` Windows node, a `Succeeded` CSV and
   no unapproved CSRs are the baseline. Note whether the machine approver has a replica and whether
   ClusterVersion carries an `Unmanaged` override for it: without the approver no machine-backed node
   can join, and everything that scales a MachineSet fails.
7. **Run the fast cases first.** Tests that do not apply to the cluster decide to skip in seconds,
   which confirms selection before a long test starts. Do not run a test that moves the operator while
   another test is running.
8. **Read each result for what it proves.** `passed` means the assertions ran, and only counts as a
   pass if the test left nothing behind - namespace, StorageClass and the components it installed.
   `skipped` carries its reason and is a result, not a pass: a test that needs a disconnected cluster
   or a catalog the cluster does not have is telling you about the environment. `failed` before the
   first `STEP:` is almost always the environment; a later failure names the assertion or the
   MachineSet state, and the machine phases plus unapproved CSRs say whether the cluster or the test
   is at fault.
9. **Check what the test cannot see.** A multi-OS result is only meaningful if the nodes really run
   different builds: compare `oc get nodes -l kubernetes.io/os=windows -L
   node.kubernetes.io/windows-build` with `oc get machines -n openshift-machine-api`. A pass with two
   identical builds proves nothing about two variants.
10. **Release and let it finish.** `touch /tmp/debug.done`, then watch the post steps: the job runs its
    own suite, gathers diagnostics and deprovisions.

Two details that cost time when missed: `run-test` needs the full name from `list tests -o names`,
including the `[OTP][sig-windows] Windows_Containers ` prefix, and the binary must be the one built
from the worktree under test, not a stale copy in the hold pod.

On Path B the same session is five stages, and none of them involve a run:

1. **Launch and log in.** `launch openshift/windows-machine-config-operator#<pr> <platform>
   ovn-hybrid`, then download the attached `cluster-bot-*.kubeconfig` into `KUBECONFIG`. Send `auth`
   if the attachment is gone.
2. **Install your build.** `hack/olm.sh build` and `hack/olm.sh run -k <key>`, from a checkout of the
   pull request head.
3. **Create the node.** `hack/machineset.sh apply`, after WMCO exists.
4. **Verify before testing.** The deployment image is the tag you pushed, the Windows node is `Ready`,
   the CSV is `Succeeded`, and no CSR is unapproved.
5. **Run from the workstation and end the cluster.** Tests run in the debug shell; there is no hold to
   release, so `done` in the bot's channel when finished.

## Evidence rules

- Never print kubeconfigs, pull secrets, private keys or secret values; report presence and length
  only.
- Record the commit or image digest under test, the exact command, and the result line.
- Report pass, skip and infrastructure failure separately; a skip is a result, not a pass.
- Do not delete resources you do not own, and do not change node taints or other jobs' pods.
