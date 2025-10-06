# Troubleshooting guide

## Enabling debug level logging
In the Subscription created to deploy the operator add the following to .spec.config.env
```
kind: Subscription
spec:
  config:
    env:
    - name: ARGS
      value: "--debugLogging"
```

## WMCO does not go to running
Please check if you are using an OKD/OCP cluster adhering to the [operator pre-requisites](wmco-prerequisites.md).

## Windows Machine does not become a worker node
There could be various reasons as to why a Windows Machine does not become a worker node. Please collect the WMCO logs
by executing:
```shell script
oc logs -f deployment/windows-machine-config-operator -n openshift-windows-machine-config-operator
```
File a GitHub issue and attach the logs to the issue along with the *MachineSet* used.

## Windows Server 2019 LTSC (1809) nodes never become Ready
Ensure that you have not [configured the cluster network](https://docs.redhat.com/en/documentation/openshift_container_platform/latest/html/networking/ovn-kubernetes-network-plugin) with a
custom VXLAN port, as that is not a supported feature in 1809.

## Accessing a Windows node
Windows nodes cannot be accessed using `oc debug node` as that requires running a privileged pod on the node which is
not yet supported for Windows. Instead, a Windows node can be accessed using SSH or RDP. An
[SSH bastion](https://github.com/eparis/ssh-bastion) needs to be setup for both methods. The following information is
common across both methods:
* The key used in the *cloud-private-key* [secret](../README.md#Usage) and the key used when creating the cluster should
  be added to the [ssh-agent](https://docs.redhat.com/en/documentation/openshift_container_platform/4.15/html/installing_on_azure/installing-azure-default#ssh-agent-using_installing-azure-default).
  For [security reasons](https://manpages.debian.org/buster/openssh-client/ssh.1.en.html#A) we suggest removing the keys
  from the ssh-agent after use.
* *\<username\>* is *Administrator* (AWS) or *capi* (Azure)
* *\<windows-node-internal-ip\>* is the internal IP address of the node, which can be discovered by:
  ```shell script
  oc get nodes <node-name> -o jsonpath={.status.addresses[?\(@.type==\"InternalIP\"\)].address}
  ```
Once the SSH bastion has been setup, you can use either method to access the Windows node.
### SSH
* Access the Windows node using the following commands:
  ```shell script
  ssh -t -o StrictHostKeyChecking=no -o ProxyCommand='ssh -A -o StrictHostKeyChecking=no -o ServerAliveInterval=30 -W %h:%p core@$(oc get service --all-namespaces -l run=ssh-bastion -o go-template="{{ with (index (index .items 0).status.loadBalancer.ingress 0) }}{{ or .hostname .ip }}{{end}}")' <username>@<windows-node-internal-ip>
  ```

### RDP
* Execute the following command to set up an SSH tunnel:
  ```shell script
  ssh -L 2020:<windows-node-internal-ip>:3389 core@$(oc get service --all-namespaces -l run=ssh-bastion -o go-template="{{ with (index (index .items 0).status.loadBalancer.ingress 0) }}{{ or .hostname .ip }}{{end}}")
  ```
  Do not exit the resulting shell.
* SSH into the Windows node and execute the following command to create a password for the user:
  ```powershell
  <username>@<node-name> C:\Users\username> net user <username> *
  ```
* You can now RDP into the Windows node at *localhost:2020* using an RDP client

## Rebooting a Windows node

In general, the operator tries to minimize disruptions and avoids node reboots whenever possible. Certain operations and
updates at the system level still require a traditional reboot process to ensure changes are applied correctly
and securely.

To reboot a Windows node, please follow the Openshift documentation on [Rebooting a node gracefully](https://docs.redhat.com/en/documentation/openshift_container_platform/latest/html/nodes/working-with-nodes#nodes-nodes-rebooting)
to cordon, drain and reboot with the following command in PowerShell:
```powershell
  Restart-Computer -Force
```

### Node reboot limitation in AWS

In AWS, Windows nodes are not able to become ready after [performing a graceful reboot](#rebooting-a-windows-node)
due to an inconsistency with the EC2 instance metadata routes and the HNS networks.

The cluster administrator must SSH into the instance and add the route after rebooting with the following command:
```cmd
  route add 169.254.169.254 mask 255.255.255.255 <gateway_ip>
```
where `169.254.169.254` and `255.255.255.255` are the address and network mask of the EC2 instance metadata endpoint,
respectively. The `<gateway_ip>` must be replaced by the corresponding IP address of the gateway in Windows instance
and can be found by running the following command:
```cmd
  ipconfig | findstr /C:"Default Gateway"
``` 

## How to collect Kubernetes node logs
Kubernetes node log files are in *C:\var\logs*. To view all the directories under *C:\var\logs*, execute:
```shell script
$ oc adm node-logs -l kubernetes.io/os=windows --path=/
ip-10-0-138-252.us-east-2.compute.internal containers/
ip-10-0-138-252.us-east-2.compute.internal hybrid-overlay/
ip-10-0-138-252.us-east-2.compute.internal kube-proxy/
ip-10-0-138-252.us-east-2.compute.internal kubelet/
ip-10-0-138-252.us-east-2.compute.internal pods/
```
You can now list files in the directories using the same command and view the individual log files. For example to view
the kubelet logs, you can execute:
```shell script
$ oc adm node-logs -l kubernetes.io/os=windows --path=/kubelet/kubelet.log
```

## How to collect containerd runtime logs
`containerd` runtime logs are part of the Kubernetes node logs, and you collect them with the following command:
```shell script
$ oc adm node-logs -l kubernetes.io/os=windows --path=/containerd/containerd.log
```

## How to collect Windows application event logs

The Get-WinEvent shim on the kubelet logs endpoint can be used to collect application event logs from Windows machines.
E.g. getting logs for any given service:
```shell script
$ oc adm node-logs -l kubernetes.io/os=windows --path=journal -u <LOG_NAME>
```
The same command is executed when collecting logs with `oc adm must-gather`.

Other Windows application logs from the EventLog can also be collected by specifying the respective service on a `-u` flag.
To view logs from all applications logging to the event logs on the Windows machine, run:
```shell script
$ oc adm node-logs -l kubernetes.io/os=windows --path=journal
```

Alternatively, any service event logs can be viewed using SSH with the following steps:
* SSH into the Windows node and enter PowerShell:
  ```powershell
  <username>@<node-name> C:\Users\username> powershell
  ```
* Now you can view any service logs by executing:
  ```powershell
  PS C:\Users\username> Get-EventLog -LogName Application -Source ServiceName
  ```

## How to collect a packet trace on Windows nodes
* An SSH [bastion](https://github.com/eparis/ssh-bastion) must first be deployed
* Use the hack/packet_trace.sh utility to start a trace
  ```shell script
  packet_trace.sh -i $SSH_KEY start $WIN_NODE_NAME
  ```
* Start the activity you wish to trace
* Stop the packet trace, downloading the trace files to the current directory
  ```shell script
  packet_trace.sh -i $SSH_KEY stop $WIN_NODE_NAME
  ```

## External troubleshooting references
* [Containers on Windows troubleshooting](https://docs.microsoft.com/en-us/virtualization/windowscontainers/troubleshooting)
* [Troubleshoot host and container image mismatches](https://docs.microsoft.com/en-us/virtualization/windowscontainers/deploy-containers/update-containers#troubleshoot-host-and-container-image-mismatches)
* [Common Kubernetes problems wrt Windows](https://docs.microsoft.com/en-us/virtualization/windowscontainers/kubernetes/common-problems)
