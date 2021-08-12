# BYOH Instance Pre-requisites

The following pre-requisites must be fulfilled in order to add a Windows BYOH node.
* The Docker container runtime must be installed on the instance.
* The instance must be on the same network as the Linux worker nodes in the cluster.
* If the cluster platform is set, the instance being added must be part of the platform.
  * The platform of a cluster can be determined with: `oc get infrastructure cluster --template={{.status.platform}}`
* Port 22 must be open and running [an SSH server](https://docs.microsoft.com/en-us/windows-server/administration/openssh/openssh_install_firstuse).
* Port 10250 must be open in order for log collection to function.
* An administrator user is present with the [private key used in the secret](/README.md#create-a-private-key-secret) set as an authorized SSH key.
* The hostname of the instance must follow the [RFC 1123](https://datatracker.ietf.org/doc/html/rfc1123) DNS label standard:
  * Contain only lowercase alphanumeric characters or '-'.
  * Start with an alphanumeric character.
  * End with an alphanumeric character.
