---
description: Create a vSphere Windows Server golden image template using govc
args: "[windows_version] [template_name]"
allowed-tools: Bash, Read, Write, AskUserQuestion
---

# Create vSphere Windows Server Golden Image

Create a Windows Server golden image template in vSphere for use with WMCO (Windows Machine Config Operator). The golden image is a generalized VM template that WMCO uses to provision Windows worker nodes.

This skill supports two modes:
1. **From existing template** -- Clone an existing dev/base template, inject an SSH public key, sysprep, and mark as template
2. **From ISO (Packer)** -- Build from scratch using Packer with the HCL build files in `docs/vsphere_ci/`

## Requirements

- `govc` installed and configured (GOVC_URL, GOVC_USERNAME, GOVC_PASSWORD, GOVC_DATACENTER)
- VPN/network access to the VCenter
- An SSH public key file (the corresponding private key will be provided to WMCO as a Secret)
- For Packer builds: Packer 1.8.3+, Windows ISO and VMware Tools ISO on a vSphere datastore

## Mode 1: Clone from existing template

Use this when a base template already exists and you need to create a variant with a different SSH key or configuration.

### Step 1: Gather parameters

If arguments are not provided, ask the user for:
- **Source template path**: The base template to clone from
- **Target template name**: Name for the new template
- **SSH public key file**: Path to the public key to inject (e.g., `~/.ssh/id_rsa.pub`)
- **Template folder**: Folder in vSphere inventory (default: `windows-golden-images`)

List available templates to help the user choose:
```bash
govc ls /${GOVC_DATACENTER}/vm/windows-golden-images/
```

### Step 2: Validate prerequisites

```bash
govc about
govc vm.info "windows-golden-images/${SOURCE_TEMPLATE}"
cat ${SSH_PUBLIC_KEY_FILE}
```

If the target already exists, ask the user whether to overwrite or abort.

### Step 3: Clone template to VM

```bash
govc vm.clone \
  -vm "windows-golden-images/${SOURCE_TEMPLATE}" \
  -on=false \
  -folder "windows-golden-images" \
  "${TARGET_TEMPLATE}"
```

### Step 4: Power on and wait for VMware Tools

```bash
govc vm.power -on "windows-golden-images/${TARGET_TEMPLATE}"
```

Poll every 15 seconds until VMware Tools reports ready:
```bash
govc object.collect -s "windows-golden-images/${TARGET_TEMPLATE}" guest.toolsStatus
```

Wait until it returns `toolsOk` or `toolsOld`.

### Step 5: Inject SSH public key

Upload the public key to the VM:
```bash
govc guest.upload \
  -vm "windows-golden-images/${TARGET_TEMPLATE}" \
  -l "Administrator:WindowsPassword" \
  ${SSH_PUBLIC_KEY_FILE} \
  "C:\\ssh-key.pub"
```

Create a temporary PowerShell script that replaces the SSH authorized key:
```powershell
$keyContent = Get-Content "C:\ssh-key.pub"
$authorizedKeyFile = "C:\ProgramData\ssh\administrators_authorized_keys"
Set-Content -Path $authorizedKeyFile -Value $keyContent -Encoding ASCII
$acl = Get-Acl $authorizedKeyFile
$acl.SetAccessRuleProtection($true, $false)
$administratorsRule = New-Object System.Security.AccessControl.FileSystemAccessRule("Administrators","FullControl","Allow")
$acl.SetAccessRule($administratorsRule)
$systemRule = New-Object System.Security.AccessControl.FileSystemAccessRule("SYSTEM","FullControl","Allow")
$acl.SetAccessRule($systemRule)
$acl | Set-Acl
Restart-Service sshd
Write-Output "SSH key replaced successfully"
```

Upload and execute:
```bash
govc guest.upload \
  -vm "windows-golden-images/${TARGET_TEMPLATE}" \
  -l "Administrator:WindowsPassword" \
  /tmp/replace-ssh-key.ps1 \
  "C:\\replace-ssh-key.ps1"

govc guest.run \
  -vm "windows-golden-images/${TARGET_TEMPLATE}" \
  -l "Administrator:WindowsPassword" \
  powershell.exe -ExecutionPolicy Bypass -File "C:\\replace-ssh-key.ps1"
```

Verify the output contains "SSH key replaced successfully".

### Step 6: Run sysprep

Sysprep generalizes the image (unique SID and hostname for each clone):
```bash
govc guest.run \
  -vm "windows-golden-images/${TARGET_TEMPLATE}" \
  -l "Administrator:WindowsPassword" \
  "C:\\Windows\\System32\\Sysprep\\sysprep.exe" /generalize /oobe /shutdown /quiet
```

NOTE: This command may return a `ServerFaultCode: Failed to authenticate` error because sysprep shuts down the VM and govc loses the connection during cleanup. This is expected behavior, not a real failure.

### Step 7: Wait for VM to power off

Poll every 15 seconds, up to 3 minutes:
```bash
govc vm.info "windows-golden-images/${TARGET_TEMPLATE}" | grep "Power state"
```

Wait until it shows `poweredOff`.

### Step 8: Mark as template

```bash
govc vm.markastemplate "windows-golden-images/${TARGET_TEMPLATE}"
```

### Step 9: Verify and report

```bash
govc vm.info "windows-golden-images/${TARGET_TEMPLATE}"
```

Print summary:
```
Golden image created successfully.

  Source:   windows-golden-images/${SOURCE_TEMPLATE}
  Target:   windows-golden-images/${TARGET_TEMPLATE}
  Path:     /${GOVC_DATACENTER}/vm/windows-golden-images/${TARGET_TEMPLATE}
  SSH Key:  ${SSH_PUBLIC_KEY_FILE}

To use this template with WMCO, set the MachineSet providerSpec.template to:
  windows-golden-images/${TARGET_TEMPLATE}

For OpenShift CI step-registry, override WINDOWS_OS_ID:
  WINDOWS_OS_ID: windows-golden-images/${TARGET_TEMPLATE}
```

---

## Mode 2: Build from ISO with Packer

Use this to build a golden image from scratch. This is the recommended approach for new environments.

### Step 1: Gather parameters

Ask the user for:
- **Windows Server version**: 2022 or 2025
- **Template name**: Name for the resulting template
- **VCenter details**: server, user, password, datacenter, cluster, network, datastore
- **ISO paths**: Windows OS ISO and VMware Tools ISO paths on the datastore
- **SSH public key file**: Path to the authorized_keys file
- **Administrator password**: Password for the VM (must match autounattend.xml)
- **Optional flags**: IPv6 disabled, FIPS mode

### Step 2: Prepare build files

The Packer build requires these files from `docs/vsphere_ci/`:

**Build file** (`windows-server-2022.pkr.hcl` or copy for 2025):
- Update all `<placeholder>` variables with the user's VCenter details
- Adjust `guest_os_type` if needed for the Windows version

**Scripts directory** (must be present where Packer runs):
- `authorized_keys` -- the SSH public key
- `configure-vm-tools.ps1` -- configures VMware Tools network reporting
- `disable-ipv6.ps1` -- disables IPv6 (optional, included by default)
- `install-firewall-rules.ps1` -- opens ports 10250 (container logs) and 9182 (Windows Exporter)
- `install-openssh.ps1` -- installs and configures OpenSSH Server with key-based auth
- `install-updates.ps1` -- installs Windows updates via PSWindowsUpdate
- `install-vm-tools.cmd` -- installs VMware Tools from mounted ISO
- `rename-computer.ps1` -- randomizes hostname on first boot

**Answer files directory**:
- `autounattend.xml` -- automates Windows installation (update password to match)
- `unattend.xml` -- sysprep answer file for generalization

### Step 3: Copy SSH key to scripts directory

```bash
cp ${SSH_PUBLIC_KEY_FILE} docs/vsphere_ci/scripts/authorized_keys
```

### Step 4: Update build file variables

Create a copy of the HCL file with the user's values:
- `os-iso-path`: datastore path to Windows ISO
- `vmtools-iso-path`: datastore path to VMware Tools ISO
- `vm-template-folder`: target folder (e.g., `windows-golden-images`)
- `vm-template-name`: target template name
- `vm-elevated-password`: Administrator password
- `vsphere-cluster`, `vsphere-datacenter`, `vsphere-network`, `vsphere-datastore`
- `vsphere-server`, `vsphere-user`, `vsphere-password`

### Step 5: Run Packer build

```bash
cd docs/vsphere_ci
packer build windows-server-XXXX.pkr.hcl
```

The build takes 30-60 minutes. It will:
1. Create a VM from the Windows ISO
2. Run autounattend.xml to automate OS installation
3. Install VMware Tools, OpenSSH, firewall rules
4. Install Windows updates (twice, with reboot between)
5. Disable IPv6 (if configured)
6. Sysprep and shutdown
7. Convert to template

Monitor via VCenter web console if the build stalls (may need manual product key bypass).

### Step 6: Verify

```bash
govc vm.info "windows-golden-images/${TARGET_TEMPLATE}"
```

---

## Template Naming Convention

```
windows-server-{VERSION}-template[-VARIANT][-YYYYMMDD]
```

- `{VERSION}`: 2019, 2022, or 2025
- `[-VARIANT]`: optional suffixes like `ipv6-disabled`, `fips`
- `[-YYYYMMDD]`: optional date suffix for versioning

Examples:
- `windows-server-2022-template-ipv6-disabled`
- `windows-server-2025-template-ipv6-disabled`
- `windows-server-2022-template-ipv6-disabled-fips`

## Golden Image Requirements for WMCO

A valid golden image must have:
1. **OpenSSH Server** installed and running with key-based authentication
2. **SSH public key** in `C:\ProgramData\ssh\administrators_authorized_keys` with correct ACLs
3. **VMware Tools** installed and configured to report all network interfaces
4. **Firewall rules** allowing ports 10250 (container logs) and 9182 (Windows Exporter)
5. **Hostname randomization** script at `C:\rename-computer.ps1` (executed on first boot via unattend.xml)
6. **Generalized with sysprep** so each clone gets a unique SID

## Troubleshooting

- **govc connection fails**: Verify VPN is active and GOVC_* env vars are set
- **Clone fails**: Source template may be locked -- check with `govc vm.info`
- **Guest operations fail**: VM may not be fully booted -- wait for VMware Tools to report ready
- **Sysprep auth error on govc**: Expected -- sysprep shuts down the VM, govc loses connection
- **Packer halts at product key**: Access VM via VCenter web console, send Ctrl+Alt+Del, tab to "I don't have a product key", Enter
- **Windows updates take too long**: Normal -- Packer has a 1h restart timeout, updates run twice
