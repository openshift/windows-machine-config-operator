# BYOH Instance Pre-requisites

The following pre-requisites must be fulfilled in order to add a Windows BYOH node.
* The instance must be on the same network as the Linux worker nodes in the cluster.
* Port 22 must be open and running [an SSH server](https://docs.microsoft.com/en-us/windows-server/administration/openssh/openssh_install_firstuse).
* Port 10250 must be open in order for log collection to function.
* An administrator user is present with the [private key used in the secret](/README.md#create-a-private-key-secret) set as an authorized SSH key.
* The hostname of the instance must follow the [RFC 1123](https://datatracker.ietf.org/doc/html/rfc1123) DNS label standard:
  * Contain only lowercase alphanumeric characters or '-'.
  * Start with an alphanumeric character.
  * End with an alphanumeric character.
* A PTR record must exist corresponding to the instance address which resolves to the instance hostname for successful reverse DNS lookups.
* Containerd should not be installed. If it is installed already, it is recommended to uninstall as WMCO installs and manages containerd.
