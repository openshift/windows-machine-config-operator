# Setup hybrid OVNKubernetes cluster

This guide assumes the user has installed current versions of the OpenShift installer (`openshift-install`) and client (`oc`) binaries.
Please refer to the [official OpenShift Container Platform documentation](https://docs.openshift.com/container-platform/latest/welcome/index.html) for details.

## Create install-config

Run the following command and follow the instructions:
```sh
$ openshift-install create install-config --dir=<cluster_directory>
```

This results in an `install-config.yaml` file in `<cluster_directory>`.
Edit the `install-config.yaml` to switch `networkType` from
`OpenShiftSDN` to `OVNKubernetes`:
```sh
$ sed -i 's/OpenShiftSDN/OVNKubernetes/g' <cluster_directory>/install-config.yaml
```

## Create manifests

Now generate the manifests for the previously created *install-config*:
```sh
$ openshift-install create manifests --dir=<cluster_directory>
```
This creates a `manifests` and `openshift` folder in your `<cluster_directory>`.
Now create a `<cluster_directory>/manifests/cluster-network-03-config.yml` file with the following contents:
```yml
apiVersion: operator.openshift.io/v1
kind: Network
metadata:
  name: cluster
spec:
  defaultNetwork:
    ovnKubernetesConfig:
      hybridOverlayConfig:
        hybridClusterNetwork:
        - cidr: 10.132.0.0/14
          hostPrefix: 23
```
The above configuration is recommended for AWS and Azure clusters.

### vSphere
For vSphere clusters, you must add the `hybridOverlayVXLANPort` option to work around the pod-to-pod connectivity
between hosts [issue](https://docs.microsoft.com/en-us/virtualization/windowscontainers/kubernetes/common-problems#pod-to-pod-connectivity-between-hosts-is-broken-on-my-kubernetes-cluster-running-on-vsphere):
```yml
apiVersion: operator.openshift.io/v1
kind: Network
metadata:
  name: cluster
spec:
  defaultNetwork:
    ovnKubernetesConfig:
      hybridOverlayConfig:
        hybridClusterNetwork:
          - cidr: 10.132.0.0/14
            hostPrefix: 23
        hybridOverlayVXLANPort: 9898
```
#### Attention
- The `hybridClusterNetwork` CIDR cannot overlap with the `clusterNetwork` CIDR
- You cannot use Windows Server 2019 LTSC (1809) as it does not have the kernel feature required for using custom VXLAN 
  ports

## Create cluster

Now proceed to cluster creation:
```sh
$ openshift-install create cluster --dir=<cluster_directory>
```

Wait until cluster creation has succeeded and login credentials are shown.
