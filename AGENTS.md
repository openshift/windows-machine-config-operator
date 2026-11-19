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

## Authoritative Documentation

Use the repository's source files and maintained documentation as the source of truth for detailed product, support, and contribution workflows:

- Product and support guidance: [README.md](README.md)
- Development setup and e2e workflows: [docs/HACKING.md](docs/HACKING.md)
- Contribution, commit, PR, and backport policy: [CONTRIBUTION.md](CONTRIBUTION.md)
- Submodule updates: run `hack/update_submodules.sh -h`

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
| ConfigMap | `configmap_controller.go` | Windows instance and service ConfigMaps, plus related Node and MachineConfig events |
| Machine | `windowsmachine_controller.go` | Windows-labeled Machine objects |
| Node | `node_controller.go` | Windows Node objects |
| Secret | `secret_controller.go` | Private key and TLS Secret changes |
| Kubelet CSR | `certificatesigningrequests_controller.go` | Pending kubelet CertificateSigningRequests |
| WICD CSR | `wicd_csr_controller.go` | Pending WICD CertificateSigningRequests |
| ControllerConfig | `controllerconfig_controller.go` | ControllerConfig changes affecting kubelet CA data |
| Registry | `registry_controller.go` | Image mirror configuration changes |
| Metrics | `metric_controller.go` | Operator namespace and ServiceMonitor changes |

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

Read the current signatures directly from their source files:

- `Windows`: [pkg/windows/windows.go](pkg/windows/windows.go)
- `Manager`: [pkg/daemon/manager/manager.go](pkg/daemon/manager/manager.go)
- `Service`: [pkg/servicescm/servicescm.go](pkg/servicescm/servicescm.go)

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

2. Add the returned service definition to the `services` slice in `GenerateManifest()`
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

- **IMPORTANT:** Prioritize changes to files that are already modified in the working tree and relevant to your task before introducing changes to clean files. This prevents proposed solutions from spreading to unrelated parts of the codebase. Clean files may still be edited when genuinely required by the task.
- Check `git status` and `git diff` to understand the current working tree state
- Preserve any pre-existing uncommitted changes that are unrelated to your task — do not overwrite, revert, stage, or commit them
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
- Checks node name against `windows-instances` ConfigMap entries: looks up instance addresses via DNS reverse resolution, then falls back to SSH hostname comparison (see `validateNodeName` in `pkg/csr/csr.go`)
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
- **Kubelet CA rotation**: The upstream ControllerConfig supplies the kubelet CA bundle (`KubeAPIServerServingCAData`), which includes rotation overlap during CA renewal. WMCO's `ControllerConfigReconciler` watches for changes and copies this bundle to `kubelet-ca.crt` on each Windows node via `UpdateKubeletClientCA` (see `controllers/controllerconfig_controller.go` and `pkg/nodeconfig/nodeconfig.go`). A separate `ca-bundle.crt` trust bundle — composed of image registry certificates and cluster-wide proxy CA data — is managed independently by `SyncTrustedCABundle`. E2e tests poll with bounded timeouts (`pkg/retry.Timeout` = 10 min). See `test/e2e/certificates_test.go:testKubeletCARotation`

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
- Git submodules (kubelet, containerd, ovn-kubernetes, etc.) are managed via `hack/update_submodules.sh`.

### Platform-Specific
- **vSphere**: Machine name max 15 chars, MachineSet name max 9
- **AWS**: Requires EC2LaunchV2 v2.0.1643+ for disconnected
- **Azure**: cloud-node-manager service required
- **GCP**: Custom hostname script in `pkg/internal/`

## Reference Documentation

Cluster operations, Windows node paths, and product limitations belong in the user-facing documentation rather than agent instructions. See [README.md](README.md) and [docs/HACKING.md](docs/HACKING.md).

Follow [CONTRIBUTION.md](CONTRIBUTION.md) for commit, PR, review, retest, and backport policy. Run `hack/update_submodules.sh -h` before changing submodule revisions.

