package smb

import (
	"context"
	"fmt"
	"reflect"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	storage "k8s.io/api/storage/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	client "k8s.io/client-go/kubernetes"
)

var (
	smbNamespace          = "csi-smb-controller"
	smbDriverName         = "smb.csi.k8s.io"
	smbServiceAccountName = "smb-controller"
)

// EnsureSMBControllerResources deploys all required resources to enable Nodes on the cluster to mount SMB volumes
func EnsureSMBControllerResources(c client.Interface) error {
	if err := ensureCSIControllerNamespace(c); err != nil {
		return err
	}
	if err := ensureCSIDriver(c); err != nil {
		return err
	}
	if err := ensureSMBRBAC(c); err != nil {
		return err
	}
	if err := ensureCSIController(c); err != nil {
		return err
	}
	if err := ensureWindowsCSIDaemonset(c); err != nil {
		return err
	}
	return nil
}

// ensureCSIControllerNamespace ensures the controller namespace exists
func ensureCSIControllerNamespace(c client.Interface) error {
	_, err := c.CoreV1().Namespaces().Get(context.TODO(), smbNamespace, meta.GetOptions{})
	if err != nil {
		if !k8sapierrors.IsNotFound(err) {
			return err
		}
	} else if err == nil {
		return nil
	}
	ns := core.Namespace{
		ObjectMeta: meta.ObjectMeta{
			Name:   smbNamespace,
			Labels: map[string]string{"pod-security.kubernetes.io/enforce": "privileged"},
		},
	}
	_, err = c.CoreV1().Namespaces().Create(context.TODO(), &ns, meta.CreateOptions{})
	return err
}

// ensureCSIDriver ensures the CSIDriver exists in the correct state
func ensureCSIDriver(c client.Interface) error {
	isFalse := false
	isTrue := true
	desiredDriver := storage.CSIDriver{
		ObjectMeta: meta.ObjectMeta{Name: smbDriverName},
		Spec: storage.CSIDriverSpec{
			AttachRequired: &isFalse,
			PodInfoOnMount: &isTrue,
		},
	}
	driver, err := c.StorageV1().CSIDrivers().Get(context.TODO(), smbDriverName, meta.GetOptions{})
	if err != nil {
		if !k8sapierrors.IsNotFound(err) {
			return err
		}
	} else if err == nil {
		if reflect.DeepEqual(desiredDriver.Spec, driver.Spec) {
			// already as expected, nothing to do here.
			return nil
		}
		err = c.StorageV1().CSIDrivers().Delete(context.TODO(), smbDriverName, meta.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("error deleting smb driver: %w", err)
		}
	}
	_, err = c.StorageV1().CSIDrivers().Create(context.TODO(), &desiredDriver, meta.CreateOptions{})
	return err
}

// ensureCSIController ensures the smb-controller deployment exists in the correct state
func ensureCSIController(c client.Interface) error {
	isTrue := true
	controllerName := "smb-controller"
	controller := apps.Deployment{
		ObjectMeta: meta.ObjectMeta{
			Name: controllerName,
		},
		Spec: apps.DeploymentSpec{
			Selector: &meta.LabelSelector{
				MatchLabels: map[string]string{"app": controllerName},
			},
			Template: core.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{
					Labels: map[string]string{"app": controllerName},
				},
				Spec: core.PodSpec{
					Containers: []core.Container{
						{
							Args: []string{"-v=2", "--csi-address=$(ADDRESS)", "--leader-election", "--leader-election-namespace=kube-system", "--extra-create-metadata=true"},
							Env: []core.EnvVar{
								{Name: "ADDRESS", Value: "/csi/csi.sock"},
							},
							Image: "registry.k8s.io/sig-storage/csi-provisioner:v3.3.0",
							Name:  "csi-provisioner",
							SecurityContext: &core.SecurityContext{
								Privileged: &isTrue,
								SeccompProfile: &core.SeccompProfile{
									Type: core.SeccompProfileTypeRuntimeDefault,
								},
							},
							VolumeMounts: []core.VolumeMount{
								{
									MountPath: "/csi",
									Name:      "socket-dir",
								},
							},
						},
						{
							Args:  []string{"--csi-address=/csi/csi.sock", "--probe-timeout=3s", "--health-port=29642", "--v=2"},
							Name:  "liveness-probe",
							Image: "registry.k8s.io/sig-storage/livenessprobe:v2.8.0",
							SecurityContext: &core.SecurityContext{
								Privileged: &isTrue,
								SeccompProfile: &core.SeccompProfile{
									Type: core.SeccompProfileTypeRuntimeDefault,
								},
							},
							VolumeMounts: []core.VolumeMount{
								{
									MountPath: "/csi",
									Name:      "socket-dir",
								},
							},
						},
						{
							Args: []string{"--v=5", "--endpoint=$(CSI_ENDPOINT)", "--metrics-address=0.0.0.0:29644"},
							Env: []core.EnvVar{
								{
									Name:  "CSI_ENDPOINT",
									Value: "unix:///csi/csi.sock",
								},
							},
							Image: "registry.k8s.io/sig-storage/smbplugin:v1.10.0",
							LivenessProbe: &core.Probe{
								FailureThreshold: 5,
								ProbeHandler: core.ProbeHandler{
									Exec: nil,
									HTTPGet: &core.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.IntOrString{Type: intstr.String, StrVal: "healthz"}},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       30,
								TimeoutSeconds:      10,
							},
							Name: "smb",
							Ports: []core.ContainerPort{
								{
									ContainerPort: 29642,
									Name:          "healthz",
									Protocol:      "TCP",
								},
								{
									ContainerPort: 29644,
									Name:          "metrics",
									Protocol:      "TCP",
								},
							},
							SecurityContext: &core.SecurityContext{
								Privileged:     &isTrue,
								SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault},
							},
							VolumeMounts: []core.VolumeMount{
								{
									MountPath: "/csi",
									Name:      "socket-dir",
								},
							},
						},
					},
					PriorityClassName:  "system-cluster-critical",
					ServiceAccountName: smbServiceAccountName,
					Volumes: []core.Volume{
						{
							Name: "socket-dir",
							VolumeSource: core.VolumeSource{
								EmptyDir: &core.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}
	// See if the deployment already exists in the state we expect it to be in.
	existingController, err := c.AppsV1().Deployments(smbNamespace).Get(context.TODO(), controllerName, meta.GetOptions{})
	if err != nil {
		if !k8sapierrors.IsNotFound(err) {
			return fmt.Errorf("error getting existing SMB controller: %w", err)
		}
	} else if err == nil {
		if reflect.DeepEqual(existingController.Spec, controller.Spec) {
			// DaemonSet is already as expected, nothing to do here.
			return nil
		}
		// Delete the deployment as it has the wrong spec.
		err = c.AppsV1().Deployments(smbNamespace).Delete(context.TODO(), controllerName, meta.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("error deleting existing SMB controller: %w", err)

		}
	}
	_, err = c.AppsV1().Deployments(smbNamespace).Create(context.TODO(), &controller, meta.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating SMB controller: %w", err)
	}
	return nil
}

// ensureSMBServiceAccount ensures the controller's service account exists
func ensureSMBServiceAccount(c client.Interface) error {
	_, err := c.CoreV1().ServiceAccounts(smbNamespace).Get(context.TODO(), smbServiceAccountName, meta.GetOptions{})
	if err != nil {
		if !k8sapierrors.IsNotFound(err) {
			return err
		}
	} else if err == nil {
		return nil
	}
	sa := core.ServiceAccount{
		ObjectMeta: meta.ObjectMeta{
			Name: smbServiceAccountName,
		},
	}
	_, err = c.CoreV1().ServiceAccounts(smbNamespace).Create(context.TODO(), &sa, meta.CreateOptions{})
	return err
}

// ensureSMBClusterRole ensures the controller's ClusterRole exists with the correct permissions
func ensureSMBClusterRole(c client.Interface) error {
	clusterRole := rbac.ClusterRole{
		ObjectMeta: meta.ObjectMeta{
			Name: smbServiceAccountName,
		},
		Rules: []rbac.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"persistentvolumes"},
				Verbs:     []string{"get", "list", "watch", "create", "delete"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"persistentvolumeclaims"},
				Verbs:     []string{"get", "list", "watch", "update"},
			},
			{
				APIGroups: []string{"storage.k8s.io"},
				Resources: []string{"storageclasses"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch"},
			},
			{
				APIGroups: []string{"storage.k8s.io"},
				Resources: []string{"csinodes"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"coordination.k8s.io"},
				Resources: []string{"leases"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"secrets"},
				Verbs:     []string{"get"},
			},
			{
				APIGroups:     []string{"security.openshift.io"},
				ResourceNames: []string{"privileged"},
				Resources:     []string{"securitycontextconstraints"},
				Verbs:         []string{"use"},
			},
		},
	}
	// See if the CR already exists in the state we expect it to be in.
	existingClusterRole, err := c.RbacV1().ClusterRoles().Get(context.TODO(), smbServiceAccountName, meta.GetOptions{})
	if err != nil {
		if !k8sapierrors.IsNotFound(err) {
			return fmt.Errorf("error getting existing SMB CR: %w", err)
		}
	} else if err == nil {
		if reflect.DeepEqual(existingClusterRole.Rules, clusterRole.Rules) {
			// DaemonSet is already as expected, nothing to do here.
			return nil
		}
		// Delete the deployment as it has the wrong spec.
		err = c.RbacV1().ClusterRoles().Delete(context.TODO(), smbServiceAccountName, meta.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("error deleting existing SMB CR: %w", err)

		}
	}
	_, err = c.RbacV1().ClusterRoles().Create(context.TODO(), &clusterRole, meta.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating SMB CR: %w", err)
	}
	return nil
}

// ensureSMBClusterRoleBinding ensures the controller's ClusterRoleBinding exists with the correct Subject and RoleRef
func ensureSMBClusterRoleBinding(c client.Interface) error {
	clusterRoleBinding := rbac.ClusterRoleBinding{
		ObjectMeta: meta.ObjectMeta{
			Name: smbServiceAccountName,
		},
		Subjects: []rbac.Subject{
			{
				Kind:      rbac.ServiceAccountKind,
				Name:      smbServiceAccountName,
				Namespace: smbNamespace,
			},
		},
		RoleRef: rbac.RoleRef{
			APIGroup: rbac.GroupName,
			Kind:     "ClusterRole",
			Name:     smbServiceAccountName,
		},
	}
	// See if the CRB already exists in the state we expect it to be in.
	existingClusterRoleBinding, err := c.RbacV1().ClusterRoleBindings().Get(context.TODO(), smbServiceAccountName, meta.GetOptions{})
	if err != nil {
		if !k8sapierrors.IsNotFound(err) {
			return fmt.Errorf("error getting existing SMB CRB: %w", err)
		}
	} else if err == nil {
		if reflect.DeepEqual(existingClusterRoleBinding.Subjects, clusterRoleBinding.Subjects) &&
			reflect.DeepEqual(existingClusterRoleBinding.RoleRef, clusterRoleBinding.RoleRef) {
			// DaemonSet is already as expected, nothing to do here.
			return nil
		}
		// Delete the deployment as it has the wrong spec.
		err = c.RbacV1().ClusterRoles().Delete(context.TODO(), smbServiceAccountName, meta.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("error deleting existing SMB CR: %w", err)

		}
	}
	_, err = c.RbacV1().ClusterRoleBindings().Create(context.TODO(), &clusterRoleBinding, meta.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating SMB CR: %w", err)
	}
	return nil
}

// ensureSMBRBAC ensures all the required RBAC resources exist
func ensureSMBRBAC(c client.Interface) error {
	if err := ensureSMBServiceAccount(c); err != nil {
		return err
	}
	if err := ensureSMBClusterRole(c); err != nil {
		return err
	}
	if err := ensureSMBClusterRoleBinding(c); err != nil {
		return err
	}
	return nil
}

// ensureWindowsCSIDaemonset ensures the Windows CSI node DaemonSet exists with the correct spec
func ensureWindowsCSIDaemonset(c client.Interface) error {
	dsName := "csi-smb-node-win"
	directoryType := core.HostPathDirectory
	directoryOrCreate := core.HostPathDirectoryOrCreate
	ds := apps.DaemonSet{
		ObjectMeta: meta.ObjectMeta{
			Name: dsName,
		},
		Spec: apps.DaemonSetSpec{
			Selector: &meta.LabelSelector{MatchLabels: map[string]string{"app": dsName}},
			Template: core.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{
					Labels: map[string]string{"app": dsName},
				},
				Spec: core.PodSpec{
					PriorityClassName:  "system-node-critical",
					NodeSelector:       map[string]string{core.LabelOSStable: "windows"},
					ServiceAccountName: smbServiceAccountName,
					OS:                 &core.PodOS{Name: core.Windows},
					Tolerations: []core.Toleration{
						{
							Key:    "os",
							Value:  "Windows",
							Effect: core.TaintEffectNoSchedule,
						},
					},
					Containers: []core.Container{
						{
							Name:  "node-driver-registrar",
							Image: "k8s.gcr.io/sig-storage/csi-node-driver-registrar:v2.7.0",
							Args:  []string{"--v=5", "--csi-address=$(ADDRESS)", "-kubelet-registration-path=$(DRIVER_REG_SOCK_PATH)"},
							Env: []core.EnvVar{
								{
									Name:  "ADDRESS",
									Value: `unix://C:\\csi\\csi.sock`,
								},
								{
									Name:  "DRIVER_REG_SOCK_PATH",
									Value: `C:\\var\\lib\\kubelet\\plugins\\smb.csi.k8s.io\\csi.sock`,
								},
							},
							VolumeMounts: []core.VolumeMount{
								{
									Name:      "plugin-dir",
									MountPath: "/csi",
								},
								{
									Name:      "registration-dir",
									MountPath: "/registration",
								},
							},
						},
						{
							Name:  "smb-csi-node",
							Image: "registry.k8s.io/sig-storage/smbplugin:v1.10.0",
							Args:  []string{"--endpoint=$(CSI_ENDPOINT)", "--nodeid=$(NODE_NAME)", "--remove-smb-mapping-during-unmount=true"},
							Env: []core.EnvVar{
								{
									Name: "NODE_NAME",
									ValueFrom: &core.EnvVarSource{
										FieldRef: &core.ObjectFieldSelector{
											APIVersion: "v1",
											FieldPath:  "spec.nodeName",
										},
									},
								},
								{
									Name:  "CSI_ENDPOINT",
									Value: `unix://C:\\csi\\csi.sock`,
								},
							},
							VolumeMounts: []core.VolumeMount{
								{
									Name:      "plugin-dir",
									MountPath: "/csi",
								},
								{
									Name:      "pod-mount-dir",
									MountPath: "/var/lib/kubelet",
								},
								{
									Name:      "csi-proxy-smb-pipe-v1",
									MountPath: `\\.\pipe\csi-proxy-smb-v1`,
								},
								{
									Name:      "csi-proxy-filesystem-v1",
									MountPath: `\\.\pipe\csi-proxy-filesystem-v1`,
								},
							},
						},
					},
					Volumes: []core.Volume{
						{
							Name: "registration-dir",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `C:\var\lib\kubelet\plugins_registry\`,
									Type: &directoryType,
								},
							},
						},
						{
							Name: "plugin-dir",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `C:\var\lib\kubelet\plugins\smb.csi.k8s.io\`,
									Type: &directoryOrCreate,
								},
							},
						},
						{
							Name: "pod-mount-dir",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `C:\var\lib\kubelet`,
									Type: &directoryType,
								},
							},
						},
						{
							Name: "csi-proxy-smb-pipe-v1",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `\\.\pipe\csi-proxy-smb-v1`,
								},
							},
						},
						{
							Name: "csi-proxy-filesystem-v1",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `\\.\pipe\csi-proxy-filesystem-v1`,
								},
							},
						},
					},
				},
			},
		},
	}
	// See if the DaemonSet already exists in the state we expect it to be in.
	existingDS, err := c.AppsV1().DaemonSets(smbNamespace).Get(context.TODO(), dsName, meta.GetOptions{})
	if err != nil {
		if !k8sapierrors.IsNotFound(err) {
			return fmt.Errorf("error getting existing Windows CSI DaemonSet: %w", err)
		}
	} else if err == nil {
		if reflect.DeepEqual(existingDS.Spec, ds.Spec) {
			// DaemonSet is already as expected, nothing to do here.
			return nil
		}
		// Delete the DaemonSet as it has the wrong spec.
		err = c.AppsV1().DaemonSets(smbNamespace).Delete(context.TODO(), dsName, meta.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("error deleting existing Windows CSI DaemonSet: %w", err)

		}
	}
	_, err = c.AppsV1().DaemonSets(smbNamespace).Create(context.TODO(), &ds, meta.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating Windows CSI DaemonSet: %w", err)
	}
	return nil
}
