# Adding PTR records for platform=none CI clusters in vSphere

WMCO's CSRs approver requires reverse DNS lookup for each Windows instance that 
joins the cluster as a Windows worker. For CI clusters with platform-agnostic
infrastructure (platform=none) a fixed lease pool is configured to ensure
reverse DNS lookup is possible.

The [create-ptr-records.sh](create-ptr-records.sh) script creates the reverse
DNS lookup by creating PTR records for each IP address available in the selected
subnets.

Before running the [create-ptr-records.sh](create-ptr-records.sh) script, ensure
AWS CLI is properly configured with AWS credentials and region for
`openshift-vmware-cloud-ci` account, for example:
```shell
# configures AWS CLI
export AWS_PROFILE="openshift-vmware-cloud-ci"
export AWS_REGION="us-west-2"

# run script
./create-ptr-records.sh
```
where `openshift-vmware-cloud-ci` is the name of profile that contains the
credentials, and `us-west-2` is the region where the resources were provisioned.
