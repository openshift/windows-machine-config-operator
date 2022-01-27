---
name: Bug report
about: Create a report to help us improve
title: ''
labels: ''
assignees: ''

---

**Describe the bug**
A clear and concise description of what the bug is.

**To Reproduce**
Steps to reproduce the behavior:
1. Go to '...'
2. Click on '....'
3. Scroll down to '....'
4. See error

**Expected behavior**
A clear and concise description of what you expected to happen.

**Screenshots**
If applicable, add screenshots to help explain your problem.

**Must gather logs**
1. WMCO & OpenShift Version 
2. Platform - AWS/Azure/vSphere/Platform=none
If the platform is vSphere,  what is the VMware tools version?
3. Is it a new test case or an old test case?
 if it is the old test case, is it regression or first-time tested? 
4. Is it platform-specific or consistent across all platforms?
5. A possible workaround has been tried? Is there a way to recover from the issue being tried out?
6. Logs
Must-gather-windows-node-logs
- oc get network.operator cluster -o yaml
- oc logs -f deployment/windows-machine-config-operator -n openshift-windows-machine-config-operator
Windows MachineSet yaml or windows-instances ConfigMap
- oc get machineset <windows_machineSet_name> -n openshift-machine-api -o yaml
- oc get configmaps <windows_configmap_name> -n <namespace_name> -o yaml




 Optional logs
	Anything that can be useful to debug the issue.
