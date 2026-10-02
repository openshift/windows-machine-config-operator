# AGENTS.md

## Project Overview

The Windows Machine Config Operator (WMCO) configures Windows Server instances as worker nodes in OpenShift/OKD clusters, enabling Windows container workloads.

**Architecture:**
```
┌─────────────────────────────────────────────────────────────┐
│                    OpenShift Cluster                        │
├────────────────────────┬────────────────────────────────────┤
│   Linux Control Plane  │        Windows Worker Nodes        │
│  ┌──────────────────┐  │  ┌──────────────────────────────┐  │
│  │  WMCO Operator   │──┼──│  WICD (daemon)               │  │
│  │  - Controllers   │SSH│  │  - Service reconciliation    │  │
│  │  - CSR Approval  │  │  │  - Certificate rotation      │  │
│  └──────────────────┘  │  │  - Environment management    │  │
│                        │  └──────────────────────────────┘  │
│                        │  ┌──────────────────────────────┐  │
│                        │  │  Windows Components          │  │
│                        │  │  kubelet, containerd,        │  │
│                        │  │  kube-proxy, hybrid-overlay  │  │
│                        │  └──────────────────────────────┘  │
└────────────────────────┴────────────────────────────────────┘
```

**Provisioning Modes:**
- **Machine API**: Create Windows VMs via MachineSets, auto-configured by WMCO
- **BYOH**: Define existing instances in `windows-instances` ConfigMap

**Tech Stack:** Go (see `go.mod` for current version), Kubernetes Operator (controller-runtime), OpenShift APIs, Windows Server 2019/2022

---

## Product Overview

Red Hat OpenShift support for Windows Containers is a layered component of OpenShift that allows the integration of Windows Nodes for running Windows Containers on an OpenShift 4 Cluster.

This is achieved by installing the Windows Machine Config Operator (WMCO), which runs on Linux based control-plane nodes. The WMCO bootstraps Windows nodes to join the cluster as Windows worker nodes.

### Subscription Requirements

Windows Containers requires a specific subscription, in addition to an OpenShift Container Platform subscription:
- **Worker nodes only** - Control plane and infrastructure nodes don't need a paid subscription for Windows Containers
- When working cases, verify entitlement mapping to ensure compliance

### Entitlement Validation

Look for the **"for Windows"** phrase in the entitlement name.

**Example Entitlements:**
- `MW01465`: Red Hat OpenShift Container Platform, Standard (2 Cores or 4 vCPUs, for Windows)
- `MW01615`: Red Hat OpenShift Container Platform, Premium (2 Cores or 4 vCPUs, for Windows)

---

## Requesting Engineering Assistance

### Jira Projects

| Project | Purpose | Link |
|---------|---------|------|
| OpenShift Bugs (OCPBUGS) | Bug reports | [Red Hat Issue Router](https://access.redhat.com/labs/rhir/) → "Windows Containers" component |
| Windows Containers (WINC) | Engineering tracking | [issues.redhat.com/projects/WINC](https://issues.redhat.com/projects/WINC/issues/) |
| RFE | Feature requests | [issues.redhat.com/project/RFE](https://issues.redhat.com/project/RFE) → "Windows Containers" component |
| Portfolio Backlog | Roadmap/planning | [Portfolio Plan View](https://issues.redhat.com/secure/PortfolioPlanView.jspa?id=1226&sid=1226&vid=4843#plan/backlog) |

### Creating a Bug Report

Search for the **"Windows Containers"** component on the [Red Hat Issue Router](https://access.redhat.com/labs/rhir/), then create a bug with:

1. **Detailed explanation** of the issue including:
   - Troubleshooting steps already taken
   - Any recent changes to the cluster
   - Relevant contextual information
2. **Link to customer case** (if one exists)
3. **Must-gather archive** recently generated on the cluster
4. **Command outputs** (not present in older must-gather archives):

```bash
oc get network.operator cluster -o yaml
oc logs -f deployment/windows-machine-config-operator -n openshift-windows-machine-config-operator
```

5. **MachineSet object** describing Windows instances (if using Machine API/IPI)
6. **windows-instances ConfigMap** (if using BYOH method)

### Bug Report Template

Use this template when filing bugs in **OpenShift Bugs (OCPBUGS)** with **Component: Windows Containers**:

```
Description of problem:
{code:none}
    
{code}

Version-Release number of selected component (if applicable):
{code:none}
    
{code}

How reproducible:
{code:none}
    
{code}

Steps to Reproduce:
{code:none}
    1.
    2.
    3.
{code}

Actual results:
{code:none}
    
{code}

Expected results:
{code:none}
    
{code}

Additional info:
{code:none}
    
{code}
```

### Reaching Out on Slack

Slack is a **supplement** to bug reports in Jira. Only reach out if:
- A bug is already filed
- The issue hasn't received a prompt response

**Channel:** `#forum-ocp-winc` (CoreOS Slack)
**Tag:** Use only `@winc-watcher` to get the team's attention
Do NOT use `@here` or `@everyone`

---

## Setup Commands

### Build
- `make build` - Build operator binary (runs `fmt`, `vet`, then `build/build.sh`)
- `make build-daemon` - Cross-compile Windows daemon (WICD); the target sets `GOOS=windows GOARCH=amd64` automatically
- `make build-tests-ext` - Build the OTE (OpenShift Tests Extension) binary from the `ote/` nested module
- `make all` - Runs `lint`, `build`, and `unit`

### Test
- `make unit` - Run all unit tests (`hack/unit.sh` — covers `./pkg/...`, `./controllers/...`, `./cmd/...`)
- `go test -v ./pkg/nodeconfig/...` - Test specific package
- `make wicd-unit` - Cross-compile and run daemon unit tests on a Windows VM (requires `INSTANCE_ADDRESS`, `INSTANCE_USERNAME`, `KUBE_SSH_KEY_PATH`)

### Lint
- `make lint` - Run format checks and vendor verification (`hack/lint-gofmt.sh` + `hack/verify-vendor.sh`). This checks `gofmt -s`, import ordering via `goio`, and `context.TODO()` usage outside e2e tests. It does **not** use golangci-lint.
- `make imports` - Auto-fix import ordering with `goio`

### Code Generation
- `make manifests` - Generate webhook, RBAC, and CRD manifests via controller-gen (processes `webhook`, `rbac`, and `crd` markers in source)
- `make generate` - Generate DeepCopy method implementations (controller-gen). Run after adding or changing a CRD.

### Dependency Management
- `go mod tidy && go mod vendor` - Update vendored dependencies (no `make vendor` target exists). `make lint` runs `hack/verify-vendor.sh` which checks that vendor is in sync.

---

## Code Organization

### Build Tags (Critical!)
- `//go:build windows` - Windows-only code (daemon, services)
- `//go:build !windows` - Linux-only code (operator)
- Cannot cross-compile without correct GOOS - always check tags

### Controllers (Linux - `controllers/`)

| Controller | File | Watches |
|------------|------|---------|
| ConfigMap | `configmap_controller.go` | `windows-instances` ConfigMap |
| Machine | `machine_controller.go` | Machine objects with Windows OS label |
| Node | `node_controller.go` | Windows Node objects |
| Secret | `secret_controller.go` | `cloud-private-key` Secret |

### Daemon (Windows - `pkg/daemon/`)

| Package | Purpose |
|---------|---------|
| `controller/` | WICD main reconciliation loop |
| `manager/` | Windows Service Control Manager interface |
| `certs/` | Certificate import/management |
| `cleanup/` | Node deconfiguration logic |
| `envvar/` | Environment variable management |
| `fake/` | Test mocks (platform-independent) |

### Core Packages

| Package | Purpose | Platform |
|---------|---------|----------|
| `pkg/nodeconfig/` | Orchestrates node configuration via SSH | Linux |
| `pkg/windows/` | SSH connectivity, SFTP, remote commands | Linux |
| `pkg/csr/` | CSR validation and approval | Linux |
| `pkg/services/` | Windows service definitions | Linux |
| `pkg/servicescm/` | Services ConfigMap schema and parsing | Any |
| `pkg/cluster/` | Cluster network config (OVN) | Linux |
| `pkg/wiparser/` | Parse windows-instances ConfigMap | Linux |

---

## Key Interfaces

### Windows (`pkg/windows/windows.go`)
All remote operations on Windows instances.

```go
type Windows interface {
    GetIPv4Address() string
    GetHostname() (string, error)
    Run(cmd string, psCmd bool) (string, error)
    EnsureFile(*payload.CompressedFileInfo, string) error
    EnsureFileContent([]byte, string, string) error
    Bootstrap(ctx context.Context, version, namespace, kubeconfig string) error
    ConfigureWICD(namespace, kubeconfig string) error
    RebootAndReinitialize(context.Context) error
    RunWICDCleanup(namespace, kubeconfig string) error
}
```

### Manager (`pkg/daemon/manager/manager.go`)
Windows service management (Windows-only build tag).

```go
type Manager interface {
    CreateService(name, exepath string, config mgr.Config, args ...string) (Service, error)
    GetServices() (map[string]struct{}, error)
    OpenService(name string) (Service, error)
    DeleteService(name string) error
    EnsureServiceState(Service, svc.State) error
    Disconnect() error
}
```

### Service (`pkg/servicescm/servicescm.go`)
Service definition in ConfigMap.

```go
type Service struct {
    Name                   string              `json:"name"`
    Command                string              `json:"path"`
    NodeVariablesInCommand []NodeCmdArg        `json:"nodeVariablesInCommand,omitempty"`
    PowershellPreScripts   []PowershellPreScript `json:"powershellPreScripts,omitempty"`
    Dependencies           []string            `json:"dependencies,omitempty"`
    Bootstrap              bool                `json:"bootstrap"`
    Priority               uint                `json:"priority"`
}
```

### Controllers
All implement `reconcile.Reconciler`:

```go
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)
```

---

## Common Workflows

### Adding a New Windows Service
1. Define in `pkg/services/services.go`:

```go
func myServiceConfiguration() servicescm.Service {
    return servicescm.Service{
        Name:         "my-service",
        Command:      "C:\\k\\my-service.exe --flag=value",
        Dependencies: []string{"containerd"},
        Bootstrap:    false,  // true = runs during node join
        Priority:     2,      // 0 = first, higher = later
    }
}
```

2. Add to `GenerateManifest()` return slice
3. Add unit tests in `pkg/services/services_test.go`
4. Service auto-reconciled by WICD on Windows nodes

### Node Configuration Flow

```
MachineSet Created
    ↓
Machine Created → WMCO detects (Windows OS label)
    ↓
SSH to Windows instance
    ↓
Transfer files (kubelet, containerd, CNI, etc.)
    ↓
Bootstrap WICD with kubeconfig
    ↓
WICD starts services, generates kubelet CSR
    ↓
WMCO approves CSR → Node joins cluster
    ↓
WICD reconciles services continuously
```

# Project-Specific Guidelines

## Preserving Unrelated Changes

When making code changes, keep your changeset focused on the task at hand.

- Check `git status` and `git diff` to understand the current working tree state
- Preserve any pre-existing uncommitted changes that are unrelated to your task
- Avoid opportunistic refactors or unrelated cleanups in the same changeset
- This keeps PRs focused, easier to review, and simpler to revert if needed

### Version Annotations (Upgrade Flow)

```yaml
# Node annotations
windowsmachineconfig.openshift.io/version: "10.0.0"          # Current
windowsmachineconfig.openshift.io/desired-version: "10.1.0"  # Target
```

- Mismatch triggers: drain → reconfigure → uncordon
- Only one node upgraded at a time (sequential)
- Reboot annotation triggers instance restart

### Certificate Flow
1. Kubelet generates CSR during bootstrap
2. WMCO CSR controller validates:
   - Node name matches ConfigMap/Machine
   - Certificate type (client vs serving)
   - Key usages correct
3. Approved → certificate issued to kubelet
4. WICD manages rotation via certificate manager
5. Trust bundle updates require node reboot

---

## Testing

### Structure
- Test files: `*_test.go` alongside source
- Table-driven tests preferred
- Use `testify/assert` and `testify/require`

### Windows Service Mocks
`pkg/daemon/fake/` provides test doubles:

```go
// Create fake service manager with existing services
existingSvcs := map[string]*fake.FakeService{
    "kubelet": fake.NewFakeService("kubelet", mgr.Config{}, svc.Status{State: svc.Running}),
}
fakeMgr := fake.NewTestMgr(existingSvcs)

// Use in tests
err := removeServices(fakeMgr, configMapServices, false)
require.NoError(t, err)
```

### Commands

```bash
# All unit tests (covers pkg, controllers, cmd — excludes e2e and ote/)
make unit

# Specific package
go test -v ./pkg/nodeconfig/...

# Specific test
go test -v ./pkg/csr/... -run TestApprove

# Race detector
go test -race ./pkg/...

# Coverage
go test -cover ./pkg/... -coverprofile=coverage.out
go tool cover -html=coverage.out

# Windows daemon unit tests (requires Windows VM access)
make wicd-unit
```

### OTE (OpenShift Tests Extension)
The `ote/` directory is a **separate Go module** (`ote/go.mod`) with its own Go version and dependency tree. It contains the WMCO e2e test extension binary. `make unit` does not cover tests in `ote/`; run `cd ote && go test ./...` separately if needed.

```bash
# Build the OTE binary
make build-tests-ext
```

The OTE module depends on `openshift-eng/openshift-tests-extension` and `openshift/origin`. It has its own `replace` directives and dependency versions, managed separately from the root module's `go.mod`.

### E2E Tests
- Located in `test/e2e/`
- Require running cluster with Windows nodes
- See `docs/HACKING.md` for setup

### CI System
WMCO uses both **Prow** (via `.ci-operator.yaml` and configs in `openshift/release`) and **Tekton** (via `.tekton/` pipeline definitions) for CI. Prow job results are browsable at [prow.ci.openshift.org](https://prow.ci.openshift.org). Use the job name and build ID from CI status checks to navigate to logs and artifacts.

---

## Code Style

### Go Standards
- `gofmt -s` for formatting
- `make lint` before committing (runs `gofmt -s`, `goio` import checks, `context.TODO()` checks, and vendor verification — see `hack/lint-gofmt.sh` and `hack/verify-vendor.sh`)

### WMCO Conventions

**Logging** - Use `logr.Logger`:

```go
log.Info("configuring node", "name", nodeName, "address", addr)
log.Error(err, "failed to configure node", "name", nodeName)
log.V(1).Info("debug message")  // Verbose
```

**Context** - Always first parameter:

```go
func (nc *NodeConfig) Configure(ctx context.Context) error
```

**Errors** - Wrap with context:

```go
return fmt.Errorf("failed to configure node %s: %w", nodeName, err)
```

**Kubernetes Client** - Prefer `client.Client`:

```go
// Good
func NewController(c client.Client) *Controller

// Avoid (unless needed for specific APIs)
func NewController(clientset *kubernetes.Clientset) *Controller
```

**RBAC** - Kubebuilder markers:

```go
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;patch;update
//+kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests,verbs=get;list;watch
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)
```

---

## Security Considerations

**Private Keys**
- Stored in `cloud-private-key` Secret under the `private-key.pem` key (see `pkg/secrets/secrets.go`)
- Never log key content
- Used for SSH authentication to Windows instances
- WMCO derives the SSH public key and generates a `windows-user-data` Secret for MachineSet provisioning
- **Changing the private key**: update the `cloud-private-key` Secret contents. Machine-backed Windows nodes are destroyed and recreated one at a time with the new key. BYOH instances require manual `authorized_keys` update — WMCO cannot access BYOH nodes until the new public key is authorized (see `README.md` § "Changing the private key secret"; behavior verified by `test/e2e/secrets_test.go:testPrivateKeyChange`)

**CSR Approval**
- Validates node identity before approval (see `pkg/csr/csr.go`)
- Checks node name against windows-instances ConfigMap or Machine objects
- Distinguishes kubelet client vs serving certificate types via separate validators
- Validates certificate type and key usages (see `pkg/csr/validation/`)

**Credentials in Annotations**
- Username encrypted with PGP (`pkg/crypto/`)
- Public key hash stored for verification
- Never store plaintext secrets

**Certificates**
- Auto-rotate before expiry (80% lifetime)
- Trust bundle changes require reboot
- WICD certificate separate from kubelet
- **Kubelet CA rotation**: WMCO detects new kubelet CA certificates (rotated by the kube-apiserver operator at ~80% of the 1-year CA lifetime), merges them into the trust bundle, and distributes to all Windows nodes. E2e tests poll with bounded timeouts (`pkg/retry.Timeout` = 10 min). See `test/e2e/certificates_test.go:testKubeletCARotation`

---

## Gotchas

### Windows-Specific
- **Reboots required** for environment variables and certificates
- **Service dependencies** must be in Priority order
- **File paths**: backslash in PowerShell (`C:\k\`), forward in Go paths
- **SSH sessions** must be closed or connections leak
- **PowerShell default shell**: commands auto-wrapped if needed

### Kubernetes
- `version` vs `desired-version` are different annotations with different purposes
- ConfigMap changes trigger immediate reconciliation
- Only one node upgraded at a time (prevents cluster disruption)
- Node drain before reconfiguration (workloads rescheduled)

### Build & Development
- `make build-daemon` cross-compiles for Windows automatically (`GOOS=windows GOARCH=amd64`); manual `go build` of daemon code requires setting `GOOS=windows`
- Run `go mod tidy && go mod vendor` after any `go.mod` changes; verify with `hack/verify-vendor.sh`
- Run `make generate` after adding or changing CRD types; run `make manifests` after changing webhook, RBAC, or CRD markers
- The `ote/` directory is a **separate Go module** (`ote/go.mod`) with its own Go version, `replace` directives, and dependencies. Use `make build-tests-ext` to build it and `cd ote && go test ./...` to run its tests. Root-module dependency commands (`go mod tidy`, `go mod vendor`) do not apply to `ote/`.
- Git submodules (kubelet, containerd, ovn-kubernetes, etc.) are managed via `hack/update_submodules.sh` — see [Submodule Maintenance](#submodule-maintenance).

### Platform-Specific
- **vSphere**: Machine name max 15 chars, MachineSet name max 9
- **AWS**: Requires EC2LaunchV2 v2.0.1643+ for disconnected
- **Azure**: cloud-node-manager service required
- **GCP**: Custom hostname script in `pkg/internal/`

---

## Commands Reference

### Build & Test

```bash
make build                          # Operator binary (fmt + vet + build/build.sh)
make build-daemon                   # Windows daemon (WICD) — cross-compiles automatically
make build-tests-ext                # OTE binary from ote/ nested module
make unit                           # Unit tests (pkg, controllers, cmd)
make lint                           # Format checks + vendor verification
make imports                        # Auto-fix import ordering (goio)
make manifests                      # Generate webhook/RBAC/CRD manifests (controller-gen)
make generate                       # Generate DeepCopy methods (controller-gen)
make all                            # lint + build + unit
```

### Cluster Operations

```bash
# Generate MachineSet for platform
./hack/machineset.sh
./hack/machineset.sh apply

# Watch Windows nodes
oc get nodes -l kubernetes.io/os=windows -w

# Operator logs
oc logs -n openshift-windows-machine-config-operator \
  deployment/windows-machine-config-operator -f

# Check services ConfigMap
oc get cm -n openshift-windows-machine-config-operator \
  -l windowsmachineconfig.openshift.io/services

# CSR debugging
oc get csr | grep system:node
oc describe csr <name>
oc adm certificate approve <name>  # Manual approval if needed
```

### Windows Node (SSH)

```powershell
# WICD status and logs
Get-Service windows-instance-config-daemon
Get-Content C:\k\logs\wicd.log -Tail 50 -Wait

# Kubelet status and logs
Get-Service kubelet
Get-Content C:\k\logs\kubelet.log -Tail 50

# All WMCO-managed services
Get-Service | Where-Object {$_.Description -like "*OpenShift Managed*"}

# Container runtime
Get-Service containerd
ctr -n k8s.io containers list

# Network
Get-HnsNetwork
Get-HnsEndpoint
```

---

## Windows Node Paths

| Path | Purpose |
|------|---------|
| `C:\k\` | Main directory (binaries, configs) |
| `C:\k\logs\` | Service logs (kubelet, wicd, containerd) |
| `C:\k\cni\` | CNI plugins and configs |
| `C:\k\cni\config\` | CNI configuration files |
| `C:\k\containerd\` | Containerd config and state |
| `C:\k\kubeconfig` | Kubelet kubeconfig |
| `C:\k\wicd-kubeconfig` | WICD service account token |
| `C:\k\kubelet.conf` | Kubelet configuration |
| `C:\k\ca.crt` | Cluster CA certificate |
| `C:\var\lib\kubelet\` | Kubelet working directory |
| `C:\var\lib\kubelet\pki\` | Kubelet certificates |

---

## Limitations

**Unsupported Features:**
- DeploymentConfigs (use Deployments instead)
- Vertical Pod Autoscaling for Windows workloads
- OpenShift Builds, Pipelines, Service Mesh
- Trunk port networking (access port only)

**Requires Manual Setup:**
- Windows CSI drivers (not deployed by WMCO)
- Custom storage classes for Windows

**Network:**
- OVN-Kubernetes hybrid networking only
- OpenShiftSDN not supported

---

## Contributing

See [CONTRIBUTION.md](CONTRIBUTION.md) for full details.

### Commit Message Format

```
[subsystem] <what changed>
<BLANK LINE>
<why this change was made>
<BLANK LINE>
<Footer>
```

**Example:**

```
[nodeconfig] Add custom DNS configuration support

The node configuration did not support custom DNS settings for Windows
nodes. This adds the ability to specify DNS servers during bootstrap.

Follow-up to Id5e7cbb1.
```

- Subject: max 50 characters
- Body: max 80 characters per line
- Subsystem examples: `docs`, `nodeconfig`, `csr`, `daemon/controller`, `services`

### PR Title Format

```
WINC|OCPBUGS-<number>: [<subsystem>] <title>
```

**Examples:**
- `WINC-959: [docs] reorganizes readme`
- `OCPBUGS-1234: [csr] Fix validation for serving certificates`
- `[nodeconfig] Add custom DNS support` (if no Jira issue or not a Red Hat employee)

Note: the PR title prefix links to Jira tracking. The **commit message** subject uses `[subsystem]` only (no Jira prefix) — see [Commit Message Format](#commit-message-format) above.

### Before Opening a PR

```bash
# Required checks
make lint                    # Format checks + vendor verification
make imports                 # Fix import ordering
make unit                    # Unit tests
make build                   # Verify operator builds
```

**Checklist:**
- Fetched and rebased against upstream master
- Linted with `make lint`
- Fixed imports with `make imports`
- Unit tests pass (`make unit`)
- Error messages are single line
- Documentation updated if user-facing change
- `go mod tidy && go mod vendor` if dependencies changed
- `make generate` if CRD types changed; `make manifests` if webhook, RBAC, or CRD markers changed

### PR Workflow

1. **Open as Draft** - Always open PRs as drafts first to prevent CI tests from running immediately
2. **Get Reviews** - Need at least one `/lgtm` and one `/approve`
3. **Mark Ready** - Click "Ready for review" to trigger CI tests
4. **Auto-Merge** - PR merges automatically when CI tests pass

### Handling Test Failures

If a test fails due to a flake, retest with this format:

```
/retest-required

<explanation of the error>
<reason for retest>
<prow.ci.openshift.org link>

<log snippet of the failure>
```

### Backports

Use the cherry-pick robot for backports to [supported versions](https://access.redhat.com/support/policy/updates/openshift#windows):

```
/cherry-pick <release>
```

For multiple versions, chain the backports (master → 4.11 → 4.10 → 4.9) to preserve Jira associations.

If cherry-pick bot fails, create manual PR and run:

```
/jira cherry-pick OCPBUGS-<number>
```

### Reporting Issues

Open a [GitHub issue](https://github.com/openshift/windows-machine-config-operator/issues) for bugs or documentation problems.

---

## Submodule Maintenance

WMCO includes Git submodules for components that are built into the operator image. These are defined in `.gitmodules`:

| Submodule | Upstream | Purpose |
|-----------|----------|---------|
| `kubelet` | openshift/kubernetes | Windows kubelet and kube-proxy binaries |
| `containerd` | openshift/containerd | Windows container runtime |
| `ovn-kubernetes` | openshift/ovn-kubernetes | OVN-Kubernetes hybrid networking |
| `containernetworking-plugins` | openshift/containernetworking-plugins | CNI plugins |
| `hcsshim` | openshift/hcsshim | Windows container shim |
| `cloud-provider-aws` | openshift/cloud-provider-aws | AWS cloud provider |
| `cloud-provider-azure` | openshift/cloud-provider-azure | Azure cloud provider |
| `csi-proxy` | openshift/csi-proxy | Windows CSI proxy |
| `windows_exporter` | openshift/prometheus-community-windows\_exporter | Prometheus metrics |
| `promu` | openshift/prometheus-promu | Prometheus build tool |

### Updating Submodules

Use `hack/update_submodules.sh` to update submodule HEADs to their remote branch tips. The positional argument is the base branch to create a new working branch from. Options: `-m` selects specific submodules (space-separated, quoted); `-r` overrides a single submodule's remote tracking branch (requires `-m` with exactly one module). See `hack/update_submodules.sh -h` for full usage.

```bash
# Update all submodules, branching from master
hack/update_submodules.sh master

# Update only kubelet and containerd
hack/update_submodules.sh -m "kubelet containerd" master

# Point kubelet at a different remote branch (e.g. a release branch listed in .gitmodules)
hack/update_submodules.sh -m kubelet -r <remote-branch> master
```

The script creates a new local branch with commits for each submodule update. For `kubelet` and `containerd`, it also generates a Makefile version commit (updating `*_GIT_VERSION` variables). For `kubelet` (unless `-r` is used), it additionally updates Go/OpenShift library dependencies and runs `go mod vendor`.

After creating the branch, push to your fork and open a PR per the usual workflow.

---

## Resources

### Documentation
- [Enhancement Proposal](https://github.com/openshift/enhancements/blob/master/enhancements/windows-containers/windows-machine-config-operator.md)
- [Development Guide](docs/HACKING.md)
- [Prerequisites](docs/wmco-prerequisites.md)
- [BYOH Requirements](docs/byoh-instance-pre-requisites.md)

### OpenShift Docs
- [Windows Containers Guide](https://docs.openshift.com/container-platform/latest/windows_containers/)
- [Windows Node Upgrades](https://docs.redhat.com/en/documentation/openshift_container_platform/latest/html/windows_container_support_for_openshift/windows-node-upgrades)

### Platform-Specific
- [AWS MachineSet](docs/machineset-aws.md)
- [Azure MachineSet](docs/machineset-azure.md)
- [GCP MachineSet](docs/machineset-gcp.md)
- [vSphere Prerequisites](docs/vsphere-prerequisites.md)
- [Nutanix Prerequisites](docs/nutanix-prerequisites.md)

