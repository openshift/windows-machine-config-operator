# Projected Volumes and Windows Pods

A [projected volume](https://kubernetes.io/docs/concepts/storage/volumes/#projected) allows mapping of several existing
volume sources into the same directory in the container. With the [proposal for file permission handling in projected
service account volume](https://github.com/kubernetes/enhancements/pull/1598) enhancement, the projected files have
the correct permissions set including container user ownership for Linux pods when `RunAsUser` is set in the Pod 
`SecurityContext`. However, for Windows pods this is not the case when `RunAsUsername` is set, due to differences in 
the way user accounts are managed in Windows. Windows stores and manages local user and group accounts in a database
file called Security Account Manager (SAM). This database is not shared between the Windows container host and the
running containers. This prevents the kubelet from setting correct ownership on the files in the projected volume.
This problem is tracked by [Windows Pod with RunAsUserName and a Projected Volume does not honor file
permissions in the volume](https://github.com/kubernetes/kubernetes/issues/102849) issue.

By default, the projected files will have the following ownership as shown for an example projected volume file:
```powershell
Path   : Microsoft.PowerShell.Core\FileSystem::C:\var\run\secrets\kubernetes.io\serviceaccount\..2021_08_31_22_22_18.318230061\ca.crt
Owner  : BUILTIN\Administrators
Group  : NT AUTHORITY\SYSTEM
Access : NT AUTHORITY\SYSTEM Allow  FullControl
         BUILTIN\Administrators Allow  FullControl
         BUILTIN\Users Allow  ReadAndExecute, Synchronize
Audit  :
Sddl   : O:BAG:SYD:AI(A;ID;FA;;;SY)(A;ID;FA;;;BA)(A;ID;0x1200a9;;;BU)
```
This implies all administrator users like `ContainerAdministrator` will have read, write and execute access while,
non-administrator users will have read and execute access.

## Bound Service Account Token Volume
The [Bound Service Account Token Volume](https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/#bound-service-account-token-volume)
feature results in the ServiceAccount admission controller adding a projected volume instead of a Secret-based volume
for the non-expiring service account token created by the Token Controller. This implies that on all Kubernetes 1.22+ 
clusters, a projected volume will be present on all Pods. Now for Windows Pods `RunAsUserName` is set in the Pod 
`SecurityContext`, the ownership will not be set correctly and will have the default permissions as shown above. This 
problem can get exacerbated when used in conjunction with
[HostPath](https://kubernetes.io/docs/concepts/storage/volumes/#hostpath) where best practices are not followed.
For example, giving a Pod access to ` c:\var\lib\kubelet\pods\` will result in that Pod being able to access service
account tokens from other Pods.

## RunAsUser
Creating a Windows Pod with `RunAsUser` in it's `SecurityContext` will result in the Pod being stuck at
`ContainerCreating` forever. This has also been called out in the [Windows Pod with RunAsUserName and a Projected Volume
does not honor file permissions in the volume](https://github.com/kubernetes/kubernetes/issues/102849) issue. Note that
`RunAsUser` is a Linux only option and shouldn't be applied to Windows Pods, however there could be admission plugins
that indiscriminately apply this to all Pods.  To handle  this scenario, a workaround ([only chown if non-windows
machine with projected volumes](https://github.com/openshift/kubernetes/pull/804)) has been introduced in OpenShift
4.7+. This brings the behavior back to how it was before the enhancement, [proposal for file permission handling in
projected service account volume](https://github.com/kubernetes/enhancements/pull/1598), was introduced, i.e.
`RunAsUsername` set in the Pod `SecurityContext` are not honored for projected volumes on Windows.
