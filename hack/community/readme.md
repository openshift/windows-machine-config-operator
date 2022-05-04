# Cut a new community release
The generate.sh script automates the community release process by copying over 
the latest WMCO manifests to a new community-WMCO folder while also replacing 
necessary fields such as the name, description, and image. It also generates a 
signed commit ready to be pushed to community-operators-prod repo. 

## Pre-requisites
Before running generate.sh, please ensure the following has merged:
- openshift/release PR to add CI jobs to relevant community branch.
[Example](https://github.com/openshift/release/pull/27081)
- Submodule update PR for the relevant community branch. 
- Ensure WMCB submodule is pointing to relevant community branch.
[Example](https://github.com/openshift/windows-machine-config-operator/blob/community-4.10/.gitmodules#L4)
- Fork and clone [community-operators-prod repo](https://github.com/redhat-openshift-ecosystem/community-operators-prod) 
- Install yq:
```shell script
  curl -L https://github.com/mikefarah/yq/releases/download/v4.13.5/yq_linux_amd64 -o /tmp/yq && chmod +x /tmp/yq
```

### From WMCO root, run:
Parameter 3 can be grabbed from our [Quay repo](https://quay.io/repository/openshift-windows/community-windows-machine-config-operator?tab=tags).
```shell script
bash ./hack/community/generate.sh <community_wmco_version> <community_ocp_version> <latest_community-4.x_tag-commit-hash> <path/to/community-operators-prod>
```
Example: Cut community release v5.1.0 for community-4.10
```shell script
bash ./hack/community/generate.sh 5.1.0 community-4.10 community-4.10-hash ../community-operators-prod
```

### Review and test the operator
- Scan over the newly generated CSV to ensure all the necessary fields were replaced
- Follow: https://github.com/openshift/windows-machine-config-operator/blob/master/docs/HACKING.md#i-want-to-try-the-unreleased-wmco-and-deploy-it-mimicking-the-official-deployment-method 
  You may skip step: Building the bundle image.
- [CSV validation](https://operatorhub.io/preview) - paste in the CSV contents to
preview what the community operator will look like in Operator Hub.


### Ask for reviews
- Tag WINC team for review
- Tag your community-operators-prod PR reviewer to ask for a manual override to 
merge. Sample message: 
  - “This PR has been tested locally. OCP v4.x will not pass CI 
  since our operator requires a cluster with hybrid-ovn networking for it to 
  start up properly. This is by design. Will need a manual override for this to 
  merge.”
