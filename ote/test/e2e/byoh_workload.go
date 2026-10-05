package winc

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

const byohWorkloadOwnershipAnnotation = "windowsmachineconfig.openshift.io/test-workload-owner"

func newBYOHWorkloadOwnershipNonce() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate BYOH workload ownership nonce: %w", err)
	}
	return fmt.Sprintf("%x", value), nil
}

func workloadObjectHasOwnership(meta metav1.Object, nonce string) bool {
	return meta != nil && nonce != "" && meta.GetAnnotations()[byohWorkloadOwnershipAnnotation] == nonce
}

func newBYOHWorkloadDeployment(name, namespace string, replicas int32, nodeNames []string) *appsv1.Deployment {
	containerUser := `ContainerUser`
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace,
		Labels: map[string]string{"app": name}}, Spec: appsv1.DeploymentSpec{
		Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
			Spec: corev1.PodSpec{
				Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name",
							Operator: corev1.NodeSelectorOpIn, Values: append([]string(nil), nodeNames...)}}}}}}},
				NodeSelector: map[string]string{"kubernetes.io/os": "windows"},
				Tolerations: []corev1.Toleration{{Key: "os", Operator: corev1.TolerationOpEqual,
					Value: "Windows", Effect: corev1.TaintEffectNoSchedule}},
				Containers: []corev1.Container{{Name: "readiness", Image: windowsDebugImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					SecurityContext: &corev1.SecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{
						RunAsUserName: &containerUser}},
					Command: []string{"pwsh.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command",
						`while ($true) { Start-Sleep -Seconds 30 }`}}}}}}}
}

func (f *byohTestFixture) createBYOHWebServer(namespace string, leases []*byohHostLease) error {
	if err := distinctLeaseNodes(leases); err != nil {
		return fmt.Errorf("validate selected workload Node identities: %w", err)
	}
	nonce, err := newBYOHWorkloadOwnershipNonce()
	if err != nil {
		return err
	}
	f.workloadNS, f.workloadOwnershipNonce = namespace, nonce
	attemptedNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace,
		Labels:      map[string]string{"type": "byoh-node"},
		Annotations: map[string]string{byohWorkloadOwnershipAnnotation: nonce}}}
	namespaceObject, createErr := f.coreClient.Namespaces().Create(f.ctx, attemptedNamespace, metav1.CreateOptions{})
	if createErr != nil {
		reconcileCtx, cancel := detachedContextWithin(f.ctx, byohHostProcessCleanup)
		current, getErr := f.coreClient.Namespaces().Get(reconcileCtx, namespace, metav1.GetOptions{})
		cancel()
		if getErr != nil || current.UID == "" || !workloadObjectHasOwnership(current, nonce) {
			return errors.Join(fmt.Errorf("create isolated BYOH workload namespace: %w", createErr),
				wrapOptionalError("reconcile ambiguous workload Namespace creation", getErr))
		}
		f.workloadNSUID = current.UID
		return fmt.Errorf("create isolated BYOH workload namespace returned an ambiguous error: %w", createErr)
	}
	if namespaceObject.UID == "" || !workloadObjectHasOwnership(namespaceObject, nonce) {
		return fmt.Errorf("created BYOH workload namespace has incomplete ownership identity")
	}
	f.workloadNSUID = namespaceObject.UID
	nodes := make([]string, 0, len(leases))
	for _, lease := range leases {
		if lease.NodeName == "" {
			return fmt.Errorf("cannot constrain workload to an unidentified BYOH Node")
		}
		nodes = append(nodes, lease.NodeName)
	}
	attemptedDeployment := newBYOHWorkloadDeployment(byohWorkloadDeployment, namespace, 5, nodes)
	attemptedDeployment.Annotations = map[string]string{byohWorkloadOwnershipAnnotation: nonce}
	f.workloadDeploymentAttempted = true
	deployment, createErr := f.appsClient.Deployments(namespace).Create(f.ctx, attemptedDeployment,
		metav1.CreateOptions{})
	if createErr != nil {
		reconcileCtx, cancel := detachedContextWithin(f.ctx, byohHostProcessCleanup)
		current, getErr := f.appsClient.Deployments(namespace).Get(reconcileCtx, byohWorkloadDeployment,
			metav1.GetOptions{})
		cancel()
		if getErr != nil || current.UID == "" || !workloadObjectHasOwnership(current, nonce) {
			return errors.Join(fmt.Errorf("create typed BYOH readiness Deployment: %w", createErr),
				wrapOptionalError("reconcile ambiguous workload Deployment creation", getErr))
		}
		f.workloadDeploymentUID = current.UID
		return fmt.Errorf("create typed BYOH readiness Deployment returned an ambiguous error: %w", createErr)
	}
	if deployment.UID == "" || !workloadObjectHasOwnership(deployment, nonce) {
		return fmt.Errorf("created BYOH readiness Deployment has incomplete ownership identity")
	}
	f.workloadDeploymentUID = deployment.UID
	return f.waitForBYOHWebServer(leases)
}

func (f *byohTestFixture) waitForBYOHWebServer(leases []*byohHostLease) error {
	return wait.PollUntilContextTimeout(f.ctx, 10*time.Second, 15*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			namespace, err := f.coreClient.Namespaces().Get(ctx, f.workloadNS, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if f.workloadNSUID == "" || namespace.UID != f.workloadNSUID ||
				!workloadObjectHasOwnership(namespace, f.workloadOwnershipNonce) {
				return false, fmt.Errorf("BYOH workload Namespace identity changed during readiness")
			}
			deployment, err := f.appsClient.Deployments(f.workloadNS).Get(ctx, byohWorkloadDeployment,
				metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if f.workloadDeploymentUID == "" || deployment.UID != f.workloadDeploymentUID ||
				!workloadObjectHasOwnership(deployment, f.workloadOwnershipNonce) {
				return false, fmt.Errorf("BYOH workload Deployment identity changed during readiness")
			}
			replicaSets, err := f.appsClient.ReplicaSets(f.workloadNS).List(ctx,
				metav1.ListOptions{LabelSelector: "app=" + byohWorkloadDeployment})
			if err != nil {
				return false, err
			}
			currentReplicaSetUID, found := currentDeploymentReplicaSetUID(deployment, replicaSets.Items)
			if !found {
				return false, nil
			}
			pods, err := f.coreClient.Pods(f.workloadNS).List(ctx,
				metav1.ListOptions{LabelSelector: "app=" + byohWorkloadDeployment})
			if err != nil {
				return false, err
			}
			return currentBYOHWorkloadReady(ctx, f.coreClient.Nodes(), pods.Items, currentReplicaSetUID, leases, 5)
		})
}

func currentDeploymentReplicaSetUID(deployment *appsv1.Deployment, replicaSets []appsv1.ReplicaSet) (types.UID, bool) {
	if deployment == nil {
		return "", false
	}
	revision := deployment.Annotations["deployment.kubernetes.io/revision"]
	if revision == "" {
		return "", false
	}
	wantedTemplate := deployment.Spec.Template.DeepCopy()
	if wantedTemplate.Labels != nil {
		delete(wantedTemplate.Labels, "pod-template-hash")
	}
	var current types.UID
	for i := range replicaSets {
		replicaSet := &replicaSets[i]
		if !controlledByUID(replicaSet.OwnerReferences, deployment.UID) ||
			replicaSet.Annotations["deployment.kubernetes.io/revision"] != revision {
			continue
		}
		template := replicaSet.Spec.Template.DeepCopy()
		if template.Labels != nil {
			delete(template.Labels, "pod-template-hash")
		}
		if !reflect.DeepEqual(wantedTemplate, template) || current != "" {
			return "", false
		}
		current = replicaSet.UID
	}
	return current, current != ""
}

func controlledByUID(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range refs {
		if ref.UID == uid && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

func podIsReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func currentBYOHWorkloadReady(ctx context.Context, nodes corev1client.NodeInterface, pods []corev1.Pod,
	replicaSetUID types.UID, leases []*byohHostLease, replicas int) (bool, error) {
	allowed := map[string]types.UID{}
	for _, lease := range leases {
		allowed[lease.NodeName] = lease.NodeUID
	}
	current := make([]*corev1.Pod, 0, replicas)
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || !podIsReady(pod) {
			continue
		}
		if controlledByUID(pod.OwnerReferences, replicaSetUID) {
			current = append(current, pod)
		}
	}
	if len(current) != replicas {
		return false, nil
	}
	for _, pod := range current {
		expectedUID, ok := allowed[pod.Spec.NodeName]
		if !ok {
			return false, fmt.Errorf("BYOH workload escaped the selected physical hosts")
		}
		node, err := nodes.Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if node.UID != expectedUID {
			return false, fmt.Errorf("BYOH workload was placed on a replaced Node identity")
		}
	}
	return true, nil
}

func (f *byohTestFixture) deleteWorkload(ctx context.Context) error {
	if f.workloadNS == "" {
		return nil
	}
	if f.workloadDeploymentAttempted {
		if f.workloadDeploymentUID == "" {
			return fmt.Errorf("refusing Namespace cleanup with uncertain BYOH workload Deployment ownership")
		}
		deployments := f.appsClient.Deployments(f.workloadNS)
		deployment, err := deployments.Get(ctx, byohWorkloadDeployment, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("read retained BYOH workload Deployment: %w", err)
		}
		if err == nil {
			if deployment.UID != f.workloadDeploymentUID ||
				!workloadObjectHasOwnership(deployment, f.workloadOwnershipNonce) {
				return fmt.Errorf("refusing to delete replacement BYOH workload Deployment")
			}
			uid, resourceVersion := deployment.UID, deployment.ResourceVersion
			deleteErr := deployments.Delete(ctx, byohWorkloadDeployment, metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}})
			if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				live, getErr := deployments.Get(ctx, byohWorkloadDeployment, metav1.GetOptions{})
				switch {
				case apierrors.IsNotFound(getErr):
					// The UID-preconditioned delete was applied and only its response was lost.
				case getErr != nil:
					return errors.Join(fmt.Errorf("delete retained BYOH workload Deployment: %w", deleteErr),
						fmt.Errorf("reconcile ambiguous BYOH workload Deployment deletion: %w", getErr))
				case live.UID != f.workloadDeploymentUID ||
					!workloadObjectHasOwnership(live, f.workloadOwnershipNonce):
					return fmt.Errorf("refusing Namespace cleanup after BYOH workload Deployment replacement")
				default:
					return fmt.Errorf("delete retained BYOH workload Deployment was not applied: %w", deleteErr)
				}
			}
		}
		if err := wait.PollUntilContextTimeout(ctx, time.Second, 10*time.Minute, true,
			func(ctx context.Context) (bool, error) {
				live, err := deployments.Get(ctx, byohWorkloadDeployment, metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				if err != nil {
					return false, err
				}
				if live.UID != f.workloadDeploymentUID ||
					!workloadObjectHasOwnership(live, f.workloadOwnershipNonce) {
					return false, fmt.Errorf("BYOH workload Deployment was replaced while awaiting deletion")
				}
				return false, nil
			}); err != nil {
			return fmt.Errorf("wait for retained BYOH workload Deployment deletion: %w", err)
		}
		f.workloadDeploymentAttempted, f.workloadDeploymentUID = false, ""
	}
	current, err := f.coreClient.Namespaces().Get(ctx, f.workloadNS, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		f.workloadNS, f.workloadNSUID, f.workloadDeploymentUID, f.workloadOwnershipNonce = "", "", "", ""
		f.workloadDeploymentAttempted = false
		return nil
	}
	if err != nil {
		return fmt.Errorf("read retained BYOH workload namespace: %w", err)
	}
	if f.workloadNSUID == "" || current.UID != f.workloadNSUID ||
		!workloadObjectHasOwnership(current, f.workloadOwnershipNonce) {
		return fmt.Errorf("refusing to delete replacement BYOH workload namespace")
	}
	uid, resourceVersion := current.UID, current.ResourceVersion
	if err := f.coreClient.Namespaces().Delete(ctx, f.workloadNS, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
		UID: &uid, ResourceVersion: &resourceVersion}}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete retained BYOH workload namespace: %w", err)
	}
	err = wait.PollUntilContextTimeout(ctx, time.Second, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := f.coreClient.Namespaces().Get(ctx, f.workloadNS, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	if err != nil {
		return fmt.Errorf("wait for retained BYOH workload namespace deletion: %w", err)
	}
	f.workloadNS, f.workloadNSUID, f.workloadDeploymentUID, f.workloadOwnershipNonce = "", "", "", ""
	f.workloadDeploymentAttempted = false
	return nil
}
