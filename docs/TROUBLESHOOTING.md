# Troubleshooting guide

## WMCO does not go to running
Please check if you are using an OKD/OCP 4.6 cluster running on Azure or AWS, configured with
[hybrid OVN Kubernetes networking](setup-hybrid-OVNKubernetes-cluster.md).

## Windows Machine does not become a worker node
There could various reasons as to why a Windows Machine does not become a worker node. Please collect the WMCO logs
by executing:
```shell script
oc logs -f $(oc get pods -o jsonpath={.items[0].metadata.name} -n openshift-windows-machine-config-operator) -n openshift-windows-machine-config-operator
```
File a GitHub issue and attach the logs to the issue along with the *MachineSet* used.

## Accessing a Windows node
Windows nodes cannot be accessed using `oc debug node` as that requires running a privileged pod on the node which is
not yet supported for Windows. Instead, a Windows node can be accessed using SSH or RDP. An
[SSH bastion](https://github.com/eparis/ssh-bastion) needs to be setup for both methods. The following information is
common across both methods:
* The key used in the *cloud-private-key* [secret](../README.md#Usage) and the key used when creating the cluster should
  be added to the [ssh-agent](https://docs.openshift.com/container-platform/4.6/installing/installing_azure/installing-azure-default.html#ssh-agent-using_installing-azure-default).
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

## How to collect Kubernetes node logs
All Kubernetes node logs are in *C:\k\logs*. To view all the directories under *C:\k\logs*, execute:
```shell script
# oc adm node-logs -l kubernetes.io/os=windows --path=/
ip-10-0-138-252.us-east-2.compute.internal containers/
ip-10-0-138-252.us-east-2.compute.internal hybrid-overlay/
ip-10-0-138-252.us-east-2.compute.internal kube-proxy/
ip-10-0-138-252.us-east-2.compute.internal kubelet/
ip-10-0-138-252.us-east-2.compute.internal pods/
```
You can now list files in the directories using the same command and view the individual log files. For example to view
the kubelet logs, you can execute:
```shell script
oc adm node-logs -l kubernetes.io/os=windows --path=/kubelet/kubelet.log
```

## How to collect Docker logs
The Windows Docker service does not log to stdout but instead logs to the Windows' event log. You can view the Docker
event logs using the following steps:
* SSH into the Windows node and enter PowerShell:
  ```powershell
  <username>@<node-name> C:\Users\username> powershell
  ```
* Now you can view the Docker logs by executing:
  ```powershell
  PS C:\Users\username> Get-EventLog -LogName Application -Source Docker
  ```

## External troubleshooting references
* [Containers on Windows troubleshooting](https://docs.microsoft.com/en-us/virtualization/windowscontainers/troubleshooting)
* [Troubleshoot host and container image mismatches](https://docs.microsoft.com/en-us/virtualization/windowscontainers/deploy-containers/update-containers#troubleshoot-host-and-container-image-mismatches)
* [Docker for Windows troubleshooting](https://docs.docker.com/docker-for-windows/troubleshoot/)
* [Common Kubernetes problems wrt Windows](https://docs.microsoft.com/en-us/virtualization/windowscontainers/kubernetes/common-problems)
