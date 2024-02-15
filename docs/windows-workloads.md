# Deploying a Windows workload

## Choosing a container image

When deploying Windows workloads, container image used must match the node's OS version.
A container built from Windows Server 2019 will fail to run on a Windows Server 2022 Node, and vice versa.

#### Microsoft Documentation
- [Choosing a container os version](https://learn.microsoft.com/en-us/virtualization/windowscontainers/deploy-containers/version-compatibility?tabs=windows-server-2022%2Cwindows-11#choose-which-container-os-version-to-use)
- [Windows Server versions](https://learn.microsoft.com/en-us/windows-server/get-started/windows-server-release-info)

## Using a RuntimeClass

### Why use a RuntimeClass

In order to properly schedule Windows workloads onto correct Nodes, a RuntimeClass should be used.
Without using one, there may be issues scheduling Windows pods to a Node, or having pods fail to come running due to a
mismatch of Windows versions between the container and the Node.

### Example Windows Server 2019 RuntimeClass

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: windows2019
handler: 'runhcs-wcow-process'
scheduling:
  nodeSelector:
    kubernetes.io/os: 'windows'
    kubernetes.io/arch: 'amd64'
    # This must match the Windows Server build, for 2022 use 10.0.20348
    node.kubernetes.io/windows-build: '10.0.17763'
  tolerations:
    - effect: NoSchedule
      key: os
      operator: Equal
      value: "windows"
    - effect: NoSchedule
      key: os
      operator: Equal
      value: "Windows"
```

## Example Windows Server 2019 workload

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app: win-webserver
  name: win-webserver
spec:
  selector:
    matchLabels:
      app: win-webserver
  replicas: 1
  template:
    metadata:
      labels:
        app: win-webserver
      name: win-webserver
    spec:
      containers:
      - name: windowswebserver
        image: mcr.microsoft.com/windows/servercore:ltsc2019
        imagePullPolicy: IfNotPresent
        command:
        - powershell.exe
        - -command
        - $listener = New-Object System.Net.HttpListener; $listener.Prefixes.Add('http://*:80/'); $listener.Start();Write-Host('Listening at http://*:80/'); while ($listener.IsListening) { $context = $listener.GetContext(); $response = $context.Response; $content='<html><body><H1>Red Hat OpenShift + Windows Container Workloads</H1></body></html>'; $buffer = [System.Text.Encoding]::UTF8.GetBytes($content); $response.ContentLength64 = $buffer.Length; $response.OutputStream.Write($buffer, 0, $buffer.Length); $response.Close(); };
        securityContext:
          runAsNonRoot: false
          windowsOptions:
            runAsUserName: "ContainerAdministrator"
      os:
        name: "windows"
      runtimeClassName: windows2019
```
