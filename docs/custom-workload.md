# Deploying a custom workload

## Example workload

An example Windows deployment would look like this.

```shell script
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
      os: 
        name: "windows"
      tolerations:
      - key: "os"
        value: "Windows"
        Effect: "NoSchedule"
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
      nodeSelector:
        kubernetes.io/os: windows
```

For more information about Windows workloads, visit
[the docs](<https://docs.openshift.com/container-platform/latest/windows_containers/scheduling-windows-workloads.html>).

## Configure the base image

It is very important to set the correct base image version for your test workload.
Each version of the WMCO uses a specific Windows operating system version, and each workload has its own base image.

The windows container image must match the node's OS version, which is explained in more detail here in the 
[Microsoft docs](https://learn.microsoft.com/en-us/virtualization/windowscontainers/deploy-containers/version-compatibility?tabs=windows-server-2022%2Cwindows-11#choose-which-container-os-version-to-use). 

To know which base image your release uses, check your MachineSet.yaml in the repo root, or your BYOH image. 


### Check the operating system on the cluster

To be sure that your deployment’s base image corresponds with the cluster’s OS version, check what version
of OS your cluster is running using your platform's recommended method. 

### Edit your deployment

Find the corresponding base image to set your deployment to. 
For example, on Azure these Windows versions correspond to these base image versions.

| Windows Version                 | Base Image version                                   |
|---------------------------------|------------------------------------------------------|
| 2019-Datacenter-with-Containers | mcr.microsoft.com/powershell:lts-nanoserver-1809     |
| 2022-datacenter-smalldisk       | mcr.microsoft.com/powershell:lts-nanoserver-ltsc2022 |

For information about other platforms, see the 
[prerequisites doc](https://github.com/openshift/windows-machine-config-operator/blob/master/docs/wmco-prerequisites.md#supported-windows-server-versions)

### Set the powershell executable

Different base images have different executables for powershell. When using powershell images, the 
command should be set to pwsh.exe.

## Deploy the workload

Deploy the workload by running

```shell script
oc apply -f deployment.yaml
```

