# OTE Migration Skill for WMCO

## Description

Migrate Windows Containers (WINC) tests from openshift-tests-private (OTP) into the WMCO repository's `ote/` directory using the OpenShift Tests Extension (OTE) framework.

Two modes:
1. **Bootstrap** -- one-time setup: copy shared infrastructure (utils.go, generators.go, windows_template.go, template_test.go, testdata fixtures, Describe-level setup) into ote/
2. **Migrate test** -- move a specific test by OCP ID from OTP into the bootstrapped ote/ directory

## When to Use

- **Bootstrap**: User says "bootstrap OTE", "set up OTE", "prepare OTE for migration", or this is the first migration and ote/ lacks the shared test files
- **Migrate test**: User says "migrate OCP-XXXXX", "move test XXXXX to OTE", "port test XXXXX"

## Prerequisites

- Go 1.24+
- Git with access to openshift/openshift-tests-private
- Local clone of openshift-tests-private
- WMCO repo (this repository)

Set these environment variables before running any commands (adjust paths to your local setup):

```bash
export WMCO_REPO="$(git rev-parse --show-toplevel)"  # root of this repo
export OTP_REPO="/path/to/openshift-tests-private"   # user must set this
```

## Critical Rules

- ALWAYS fetch latest upstream master/main before any work:
  ```bash
  cd $OTP_REPO && git fetch upstream && git pull upstream master
  cd $WMCO_REPO && git fetch upstream && git pull upstream master
  ```
- NEVER commit or push without explicit user confirmation
- NEVER add Co-Authored-By or AI attribution
- NEVER use emojis in any output
- Use `GONOSUMDB="*"` for all go commands (internal packages not in public sumdb)

## Repository Layout

### Source (OTP)

```
openshift-tests-private/
  test/extended/winc/         # 6 Go files, 52 test cases, ~7,925 lines
    winc.go                   # 51 Ginkgo tests -- main suite (package winc)
    storage.go                # 1 Ginkgo test -- CSI storage (package winc)
    utils.go                  # Helper functions (SSH, node mgmt, templates)
    generators.go             # Kubernetes manifest generators (pure Go, no compat_otp)
    windows_template.go       # Go template rendering (uses compat_otp.FixturePath)
    template_test.go          # Standard Go unit tests (package winc_test, NOT Ginkgo)
  test/extended/testdata/winc/ # 21 fixture files (machinesets, storage, templates)
```

### Target (WMCO ote/)

```
windows-machine-config-operator/ote/
  cmd/main.go                 # OTE entry point (already exists)
  test/e2e/                   # Migrated test files
    winc.go                   # Describe block + migrated g.It tests from OTP winc.go
    storage.go                # Describe block + migrated g.It test from OTP storage.go
    utils.go                  # Helpers (copied from OTP, no changes)
    generators.go             # Manifest generators (copied from OTP, no changes)
    windows_template.go       # Template rendering (FixturePath call updated)
    template_test.go          # Unit tests (import path updated)
    testdata/                 # Fixtures + bindata
      winc/                   # Copied from OTP testdata/winc/
      fixtures.go             # FixturePath helper
      bindata.go              # Generated (go-bindata)
    bindata.mk                # Bindata generation makefile
  go.mod                      # Module: github.com/openshift/windows-machine-config-operator/ote
  go.sum
  Makefile
  vendor/
```

## Migration Scope (WINC-1536 / WINC-1951)

| Metric | Value |
|--------|-------|
| Total test cases | 52 |
| Go source files | 6 |
| Total lines | ~7,925 |
| Testdata fixtures | 21 YAML files |
| Level0 tests | 0 |
| Cloud platforms | AWS, Azure, GCP, vSphere, Nutanix |
| compat_otp functions used | 12 |

### compat_otp Usage in OTP Tests

1. `compat_otp.NewCLIWithoutNamespace("default")` -- CLI client initialization
2. `compat_otp.GetPrivateKey()` / `compat_otp.GetPublicKey()` -- SSH keys
3. `compat_otp.FixturePath("testdata", "winc", filename)` -- fixture file paths
4. `compat_otp.GenerateManifestFile(oc, subfolder, filename, replacements)` -- manifest generation
5. `compat_otp.SetNamespacePrivileged(oc, namespace)` -- PodSecurity config
6. `compat_otp.MapiMachineset` / `compat_otp.MapiMachine` / `compat_otp.MachineAPINamespace` -- constants
7. `compat_otp.AssertWaitPollNoErr(err, msg)` -- assertion helper
8. `compat_otp.By(msg)` -- test step narration
9. `compat_otp.NewPrometheusMonitor(oc)` -- Prometheus integration

---

## Mode 1: Bootstrap

One-time setup. Creates the shared infrastructure that ALL tests depend on. Run this ONCE before migrating any individual test.

After bootstrap, the ote/ directory will have all shared files but NO g.It test blocks -- just empty Describe shells ready to receive tests.

### Phase 1: Fetch Upstream and Backup

```bash
cd $OTP_REPO && git fetch upstream && git checkout master && git pull upstream master
cd $WMCO_REPO && git fetch upstream && git checkout master && git pull upstream master
```

Back up existing ote/ content:

```bash
cd $WMCO_REPO/ote
BACKUP_DIR=$(mktemp -d)
cp -r . "$BACKUP_DIR/ote-backup"
echo "Backup at: $BACKUP_DIR/ote-backup"
```

Set variables:

```bash
OTE_DIR="$WMCO_REPO/ote"
SOURCE_REPO="$OTP_REPO"
SOURCE_TEST_PATH="$SOURCE_REPO/test/extended/winc"
SOURCE_TESTDATA_PATH="$SOURCE_REPO/test/extended/testdata/winc"
MODULE_NAME="github.com/openshift/windows-machine-config-operator/ote"
```

### Phase 2: Create Directory Structure

```bash
cd "$OTE_DIR"
mkdir -p cmd bin test/e2e/testdata/winc
```

### Phase 3: Copy Shared Files (NOT test blocks)

Copy only the helper/infrastructure files. Do NOT copy winc.go or storage.go wholesale -- those contain the g.It blocks that will be migrated one at a time.

```bash
cd "$OTE_DIR"

# Copy helpers (no test cases, just functions)
cp "$SOURCE_TEST_PATH/utils.go" test/e2e/utils.go
cp "$SOURCE_TEST_PATH/generators.go" test/e2e/generators.go
cp "$SOURCE_TEST_PATH/windows_template.go" test/e2e/windows_template.go
cp "$SOURCE_TEST_PATH/template_test.go" test/e2e/template_test.go

# Copy ALL testdata fixtures (all tests share these)
cp -rv "$SOURCE_TESTDATA_PATH"/* test/e2e/testdata/winc/

echo "Shared files copied:"
ls -la test/e2e/*.go
echo "Testdata files: $(find test/e2e/testdata -type f ! -name '*.go' | wc -l)"
```

### Phase 4: Create Empty Describe Shells

Create `test/e2e/winc.go` with the Describe block, BeforeEach setup, and shared variables -- but NO g.It test blocks. Tests will be added one at a time in Mode 2.

**IMPORTANT:** Extract the exact Describe-level code from OTP winc.go (lines 1-51) including:
- Package declaration and imports
- `g.Describe("[OTP][sig-windows] Windows_Containers", ...)` 
- `oc := compat_otp.NewCLIWithoutNamespace("default")`
- The `Service` struct
- `g.BeforeEach` that sets `iaasPlatform`, `privateKey`, `publicKey`
- Close the Describe with `})`

```go
// test/e2e/winc.go
package winc

import (
	"strings"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
)

var _ = g.Describe("[OTP][sig-windows] Windows_Containers", func() {
	defer g.GinkgoRecover()

	oc := compat_otp.NewCLIWithoutNamespace("default")

	// Struct used to define a service in the windows-services
	type Service struct {
		Name         string   `json:"name"`
		Path         string   `json:"path"`
		Bootstrap    bool     `json:"bootstrap"`
		Priority     int      `json:"priority"`
		Dependencies []string `json:"dependencies,omitempty"`
	}

	g.BeforeEach(func() {
		output, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("infrastructure", "cluster", "-o=jsonpath={.status.platformStatus.type}").Output()
		iaasPlatform = strings.ToLower(output)
		var err error
		privateKey, err = compat_otp.GetPrivateKey()
		o.Expect(err).NotTo(o.HaveOccurred())
		publicKey, err = compat_otp.GetPublicKey()
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	// --- Migrated tests go below this line (added via Mode 2) ---

})
```

Similarly create `test/e2e/storage.go` with an empty Describe shell:

```go
// test/e2e/storage.go
package winc

import (
	"strings"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
)

var _ = g.Describe("[OTP][sig-windows] Windows_Containers Storage", func() {
	defer g.GinkgoRecover()
	var oc = compat_otp.NewCLIWithoutNamespace("default")

	g.BeforeEach(func() {
		output, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("infrastructure", "cluster", "-o=jsonpath={.status.platformStatus.type}").Output()
		iaasPlatform = strings.ToLower(output)
		var err error
		privateKey, err = compat_otp.GetPrivateKey()
		o.Expect(err).NotTo(o.HaveOccurred())
		publicKey, err = compat_otp.GetPublicKey()
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	// --- Migrated tests go below this line (added via Mode 2) ---

})
```

**NOTE:** Add `[OTP]` prefix to both Describe strings. The OTP source does NOT have it yet -- we add it during migration.

### Phase 5: Fix FixturePath and Imports

```bash
cd "$OTE_DIR"

# Replace compat_otp.FixturePath with testdata.FixturePath in windows_template.go
sed 's/compat_otp\.FixturePath("testdata", /testdata.FixturePath(/g' test/e2e/windows_template.go > test/e2e/windows_template.go.tmp && mv test/e2e/windows_template.go.tmp test/e2e/windows_template.go

# Add testdata import to windows_template.go
sed '/compat_otp/a\
	"github.com/openshift/windows-machine-config-operator/ote/test/e2e/testdata"
' test/e2e/windows_template.go > test/e2e/windows_template.go.tmp && mv test/e2e/windows_template.go.tmp test/e2e/windows_template.go

# Remove compat_otp import from windows_template.go if FixturePath was its only use
# Check first:
grep -c 'compat_otp\.' test/e2e/windows_template.go
# If count is 0, remove the import line

# Fix template_test.go import path
sed 's|"github.com/openshift/openshift-tests-private/test/extended/winc"|"github.com/openshift/windows-machine-config-operator/ote/test/e2e"|g' test/e2e/template_test.go > test/e2e/template_test.go.tmp && mv test/e2e/template_test.go.tmp test/e2e/template_test.go
```

### Phase 6: Generate fixtures.go

Create `test/e2e/testdata/fixtures.go`:

```go
package testdata

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var fixtureDir string

func init() {
	var err error
	fixtureDir, err = ioutil.TempDir("", "testdata-fixtures-")
	if err != nil {
		panic(fmt.Sprintf("failed to create fixture directory: %v", err))
	}
	if err := os.Chmod(fixtureDir, 0755); err != nil {
		panic(fmt.Sprintf("failed to set fixture directory permissions: %v", err))
	}
}

func FixturePath(elem ...string) string {
	relativePath := filepath.Join(elem...)
	targetPath := filepath.Join(fixtureDir, relativePath)

	if _, err := os.Stat(targetPath); err == nil {
		return targetPath
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		panic(fmt.Sprintf("failed to create directory for %s: %v", relativePath, err))
	}

	bindataPath := relativePath
	tempDir, err := os.MkdirTemp("", "bindata-extract-")
	if err != nil {
		panic(fmt.Sprintf("failed to create temp directory: %v", err))
	}
	defer os.RemoveAll(tempDir)

	if err := RestoreAsset(tempDir, bindataPath); err != nil {
		if err := RestoreAssets(tempDir, bindataPath); err != nil {
			panic(fmt.Sprintf("failed to restore fixture %s: %v", relativePath, err))
		}
	}

	extractedPath := filepath.Join(tempDir, bindataPath)

	filepath.Walk(extractedPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			os.Chmod(path, 0755)
		} else {
			os.Chmod(path, 0644)
		}
		return nil
	})

	if err := os.Rename(extractedPath, targetPath); err != nil {
		panic(fmt.Sprintf("failed to move extracted files: %v", err))
	}

	if info, err := os.Stat(targetPath); err == nil {
		if info.IsDir() {
			os.Chmod(targetPath, 0755)
		} else {
			os.Chmod(targetPath, 0644)
		}
	}

	return targetPath
}

func CleanupFixtures() error {
	if fixtureDir != "" {
		return os.RemoveAll(fixtureDir)
	}
	return nil
}

func GetFixtureData(elem ...string) ([]byte, error) {
	relativePath := filepath.Join(elem...)
	cleanPath := relativePath
	if len(cleanPath) > 0 && cleanPath[0] == '/' {
		cleanPath = cleanPath[1:]
	}
	return Asset(cleanPath)
}

func MustGetFixtureData(elem ...string) []byte {
	data, err := GetFixtureData(elem...)
	if err != nil {
		panic(fmt.Sprintf("failed to get fixture data: %v", err))
	}
	return data
}

func FixtureExists(elem ...string) bool {
	relativePath := filepath.Join(elem...)
	cleanPath := relativePath
	if len(cleanPath) > 0 && cleanPath[0] == '/' {
		cleanPath = cleanPath[1:]
	}
	_, err := Asset(cleanPath)
	return err == nil
}

func ListFixtures() []string {
	names := AssetNames()
	fixtures := make([]string, 0, len(names))
	for _, name := range names {
		if strings.HasPrefix(name, "testdata/") {
			fixtures = append(fixtures, strings.TrimPrefix(name, "testdata/"))
		}
	}
	sort.Strings(fixtures)
	return fixtures
}
```

### Phase 7: Generate bindata.mk

Create `test/e2e/bindata.mk`:

```makefile
TESTDATA_PATH := testdata
GOPATH ?= $(shell go env GOPATH)
GO_BINDATA := $(GOPATH)/bin/go-bindata

$(GO_BINDATA):
	@echo "Installing go-bindata..."
	@GOFLAGS= go install github.com/go-bindata/go-bindata/v3/go-bindata@v3.1.3

.PHONY: update-bindata
update-bindata: $(GO_BINDATA)
	@echo "Generating bindata..."
	@mkdir -p $(TESTDATA_PATH)
	$(GO_BINDATA) -nocompress -nometadata \
		-pkg testdata -o $(TESTDATA_PATH)/bindata.go -prefix "testdata" $(TESTDATA_PATH)/...
	@gofmt -s -w $(TESTDATA_PATH)/bindata.go

.PHONY: verify-bindata
verify-bindata: update-bindata
	git diff --exit-code $(TESTDATA_PATH)/bindata.go

.PHONY: bindata
bindata: clean-bindata update-bindata

.PHONY: clean-bindata
clean-bindata:
	@rm -f $(TESTDATA_PATH)/bindata.go
```

### Phase 8: Generate/Update Makefile

Create `Makefile` in ote/:

```makefile
BINARY := bin/windows-machine-config-operator-tests-ext

.PHONY: build
build:
	@echo "Building extension binary..."
	@cd test/e2e && $(MAKE) -f bindata.mk update-bindata
	@mkdir -p bin
	CGO_ENABLED=0 GO_COMPLIANCE_POLICY="exempt_all" GOTOOLCHAIN=auto GONOSUMDB="*" go build -mod=vendor -o $(BINARY) ./cmd
	@echo "Binary built: $(BINARY)"

.PHONY: clean
clean:
	@rm -f $(BINARY)
	@cd test/e2e && $(MAKE) -f bindata.mk clean-bindata

.PHONY: help
help:
	@echo "Available targets:"
	@echo "  build  - Build extension binary"
	@echo "  clean  - Remove binaries and bindata"
```

### Phase 9: Generate/Update cmd/main.go

Use the cmd/main.go template from the existing ote/ directory. If it does not exist, create it. Key requirements:
- MUST import both `util` and `compat_otp` from origin
- MUST import `_ "github.com/openshift/windows-machine-config-operator/ote/test/e2e"` to register tests
- MUST import `_ "github.com/openshift/windows-machine-config-operator/ote/test/e2e/testdata"` for bindata
- MUST filter specs by `/test/e2e/` code location
- MUST call `compat_otp.InitTest(false)` in BeforeAll
- MUST call `util.WithCleanup(func() {})` to set testsStarted flag

See the cmd/main.go template in the existing ote/ directory. If it needs updating, reference the full template in the "cmd/main.go Template" section at the bottom of this document.

### Phase 10: Generate/Update go.mod and Vendor

```bash
cd "$OTE_DIR"

# If go.mod needs to be regenerated:
cat > go.mod << 'EOF'
module github.com/openshift/windows-machine-config-operator/ote

go 1.24.0
EOF

GOTOOLCHAIN=auto GONOSUMDB="*" go get github.com/openshift-eng/openshift-tests-extension@latest
GOTOOLCHAIN=auto GONOSUMDB="*" go get "github.com/openshift/origin@v1.5.0-alpha.3.0.20260310231025-5d3fd0545b5d"
GOTOOLCHAIN=auto GONOSUMDB="*" go get github.com/onsi/ginkgo/v2@latest
GOTOOLCHAIN=auto GONOSUMDB="*" go get github.com/onsi/gomega@latest
GOTOOLCHAIN=auto GONOSUMDB="*" go get github.com/spf13/cobra@latest
GOTOOLCHAIN=auto GONOSUMDB="*" go get github.com/tidwall/gjson@latest
GOTOOLCHAIN=auto GONOSUMDB="*" go get github.com/blang/semver/v4@latest
GOTOOLCHAIN=auto GONOSUMDB="*" go get k8s.io/component-base@latest
GOTOOLCHAIN=auto GONOSUMDB="*" go get github.com/cyphar/filepath-securejoin@v0.4.1
GOTOOLCHAIN=auto GONOSUMDB="*" go get github.com/opencontainers/runtime-spec@v1.2.0
GOTOOLCHAIN=auto GONOSUMDB="*" go get github.com/opencontainers/cgroups@v0.0.3

# Copy replace directives from OTP (excluding openshift-tests-private itself)
grep -A 1000 "^replace" "$SOURCE_REPO/go.mod" | grep -B 1000 "^)" | \
    grep -v "^replace" | grep -v "^)" | \
    grep -v "github.com/openshift/openshift-tests-private" > /tmp/replace_directives.txt

echo "" >> go.mod
echo "replace (" >> go.mod
cat /tmp/replace_directives.txt >> go.mod
echo ")" >> go.mod
rm -f /tmp/replace_directives.txt

# Tidy and vendor
GOTOOLCHAIN=auto GONOSUMDB="*" go mod tidy
GOTOOLCHAIN=auto GONOSUMDB="*" go mod vendor
```

### Phase 11: Clean Up Old Files

```bash
cd "$OTE_DIR"

# Remove old custom test code if it exists
rm -rf test/extended/
rm -rf cmd/wmco-tests-ext/
```

### Phase 12: Build and Validate Bootstrap

```bash
cd "$OTE_DIR"
make build

# Should build with 0 tests (empty Describe shells)
./bin/windows-machine-config-operator-tests-ext list
echo "Test count (should be 0 at bootstrap): $(./bin/windows-machine-config-operator-tests-ext list 2>/dev/null | wc -l)"

# Verify template_test.go passes
cd test/e2e && go test -run TestGenerate -v ./... 2>&1 | head -30
```

Bootstrap is complete. Show diff, wait for user confirmation before any git operations.

---

## Mode 2: Migrate Test by OCP ID

Use this to move a specific test from OTP into the bootstrapped ote/ directory. Repeat for each test.

### Step 0: Fetch Latest

```bash
cd $OTP_REPO && git fetch upstream && git pull upstream master
cd $WMCO_REPO && git fetch upstream && git pull upstream master
```

### Step 1: Find the Test in OTP

```bash
OCP_ID="XXXXX"  # e.g., 33612

# Find the g.It block
grep -n "g\.It.*$OCP_ID" $OTP_REPO/test/extended/winc/winc.go $OTP_REPO/test/extended/winc/storage.go
```

This tells you:
- Which file the test is in (winc.go or storage.go)
- The line number where g.It starts

### Step 2: Extract the g.It Block

Read the full g.It block from the source file. The block starts at `g.It("...<OCP_ID>..."` and ends at the matching `})` that closes it.

**Determine which target file:**
- If the test is in OTP `winc.go` -> add to `ote/test/e2e/winc.go`
- If the test is in OTP `storage.go` -> add to `ote/test/e2e/storage.go`

### Step 3: Trace Dependencies

Before copying the g.It block, check what it uses:

```bash
# List all function calls in the g.It block (excluding standard library)
# Look for calls to functions defined in utils.go, generators.go, windows_template.go
```

Common dependencies (already available from bootstrap):
- `oc` -- shared CLI client (Describe-level variable)
- `iaasPlatform`, `privateKey`, `publicKey` -- set in BeforeEach
- `svcs`, `folders`, `administratorNames` -- package-level vars in utils.go
- `getWindowsInternalIPs()`, `getSSHBastionHost()`, etc. -- utils.go functions
- `RenderWindowsWebServerTemplate()`, etc. -- windows_template.go functions
- `GenerateNamespace()`, `GeneratePVC()`, etc. -- generators.go functions
- `wmcoNamespace`, `defaultNamespace`, `windowsNodeLabel`, etc. -- package-level vars in utils.go

If the test uses a function NOT in utils.go/generators.go/windows_template.go, copy that function too.

### Step 4: Check for New Imports

Check if the g.It block uses packages not already imported in the target file:

```bash
# Check what the test block references
# Compare against existing imports in ote/test/e2e/winc.go (or storage.go)
```

Common imports that may need adding per test:
- `"context"` -- for tests using context.WithCancel
- `"encoding/json"` -- for JSON marshal/unmarshal
- `"fmt"` -- for Sprintf
- `"os"` / `"os/exec"` -- for file/command operations
- `"time"` -- for time.Duration, time.Minute
- `"k8s.io/apimachinery/pkg/util/wait"` -- for polling
- `"k8s.io/client-go/util/retry"` -- for retry logic
- `"github.com/tidwall/gjson"` -- for JSON path queries
- `"github.com/blang/semver/v4"` -- for version comparison

Add missing imports to the target file's import block.

### Step 5: Insert the g.It Block

Insert the g.It block into the target file, BEFORE the closing `})` of the Describe block.

**Find the insertion point:**
```bash
# The line with "// --- Migrated tests go below this line" or before the final })"
grep -n "Migrated tests\|^})" $WMCO_REPO/ote/test/e2e/winc.go | tail -2
```

Insert the extracted g.It block at that location.

### Step 6: Check for Test-Specific Testdata

Some tests use fixture files via `RenderWindowsWebServerTemplate`, `RenderLinuxWebServerTemplate`, `RenderHPA`, or `RenderWindowsDaemonSet`. These all call `GetFileContent()` which reads from testdata/winc/.

All 21 testdata fixtures are copied during bootstrap, so this is usually already covered. But if a NEW fixture was added to OTP after bootstrap:

```bash
# Compare testdata files
diff <(ls $OTP_REPO/test/extended/testdata/winc/) <(ls $WMCO_REPO/ote/test/e2e/testdata/winc/)
```

If new files exist in OTP, copy them and regenerate bindata:

```bash
cp $OTP_REPO/test/extended/testdata/winc/<new-file>.yaml $WMCO_REPO/ote/test/e2e/testdata/winc/
cd $WMCO_REPO/ote/test/e2e && make -f bindata.mk update-bindata
```

### Step 7: Handle g.Skip to OTE Environment Selectors (Optional)

If the g.It block contains platform-specific `g.Skip()` calls, optionally replace with OTE environment selectors. This is tracked under WINC-1778 and can be done as a follow-up.

**Pattern to replace:**
```go
// Before (OTP pattern):
if iaasPlatform == "vsphere" || iaasPlatform == "nutanix" {
    g.Skip(fmt.Sprintf("Platform %s does not support ...", iaasPlatform))
}

// After (OTE pattern -- optional, can be done later):
// Add to cmd/main.go's componentSpecs.Walk:
// spec.Exclude(et.PlatformEquals("vsphere"))
// spec.Exclude(et.PlatformEquals("nutanix"))
```

For now, leaving g.Skip as-is is fine -- the tests will still pass correctly.

### Step 8: Build and Verify

```bash
cd $WMCO_REPO/ote

# Build
make build

# Verify the new test appears in list output
./bin/windows-machine-config-operator-tests-ext list | grep "$OCP_ID"

# Count total migrated tests
echo "Total tests: $(./bin/windows-machine-config-operator-tests-ext list 2>/dev/null | wc -l)"
```

### Step 9: Show Diff and Wait

Show the diff of changes. NEVER commit or push without explicit user confirmation.

```bash
cd $WMCO_REPO
git diff ote/
```

---

## Example: Migrating OCP-33612

### Step 1: Find it

```bash
grep -n "g\.It.*33612" $OTP_REPO/test/extended/winc/winc.go
# Output: 54:  g.It("Smokerun-Author:sgao-Critical-33612-Windows node basic check", func() {
```

Found in winc.go at line 54. Target: `ote/test/e2e/winc.go`

### Step 2: Extract the block

Read lines 54-136 from OTP winc.go (the full g.It block for 33612).

### Step 3: Trace dependencies

Functions used by OCP-33612:
- `oc` -- Describe-level (already in shell)
- `iaasPlatform`, `privateKey` -- BeforeEach (already in shell)
- `svcs` -- package-level var in utils.go (already copied)
- `getKubeletVersionWithRetry()` -- utils.go (already copied)
- `matchKubeletVersion()` -- utils.go (already copied)
- `getWindowsHostNames()` -- utils.go (already copied)
- `checkVersionAnnotationReady()` -- utils.go (already copied)
- `getSSHBastionHost()` -- utils.go (already copied)
- `getWindowsInternalIPs()` -- utils.go (already copied)
- `checkWindowsServiceWithRetry()` -- utils.go (already copied)
- `wmcoNamespace` -- package-level var in utils.go (already copied)

All dependencies satisfied by bootstrap. No new helpers needed.

### Step 4: Check imports

The g.It block uses: `fmt`, `strings`, `time`, `e2e`, `o`, `g`, `os/exec`

Check if all are in ote/test/e2e/winc.go's import block. Add any missing ones.

### Step 5: Insert

Add the g.It block before the closing `})` in ote/test/e2e/winc.go:

```go
	// author: sgao@redhat.com
	g.It("Smokerun-Author:sgao-Critical-33612-Windows node basic check", func() {
		// ... exact copy of the test body ...
	})
```

### Step 6: Build

```bash
cd $WMCO_REPO/ote && make build
./bin/windows-machine-config-operator-tests-ext list | grep 33612
# Should show: [OTP][sig-windows] Windows_Containers Smokerun-Author:sgao-Critical-33612-Windows node basic check
```

Done. Show diff, wait for user.

---

## Dockerfile Integration (Manual)

After migration is complete, update the WMCO Containerfile to build and include the OTE binary.

### Add test extension builder stage

Add after the existing builder stage in `Containerfile`:

```dockerfile
FROM registry.ci.openshift.org/ocp/builder:rhel-9-golang-1.25-openshift-4.22 AS test-extension-builder
RUN mkdir -p /go/src/github.com/openshift/windows-machine-config-operator
WORKDIR /go/src/github.com/openshift/windows-machine-config-operator
COPY . .
RUN cd ote && make build && \
    gzip ote/bin/windows-machine-config-operator-tests-ext
```

### Add COPY to final stage

```dockerfile
COPY --from=test-extension-builder \
    /go/src/github.com/openshift/windows-machine-config-operator/ote/bin/windows-machine-config-operator-tests-ext.gz \
    /usr/bin/
```

---

## cmd/main.go Template

Full template for when cmd/main.go needs to be created or regenerated:

```go
package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/component-base/logs"

	"github.com/openshift-eng/openshift-tests-extension/pkg/cmd"
	e "github.com/openshift-eng/openshift-tests-extension/pkg/extension"
	et "github.com/openshift-eng/openshift-tests-extension/pkg/extension/extensiontests"
	g "github.com/openshift-eng/openshift-tests-extension/pkg/ginkgo"
	"github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	framework "k8s.io/kubernetes/test/e2e/framework"

	_ "github.com/openshift/windows-machine-config-operator/ote/test/e2e/testdata"
	_ "github.com/openshift/windows-machine-config-operator/ote/test/e2e"
)

func main() {
	util.InitStandardFlags()
	framework.AfterReadingAllFlags(&framework.TestContext)

	logs.InitLogs()
	defer logs.FlushLogs()

	registry := e.NewRegistry()
	ext := e.NewExtension("openshift", "payload", "windows-machine-config-operator")

	registerSuites(ext)

	allSpecs, err := g.BuildExtensionTestSpecsFromOpenShiftGinkgoSuite()
	if err != nil {
		panic(fmt.Sprintf("couldn't build extension test specs from ginkgo: %+v", err.Error()))
	}

	componentSpecs := allSpecs.Select(func(spec *et.ExtensionTestSpec) bool {
		for _, loc := range spec.CodeLocations {
			if strings.Contains(loc, "/test/e2e/") &&
				!strings.Contains(loc, "/go/pkg/mod/") &&
				!strings.Contains(loc, "/vendor/") {
				return true
			}
		}
		return false
	})

	componentSpecs.AddBeforeAll(func() {
		if err := compat_otp.InitTest(false); err != nil {
			panic(err)
		}
		util.WithCleanup(func() {})
	})

	componentSpecs.Walk(func(spec *et.ExtensionTestSpec) {
		for label := range spec.Labels {
			if strings.HasPrefix(label, "Platform:") {
				platformName := strings.TrimPrefix(label, "Platform:")
				spec.Include(et.PlatformEquals(platformName))
			}
		}
		re := regexp.MustCompile(`\[platform:([a-z]+)\]`)
		if match := re.FindStringSubmatch(spec.Name); match != nil {
			spec.Include(et.PlatformEquals(match[1]))
		}
		spec.Lifecycle = et.LifecycleInforming
	})

	ext.AddSpecs(componentSpecs)
	registry.Register(ext)

	root := &cobra.Command{
		Long: "Windows Machine Config Operator Tests",
	}
	root.AddCommand(cmd.DefaultExtensionCommands(registry)...)

	if err := func() error {
		return root.Execute()
	}(); err != nil {
		os.Exit(1)
	}
}

func registerSuites(ext *e.Extension) {
	suites := []e.Suite{
		{
			Name:    "windows-machine-config-operator/conformance/parallel",
			Parents: []string{"openshift/conformance/parallel"},
			Qualifiers: []string{
				`name.contains("[Level0]") && !(name.contains("[Serial]") || name.contains("[Disruptive]"))`,
			},
		},
		{
			Name:    "windows-machine-config-operator/conformance/serial",
			Parents: []string{"openshift/conformance/serial"},
			Qualifiers: []string{
				`name.contains("[Level0]") && name.contains("[Serial]") && !name.contains("[Disruptive]")`,
			},
		},
		{
			Name:    "windows-machine-config-operator/disruptive",
			Parents: []string{"openshift/disruptive"},
			Qualifiers: []string{`name.contains("[Disruptive]")`},
		},
		{
			Name:       "windows-machine-config-operator/non-disruptive",
			Qualifiers: []string{`!name.contains("[Disruptive]")`},
		},
		{
			Name: "windows-machine-config-operator/all",
		},
	}
	for _, suite := range suites {
		ext.AddSuite(suite)
	}
}
```

---

## CI Integration and Testing

### How Tests Run After Migration

Once the OTE binary is built into the WMCO container image and registered in origin's extension registry,
tests run automatically in existing CI jobs. You do NOT need to create new CI job configurations.

The flow:
1. ci-operator builds WMCO image from Dockerfile (includes gzipped OTE binary in `/usr/bin/`)
2. `openshift-tests` discovers the extension binary at runtime
3. Calls `./binary info` and `./binary list` to discover tests
4. Schedules tests into matching suites (parallel, serial, etc.)
5. Runs tests via `./binary run-test -n <test-name>`

Tests automatically run in:
- Component repo presubmits (e.g. e2e-aws-ovn, e2e-gcp)
- Periodic jobs that include matching test suites
- Any job running `openshift-tests` with suites that match

### Registering in Origin's Extension Registry

After migration, register the binary in `openshift/origin`:
- File: `pkg/test/extensions/binary.go`
- Add the WMCO extension binary entry
- This PR can be tested together with the WMCO PR using multi-PR testing

### Multi-PR Testing

Test OTE changes across repos before either PR merges:

```
/payload-job-with-prs <job_name> openshift/origin#<PR_NUMBER>
```

Example: if PR in WMCO has OTE tests and PR in origin registers the binary:
```
/payload-job-with-prs periodic-ci-openshift-windows-machine-config-operator-master-e2e-aws openshift/origin#12345
```

### Test Lifecycle: Informing vs Blocking

All newly migrated tests start as `LifecycleInforming` (already set in main.go template).
This means test failures are non-fatal for 2-3 sprints while reliability is validated.

To mark individual tests as informing using Ginkgo labels:
```go
g.It("my test name", ote.Informing(), func() {
    // test code
})
```

Once tests achieve >=99% pass rate, remove informing status to make them blocking.

### Environment Selectors

Control which tests run on specific platforms/topologies. Already handled in main.go
via the `Platform:` label prefix and `[platform:xxx]` name tag patterns.

Additional selectors available (add to main.go Walk function as needed):
```go
// Skip on HyperShift
spec.Exclude(et.TopologyEquals("External"))

// Skip on disconnected environments
spec.Exclude(et.ExternalConnectivityEquals("Disconnected"))

// Only run on specific platforms
spec.Include(et.Or(et.PlatformEquals("aws"), et.PlatformEquals("azure")))
```

See the Integration Guide for full list of available environment flags:
`--platform`, `--topology`, `--network`, `--network-stack`, `--architecture`,
`--external-connectivity`, `--version`, `--upgrade`, `--feature-gate`,
`--api-group`, `--optional-capability`

### Test Compliance Requirements (from Integration Guide)

- Ownership: each test must have `[sig-windows]` tag or entry in ci-test-mapping
- Tracking: `[OTP]` annotation on all ported Describe blocks
- Stable names: no dynamic content (pod UIDs, timestamps) in test names
- Deterministic results: must always produce Pass or Fail
- Reliability: must pass at >=99% (after informing period)
- Suite membership: every test must be in at least one suite

### Build Requirements

- Build statically linked: `CGO_ENABLED=0`
- Gzip compress the binary (NOT tar): `gzip binary` produces `binary.gz`
- Set `GO_COMPLIANCE_POLICY="exempt_all"` for ART compliance
- Include the `.gz` file in the Dockerfile at `/usr/bin/`

### Local Testing

After building, verify the binary works:
```bash
cd ote/
make build
./bin/windows-machine-config-operator-tests-ext info     # check API version, metadata
./bin/windows-machine-config-operator-tests-ext list     # list all discovered tests
./bin/windows-machine-config-operator-tests-ext run-test -n "<test-name>"  # run single test
```

---

## Tracking Progress

After each test migration, update the tracking table:

| OCP ID | Test Name (short) | Source File | Status | PR |
|--------|-------------------|-------------|--------|-----|
| 33612 | Windows node basic check | winc.go | pending | |
| 32615 | Generate userData secret | winc.go | pending | |
| 32554 | wmco HostNetwork | winc.go | pending | |
| ... | ... | ... | ... | |
| 66352 | CSI persistent storage | storage.go | pending | |

Use `grep -c "g\.It" ote/test/e2e/winc.go ote/test/e2e/storage.go` to count migrated tests.

---

## Troubleshooting

### "May only be called from within a test case" panic

`compat_otp.NewCLIWithoutNamespace()` called before BeforeAll initializes the framework. Move CLI init into BeforeEach if this occurs:

```go
var oc *exutil.CLI
g.BeforeEach(func() {
    oc = compat_otp.NewCLIWithoutNamespace("default")
})
```

### sum.golang.org 500/410 errors

Internal OpenShift packages are not in the public Go module proxy:

```bash
GOTOOLCHAIN=auto GONOSUMDB="*" go mod tidy
```

### Vendor directory out of sync

```bash
rm -rf vendor/
GOTOOLCHAIN=auto GONOSUMDB="*" go mod tidy
GOTOOLCHAIN=auto GONOSUMDB="*" go mod vendor
make clean && make build
```

### bindata.go not found

```bash
cd test/e2e && make -f bindata.mk update-bindata
```

### New test needs a package not in go.mod

```bash
cd $WMCO_REPO/ote
GOTOOLCHAIN=auto GONOSUMDB="*" go get <package>@<version>
GOTOOLCHAIN=auto GONOSUMDB="*" go mod tidy
GOTOOLCHAIN=auto GONOSUMDB="*" go mod vendor
make build
```

### Build fails after adding test with unused imports

The test block may reference packages not used by other tests in the file. Add the import and rebuild. If the import is only used conditionally (e.g., inside an if block), Go still requires it in the import block.

---

## Rules

- ALWAYS fetch latest upstream master/main BEFORE starting any migration work
- NEVER commit or push without explicit user confirmation
- NEVER add Co-Authored-By or AI attribution to commits
- NEVER use emojis in any output
- Use `GONOSUMDB="*"` for all go get/tidy/vendor commands
- The ote/ module is `github.com/openshift/windows-machine-config-operator/ote`
- Test package is `winc` (matching OTP source)
- Testdata fixtures go under `test/e2e/testdata/winc/`
- `template_test.go` is standard Go test (`package winc_test`), not Ginkgo -- runs via `go test`, not OTE binary
- `windows_template.go` has a FixturePath call in `GetFileContent()` that must be migrated
- Always verify build succeeds before reporting completion
- Always show diff and wait for user before any git operations
- Bootstrap creates empty Describe shells with NO tests -- tests are added one at a time
- Each test migration is one PR (or batch of related tests per PR)

---

## References

- OTE Integration Guide: https://docs.google.com/document/d/1cFZj9QdzW8hbHc3H0Nce-2xrJMtpDJrwAse9H7hLiWk/edit
- OTE Enhancement: https://github.com/openshift/enhancements/blob/master/enhancements/testing/openshift-tests-extension.md
- OTE Repository: https://github.com/openshift-eng/openshift-tests-extension
- Origin extension registry: https://github.com/openshift/origin/blob/master/pkg/test/extensions/binary.go
- MCO OTE PR (reference implementation): https://github.com/openshift/machine-config-operator/pull/5108
- NTO OTE PR (reference implementation): https://github.com/openshift/cluster-node-tuning-operator/pull/1436
- OTE Migration Plugin Design: https://redhat.atlassian.net/wiki/spaces/OCPERT/pages/265782250/OTE-migration+Plugin+Design
- Component Teams progress tracker: https://docs.google.com/spreadsheets/d/16jUpKEe-U708Bjk4HthQF8R2BbbgNPvaTh2UKPj0TR8/edit
- Slack: #wg-openshift-tests-extension
- ci-test-mapping: https://github.com/openshift-eng/ci-test-mapping
