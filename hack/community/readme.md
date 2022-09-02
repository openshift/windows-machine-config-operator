# Cut a new community release
In CI jobs the `COMMUNITY` environment variable is set to false by default, and 
is set true when testing PRs against community-4.x branches. When true, CI is 
signaled to copy the repo's bundle manifests into the test artifact directory, 
edit the manifests, and then use the revised bundle manifests in the PR's 
aws-e2e-operator test. After the test passes, the bundle manifests can be copied
from the PR's artifact directory and used to release a new community WMCO.

### Benefit
Previously, a manual testing process was done to ensure that the changes to the
Red Hat operators WMCO manifests were brought in while maintaining the
differences needed by the community operator. This new process automates the
testing portion of a community WMCO release by reducing the manual intervention
done in the manifests and testing the bundle manifests directly in CI.

### Process
1. Fork and clone [community-operators-prod](https://github.com/redhat-openshift-ecosystem/community-operators-prod)
repo.
2. Copy the manifests/ and metadata/ directories from the aws-e2e-operator test 
artifact directory of the most recent community-4.x branch PR.
3. Paste the manifest contents into a new folder under 
operators/community-windows-machine-config-operator/<x.y.z>. The folder title 
should be the next semantic version of the community WMCO that is to be 
released.
4. Scan over the CSV and annotation files to ensure all the necessary fields
were replaced by the generate.sh script. The script handles replacing the date, 
operator description, display name, and maturity.
5. Manual changes needed in the CSV:
   - Add 'community-' prefix to 
     - 'metadata.name' field
     - 'OPERATOR_NAME' environment variable value field
     'spec.install.spec.deployments.spec.template.spec.containers.env.value'
   - Add image path 
   'quay.io/openshift-windows/community-windows-machine-config-operator:community-4.x-<PR-commit-hash>' 
   to
     - 'metadata.annotations.containerImage'
     - 'spec.install.spec.deployments.spec.template.spec.containers.image'
   - If this is a minor release:
     - remove 'metadata.annotations.olm.skipRange'
     - add
       'spec.replaces: community-windows-machine-config-operator.v<x.y.z>' where 
     x.y.z is the last community WMCO version.
6. Manual changes needed in the metadata/annotations.yaml
   - Add 'community-' prefix to 
          'annotations.operators.operatorframework.io.bundle.package.v1'
   - Remove 'annotations.operators.operatorframework.io.test.config.v1'
7. Add a /hold to the PR.
8. Ask WINC team for reviews. 
9. Once the team has reviewed, /unhold the PR and it 
will merge in community-operators-prod. 
