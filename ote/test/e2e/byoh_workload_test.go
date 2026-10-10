package winc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestBYOHReadinessDeploymentIsNonAdministratorAndExactlyConstrained(t *testing.T) {
	nodes := []string{"node-a", "node-b"}
	deployment := newBYOHWorkloadDeployment("readiness", "fixture", 5, nodes)
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 5 {
		t.Fatalf("readiness Deployment replicas = %v, want 5", deployment.Spec.Replicas)
	}
	termb := deployment.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(termb) != 1 || len(termb[0].MatchFields) != 1 || termb[0].MatchFields[0].Key != "metadata.name" ||
		!reflect.DeepEqual(termb[0].MatchFields[0].Values, nodes) {
		t.Fatalf("readiness Deployment is not constrained to exact selected Node names: %#v", termb)
	}
	podSpec := deployment.Spec.Template.Spec
	if podSpec.NodeSelector["kubernetes.io/os"] != "windows" || len(podSpec.Tolerations) != 1 {
		t.Fatalf("readiness Deployment omitted Windows selector/toleration: %#v", podSpec)
	}
	if podSpec.SecurityContext != nil && podSpec.SecurityContext.WindowsOptions != nil &&
		podSpec.SecurityContext.WindowsOptions.HostProcess != nil && *podSpec.SecurityContext.WindowsOptions.HostProcess {
		t.Fatal("ordinary readiness workload unexpectedly enables HostProcess")
	}
	if len(podSpec.Containers) != 1 || podSpec.Containers[0].SecurityContext == nil ||
		podSpec.Containers[0].SecurityContext.WindowsOptions == nil ||
		podSpec.Containers[0].SecurityContext.WindowsOptions.RunAsUserName == nil ||
		*podSpec.Containers[0].SecurityContext.WindowsOptions.RunAsUserName != "ContainerUser" {
		t.Fatalf("readiness workload is not explicitly non-administrator: %#v", podSpec.Containers)
	}
	command := strings.Join(podSpec.Containers[0].Command, " ")
	if strings.Contains(command, "HttpListener") || strings.Contains(command, "ContainerAdministrator") {
		t.Fatalf("readiness workload retains administrator listener behavior: %s", command)
	}
}

func TestBYOHWorkloadNamespaceUIDReplacementProtection(t *testing.T) {
	replacement := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "fixture", UID: "replacement-uid",
		ResourceVersion: "2"}}
	client := fake.NewSimpleClientset(replacement)
	fixture := &byohTestFixture{coreClient: client.CoreV1(), workloadNS: replacement.Name,
		workloadNSUID: "original-uid"}
	if err := fixture.deleteWorkload(context.Background()); err == nil {
		t.Fatal("same-name replacement Namespace was accepted as test-owned")
	}
	if _, err := client.CoreV1().Namespaces().Get(context.Background(), replacement.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("replacement Namespace was deleted: %v", err)
	}
}

func TestBYOHWorkloadCleanupDeletesRetainedDeploymentBeforeNamespace(t *testing.T) {
	const namespace = "fixture"
	const nonce = "owned-nonce"
	newOwned := func() (*corev1.Namespace, *appsv1.Deployment) {
		ownedNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace,
			UID: "namespace-uid", ResourceVersion: "namespace-rv",
			Annotations: map[string]string{byohWorkloadOwnershipAnnotation: nonce}}}
		ownedDeployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: byohWorkloadDeployment,
			Namespace: namespace, UID: "deployment-uid", ResourceVersion: "deployment-rv",
			Annotations: map[string]string{byohWorkloadOwnershipAnnotation: nonce}}}
		return ownedNamespace, ownedDeployment
	}
	newFixture := func(client *fake.Clientset) *byohTestFixture {
		return &byohTestFixture{coreClient: client.CoreV1(), appsClient: client.AppsV1(), workloadNS: namespace,
			workloadNSUID: "namespace-uid", workloadDeploymentAttempted: true,
			workloadDeploymentUID: "deployment-uid", workloadOwnershipNonce: nonce}
	}

	t.Run("exact UID and latest resourceVersion delete in order", func(t *testing.T) {
		ownedNamespace, ownedDeployment := newOwned()
		client := fake.NewSimpleClientset(ownedNamespace, ownedDeployment)
		order := []string{}
		client.PrependReactor("delete", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
			options := action.(clienttesting.DeleteAction).GetDeleteOptions()
			if options.Preconditions == nil || options.Preconditions.UID == nil ||
				*options.Preconditions.UID != ownedDeployment.UID || options.Preconditions.ResourceVersion == nil ||
				*options.Preconditions.ResourceVersion != ownedDeployment.ResourceVersion {
				t.Fatalf("Deployment delete omitted exact UID/latest-RV preconditions: %#v", options.Preconditions)
			}
			order = append(order, "deployment")
			return false, nil, nil
		})
		client.PrependReactor("delete", "namespaces", func(action clienttesting.Action) (bool, runtime.Object, error) {
			options := action.(clienttesting.DeleteAction).GetDeleteOptions()
			if options.Preconditions == nil || options.Preconditions.UID == nil ||
				*options.Preconditions.UID != ownedNamespace.UID || options.Preconditions.ResourceVersion == nil ||
				*options.Preconditions.ResourceVersion != ownedNamespace.ResourceVersion {
				t.Fatalf("Namespace delete omitted exact UID/latest-RV preconditions: %#v", options.Preconditions)
			}
			order = append(order, "namespace")
			return false, nil, nil
		})
		if err := newFixture(client).deleteWorkload(context.Background()); err != nil {
			t.Fatalf("owned workload cleanup failed: %v", err)
		}
		if !reflect.DeepEqual(order, []string{"deployment", "namespace"}) {
			t.Fatalf("workload deletion order was not Deployment then Namespace: %v", order)
		}
	})

	t.Run("already absent owned Deployment allows guarded Namespace cleanup", func(t *testing.T) {
		ownedNamespace, _ := newOwned()
		client := fake.NewSimpleClientset(ownedNamespace)
		order := []string{}
		client.PrependReactor("delete", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
			order = append(order, "namespace")
			return false, nil, nil
		})
		if err := newFixture(client).deleteWorkload(context.Background()); err != nil {
			t.Fatalf("cleanup rejected an already absent retained Deployment: %v", err)
		}
		if !reflect.DeepEqual(order, []string{"namespace"}) {
			t.Fatalf("unexpected cleanup actions for absent Deployment: %v", order)
		}
	})

	t.Run("same-name replacement is preserved with no Namespace delete", func(t *testing.T) {
		ownedNamespace, replacement := newOwned()
		replacement.UID = "replacement-uid"
		replacement.ResourceVersion = "replacement-rv"
		client := fake.NewSimpleClientset(ownedNamespace, replacement)
		deletes := 0
		client.PrependReactor("delete", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
			deletes++
			return false, nil, nil
		})
		if err := newFixture(client).deleteWorkload(context.Background()); err == nil {
			t.Fatal("replacement Deployment ownership was accepted")
		}
		if deletes != 0 {
			t.Fatalf("replacement cleanup issued %d delete requests", deletes)
		}
		if _, err := client.AppsV1().Deployments(namespace).Get(context.Background(), byohWorkloadDeployment,
			metav1.GetOptions{}); err != nil {
			t.Fatalf("replacement Deployment was not preserved: %v", err)
		}
	})

	t.Run("applied delete response loss is reconciled before Namespace cleanup", func(t *testing.T) {
		ownedNamespace, ownedDeployment := newOwned()
		client := fake.NewSimpleClientset(ownedNamespace, ownedDeployment)
		deploymentResource := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		order := []string{}
		client.PrependReactor("delete", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
			order = append(order, "deployment-response-lost")
			if err := client.Tracker().Delete(deploymentResource, namespace, byohWorkloadDeployment); err != nil {
				t.Fatal(err)
			}
			return true, nil, errors.New("simulated applied Deployment delete response loss")
		})
		client.PrependReactor("delete", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
			order = append(order, "namespace")
			return false, nil, nil
		})
		if err := newFixture(client).deleteWorkload(context.Background()); err != nil {
			t.Fatalf("applied Deployment delete response loss was not reconciled: %v", err)
		}
		if !reflect.DeepEqual(order, []string{"deployment-response-lost", "namespace"}) {
			t.Fatalf("response-loss cleanup order mismatch: %v", order)
		}
	})

	t.Run("ambiguous delete replacement is preserved and Namespace is untouched", func(t *testing.T) {
		ownedNamespace, ownedDeployment := newOwned()
		client := fake.NewSimpleClientset(ownedNamespace, ownedDeployment)
		deploymentResource := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		namespaceDeletes := 0
		client.PrependReactor("delete", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
			if err := client.Tracker().Delete(deploymentResource, namespace, byohWorkloadDeployment); err != nil {
				t.Fatal(err)
			}
			replacement := ownedDeployment.DeepCopy()
			replacement.UID, replacement.ResourceVersion = "replacement-uid", "replacement-rv"
			if err := client.Tracker().Create(deploymentResource, replacement, namespace); err != nil {
				t.Fatal(err)
			}
			return true, nil, errors.New("ambiguous Deployment delete response")
		})
		client.PrependReactor("delete", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
			namespaceDeletes++
			return false, nil, nil
		})
		if err := newFixture(client).deleteWorkload(context.Background()); err == nil {
			t.Fatal("ambiguous replacement after Deployment delete was accepted")
		}
		if namespaceDeletes != 0 {
			t.Fatalf("Namespace was deleted %d times after replacement ambiguity", namespaceDeletes)
		}
		live, err := client.AppsV1().Deployments(namespace).Get(context.Background(), byohWorkloadDeployment,
			metav1.GetOptions{})
		if err != nil || live.UID != "replacement-uid" {
			t.Fatalf("foreign replacement was not preserved: live=%#v err=%v", live, err)
		}
	})
}

func TestCurrentBYOHWorkloadPlacementUsesOwnersReadinessAndUIDs(t *testing.T) {
	controller := true
	node := readyBYOHNode("node-a", "uid-a", "192.0.2.10")
	lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID}
	readyPod := func(name string) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{{
			UID: "rs-current", Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: node.Name},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady,
				Status: corev1.ConditionTrue}}}}
	}
	pods := []corev1.Pod{}
	for i := 0; i < 5; i++ {
		pods = append(pods, readyPod(fmt.Sprintf("current-%d", i)))
	}
	terminating := readyPod("terminating-old")
	deletion := metav1.Now()
	terminating.DeletionTimestamp = &deletion
	pods = append(pods, terminating, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foreign",
		OwnerReferences: []metav1.OwnerReference{{UID: "rs-old", Controller: &controller}}}})
	ready, err := currentBYOHWorkloadReady(context.Background(), fake.NewSimpleClientset(node).CoreV1().Nodes(),
		pods, "rs-current", []*byohHostLease{lease}, 5)
	if err != nil || !ready {
		t.Fatalf("five current Ready owned Pods were not accepted: ready=%t err=%v", ready, err)
	}
	replaced := node.DeepCopy()
	replaced.UID = "uid-replaced"
	ready, err = currentBYOHWorkloadReady(context.Background(), fake.NewSimpleClientset(replaced).CoreV1().Nodes(),
		pods, "rs-current", []*byohHostLease{lease}, 5)
	if err == nil || ready {
		t.Fatal("Pods on a replaced Node UID were accepted")
	}
}

func TestCurrentReplicaSetAndReadyOldPodsAreExcluded(t *testing.T) {
	controller := true
	template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "win-webserver"}}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "win-webserver", UID: "deployment-uid",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}},
		Spec: appsv1.DeploymentSpec{Template: template}}
	currentTemplate := template.DeepCopy()
	currentTemplate.Labels["pod-template-hash"] = "current"
	replicaSets := []appsv1.ReplicaSet{
		{ObjectMeta: metav1.ObjectMeta{Name: "old", UID: "rs-old",
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "1"},
			OwnerReferences: []metav1.OwnerReference{{UID: deployment.UID, Controller: &controller}}},
			Spec: appsv1.ReplicaSetSpec{Template: *currentTemplate}},
		{ObjectMeta: metav1.ObjectMeta{Name: "current", UID: "rs-current",
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "2"},
			OwnerReferences: []metav1.OwnerReference{{UID: deployment.UID, Controller: &controller}}},
			Spec: appsv1.ReplicaSetSpec{Template: *currentTemplate}},
	}
	uid, found := currentDeploymentReplicaSetUID(deployment, replicaSets)
	if !found || uid != "rs-current" {
		t.Fatalf("current ReplicaSet was not uniquely verified: uid=%q found=%t", uid, found)
	}
	node := readyBYOHNode("node-a", "uid-a", "192.0.2.10")
	lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID}
	readyPod := func(name string, owner types.UID) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{{
			UID: owner, Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: node.Name},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady,
				Status: corev1.ConditionTrue}}}}
	}
	pods := []corev1.Pod{readyPod("current-0", uid), readyPod("current-1", uid),
		readyPod("current-2", uid), readyPod("current-3", uid), readyPod("ready-old", "rs-old")}
	ready, err := currentBYOHWorkloadReady(context.Background(), fake.NewSimpleClientset(node).CoreV1().Nodes(),
		pods, uid, []*byohHostLease{lease}, 5)
	if err != nil || ready {
		t.Fatalf("Ready old-ReplicaSet Pod satisfied current replica count: ready=%t err=%v", ready, err)
	}
}

func TestBYOHWorkloadNamespaceAndDeploymentCreateAmbiguity(t *testing.T) {
	lease := &byohHostLease{NodeName: "node-a", NodeUID: "node-uid"}
	resource := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	for _, test := range []struct {
		name       string
		apply      bool
		concurrent bool
		owned      bool
	}{
		{name: "applied response loss", apply: true, owned: true},
		{name: "unapplied response loss"},
		{name: "concurrent creator", apply: true, concurrent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor("create", "namespaces", func(action clienttesting.Action) (bool, runtime.Object, error) {
				if test.apply {
					namespace := action.(clienttesting.CreateAction).GetObject().(*corev1.Namespace).DeepCopy()
					namespace.UID, namespace.ResourceVersion = "namespace-uid", "1"
					if test.concurrent {
						namespace.Annotations[byohWorkloadOwnershipAnnotation] = "other-owner"
					}
					if err := client.Tracker().Create(resource, namespace, ""); err != nil {
						t.Fatal(err)
					}
				}
				return true, nil, errors.New("namespace create response lost")
			})
			fixture := &byohTestFixture{coreClient: client.CoreV1(), appsClient: client.AppsV1(), ctx: context.Background()}
			err := fixture.createBYOHWebServer("fixture", []*byohHostLease{lease})
			if err == nil || (fixture.workloadNSUID != "") != test.owned {
				t.Fatalf("Namespace ambiguity ownership mismatch: uid=%q owned=%t err=%v",
					fixture.workloadNSUID, test.owned, err)
			}
			if test.concurrent {
				if err := fixture.deleteWorkload(context.Background()); err == nil {
					t.Fatal("cleanup accepted a concurrent same-name Namespace")
				}
			}
		})
	}

	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "namespaces", func(action clienttesting.Action) (bool, runtime.Object, error) {
		namespace := action.(clienttesting.CreateAction).GetObject().(*corev1.Namespace).DeepCopy()
		namespace.UID, namespace.ResourceVersion = "namespace-uid", "1"
		if err := client.Tracker().Create(resource, namespace, ""); err != nil {
			t.Fatal(err)
		}
		return true, namespace, nil
	})
	deploymentResource := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	client.PrependReactor("create", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		deployment := action.(clienttesting.CreateAction).GetObject().(*appsv1.Deployment).DeepCopy()
		deployment.UID, deployment.ResourceVersion = "deployment-uid", "1"
		if err := client.Tracker().Create(deploymentResource, deployment, "fixture"); err != nil {
			t.Fatal(err)
		}
		return true, nil, errors.New("deployment create response lost")
	})
	fixture := &byohTestFixture{coreClient: client.CoreV1(), appsClient: client.AppsV1(), ctx: context.Background()}
	err := fixture.createBYOHWebServer("fixture", []*byohHostLease{lease})
	if err == nil || fixture.workloadDeploymentUID != "deployment-uid" {
		t.Fatalf("applied Deployment response loss was not retained: uid=%q err=%v",
			fixture.workloadDeploymentUID, err)
	}
}

func TestBYOHWorkloadReadinessRejectsNamespaceAndDeploymentReplacement(t *testing.T) {
	nonce := "owned-nonce"
	ownedNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "fixture", UID: "namespace-uid",
		Annotations: map[string]string{byohWorkloadOwnershipAnnotation: nonce}}}
	ownedDeployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: byohWorkloadDeployment,
		Namespace: "fixture", UID: "deployment-uid", Annotations: map[string]string{
			byohWorkloadOwnershipAnnotation: nonce}}}
	for _, test := range []struct {
		name       string
		namespace  *corev1.Namespace
		deployment *appsv1.Deployment
	}{
		{name: "Namespace replacement", namespace: func() *corev1.Namespace {
			replacement := ownedNamespace.DeepCopy()
			replacement.UID = "replacement-uid"
			return replacement
		}(), deployment: ownedDeployment},
		{name: "Deployment replacement", namespace: ownedNamespace, deployment: func() *appsv1.Deployment {
			replacement := ownedDeployment.DeepCopy()
			replacement.UID = "replacement-uid"
			return replacement
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(test.namespace, test.deployment)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fixture := &byohTestFixture{coreClient: client.CoreV1(), appsClient: client.AppsV1(), ctx: ctx,
				workloadNS: "fixture", workloadNSUID: "namespace-uid", workloadDeploymentUID: "deployment-uid",
				workloadOwnershipNonce: nonce}
			if err := fixture.waitForBYOHWebServer(nil); err == nil {
				t.Fatal("same-name workload object replacement was accepted")
			}
		})
	}
}

func TestTwoHostWorkloadAllowsAllFivePodsOnEitherSelectedNode(t *testing.T) {
	controller := true
	nodeA := readyBYOHNode("node-a", "uid-a", "192.0.2.10")
	nodeB := readyBYOHNode("node-b", "uid-b", "192.0.2.11")
	leases := []*byohHostLease{{NodeName: nodeA.Name, NodeUID: nodeA.UID},
		{NodeName: nodeB.Name, NodeUID: nodeB.UID}}
	for _, selected := range []string{nodeA.Name, nodeB.Name} {
		t.Run(selected, func(t *testing.T) {
			pods := make([]corev1.Pod, 5)
			for index := range pods {
				pods[index] = corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", index),
					OwnerReferences: []metav1.OwnerReference{{UID: "rs-current", Controller: &controller}}},
					Spec: corev1.PodSpec{NodeName: selected}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
						Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
			}
			ready, err := currentBYOHWorkloadReady(context.Background(),
				fake.NewSimpleClientset(nodeA, nodeB).CoreV1().Nodes(), pods, "rs-current", leases, 5)
			if err != nil || !ready {
				t.Fatalf("five Pods on one selected host were rejected: ready=%t err=%v", ready, err)
			}
		})
	}
}

func TestCurrentReplicaSetRejectsOwnerRevisionTemplateAndUniquenessAmbiguity(t *testing.T) {
	controller := true
	template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "readiness"}}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{UID: "deployment-uid",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}},
		Spec: appsv1.DeploymentSpec{Template: template}}
	valid := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{UID: "rs-valid",
		Annotations:     map[string]string{"deployment.kubernetes.io/revision": "2"},
		OwnerReferences: []metav1.OwnerReference{{UID: deployment.UID, Controller: &controller}}},
		Spec: appsv1.ReplicaSetSpec{Template: template}}
	wrongOwner := valid.DeepCopy()
	wrongOwner.OwnerReferences[0].UID = "other-deployment"
	wrongTemplate := valid.DeepCopy()
	wrongTemplate.Spec.Template.Labels = map[string]string{"app": "different"}
	duplicate := valid.DeepCopy()
	duplicate.UID = "rs-duplicate"
	for name, sets := range map[string][]appsv1.ReplicaSet{
		"wrong owner":                  {*wrongOwner},
		"same revision wrong template": {valid, *wrongTemplate},
		"duplicate current":            {valid, *duplicate},
	} {
		t.Run(name, func(t *testing.T) {
			if uid, found := currentDeploymentReplicaSetUID(deployment, sets); found || uid != "" {
				t.Fatalf("ambiguous current ReplicaSet was accepted: uid=%q found=%t", uid, found)
			}
		})
	}
}
