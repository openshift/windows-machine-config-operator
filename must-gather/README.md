# Support for OpenShift Must-Gather
This tool is shipped in the operator images as a complement for [OpenShift must-gather](https://github.com/openshift/must-gather)
to expands its capabilities to gather specific information for the OpenShift Windows Machine Config Operator.

## Usage
To gather only OpenShift Windows Machine Config Operator information use the following command: 
```shell script
oc adm must-gather --image="$(oc get packagemanifests openshift-windows-machine-config-operator \
  -n openshift-marketplace \
  -o jsonpath='{.status.channels[0].currentCSVDesc.annotations.containerImage}')"
```
where a custom image for the must-gather command is pulled directly from the operator packagemanifests, so that 
it works on any cluster where OpenShift Windows Machine Config Operator is available.

To gather the default [OpenShift must-gather](https://github.com/openshift/must-gather) in addition to OpenShift 
Windows Machine Config Operator information you should fetch the operator image and combine both images with the 
following command:
```shell script
oc adm must-gather --image-stream=openshift/must-gather \
    --image="$(oc get packagemanifests openshift-windows-machine-config-operator \
        -n openshift-marketplace \
        -o jsonpath='{.status.channels[0].currentCSVDesc.annotations.containerImage}')"
 ```

In case the OpenShift Windows Machine Config Operator was deployed directly to the cluster, without OLM, you can
use the following command to gather the information using the image from the operator deployment:
```shell script
oc adm must-gather --image=$(oc get deployment.apps/windows-machine-config-operator \
  -n openshift-windows-machine-config-operator \
  -o jsonpath='{.spec.template.spec.containers[?(@.name == "manager")].image}')
```

## Collection script data
As a result of the above commands a local directory will be crated with a dump of the resources for OpenShift Windows
Machine Config Operator with the following structure:

- The `openshift-windows-machine-config-operator` namespace and its children objects, including but not limited to:
  - the `windows-instance` ConfigMap
  - the `windows-services` ConfigMap
  - the `windows-machine-config-operator` pod logs

In order to get data about other parts of the cluster that are not specific to OpenShift Windows Machine Config Operator
you should run `oc adm must-gather` without passing the custom image. Run `oc adm must-gather -h` to see more options.

## Must gather sample output
The following snippet shows a sample output of the directory tree for the OpenShift Windows Machine Config Operator 
must-gather:
```
├── namespaces
│   └── openshift-windows-machine-config-operator
│       ├── apps
│       │   ├── daemonsets.yaml
│       │   ├── deployments.yaml
│       │   ├── replicasets.yaml
│       │   └── statefulsets.yaml
│       ├── apps.openshift.io
│       │   └── deploymentconfigs.yaml
│       ├── autoscaling
│       │   └── horizontalpodautoscalers.yaml
│       ├── batch
│       │   ├── cronjobs.yaml
│       │   └── jobs.yaml
│       ├── build.openshift.io
│       │   ├── buildconfigs.yaml
│       │   └── builds.yaml
│       ├── core
│       │   ├── configmaps.yaml
│       │   ├── endpoints.yaml
│       │   ├── events.yaml
│       │   ├── persistentvolumeclaims.yaml
│       │   ├── pods.yaml
│       │   ├── replicationcontrollers.yaml
│       │   ├── secrets.yaml
│       │   └── services.yaml
│       ├── discovery.k8s.io
│       │   └── endpointslices.yaml
│       ├── image.openshift.io
│       │   └── imagestreams.yaml
│       ├── k8s.ovn.org
│       │   ├── egressfirewalls.yaml
│       │   └── egressqoses.yaml
│       ├── monitoring.coreos.com
│       │   └── servicemonitors.yaml
│       ├── networking.k8s.io
│       │   └── networkpolicies.yaml
│       ├── openshift-windows-machine-config-operator.yaml
│       ├── pods
│       │   ├── windows-machine-config-operator-644f6675c8-4hwhw
│       │   │   ├── manager
│       │   │   │   └── manager
│       │   │   │       └── logs
│       │   │   │           ├── current.log
│       │   │   │           ├── previous.insecure.log
│       │   │   │           └── previous.log
│       │   │   └── windows-machine-config-operator-644f6675c8-4hwhw.yaml
│       │   └── windows-machine-config-operator-registry-server-7f8667b5fcgcfpb
│       │       ├── windows-machine-config-operator-registry-server
│       │       │   └── windows-machine-config-operator-registry-server
│       │       │       └── logs
│       │       │           ├── current.log
│       │       │           ├── previous.insecure.log
│       │       │           └── previous.log
│       │       └── windows-machine-config-operator-registry-server-7f8667b5fcgcfpb.yaml
│       ├── policy
│       │   └── poddisruptionbudgets.yaml
│       └── route.openshift.io
│           └── routes.yaml
├── gather-debug.log
└── version

```
