package winc

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func lifecycleFixture(t *testing.T) (*suppliedBYOHLifecycle, *fake.Clientset) {
	t.Helper()
	pool := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: suppliedPool, Namespace: wmcoNamespace, UID: "pool", ResourceVersion: "1", Annotations: map[string]string{"unrelated": "keep"}}, Data: map[string]string{}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: suppliedInstances, Namespace: wmcoNamespace, UID: "instances", ResourceVersion: "1", Labels: map[string]string{"unrelated": "keep"}, Annotations: map[string]string{"unrelated": "keep"}}, Data: map[string]string{}}
	for i, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"} {
		disposable := "false"
		if i == 2 {
			disposable = "true"
		}
		pool.Data[ip] = "status: available\nusername: Administrator\naddress-type: ip\nplatform: none\ntest-id: \nallocated-at: \nlast-updated: 2026-11-01T00:00:00Z\ndisposable: " + disposable + "\n"
		cm.Data[ip] = "username=Administrator"
	}
	address, user, err := suppliedHost(pool, cm)
	if err != nil {
		t.Fatal(err)
	}
	pool.Annotations[lifecycleOwner] = "run:attempt"
	pool.Data[address] = strings.Replace(pool.Data[address], "status: available", "status: allocated", 1)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "supplied-node", UID: "initial", Labels: map[string]string{"kubernetes.io/os": "windows", byohLabel: "true"}, Annotations: map[string]string{wmcoVersion: "test-version"}}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: address}}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	client := fake.NewSimpleClientset(pool, cm, node)
	f := &suppliedBYOHLifecycle{core: client.CoreV1(), pool: pool.DeepCopy(), instances: cm.DeepCopy(), owner: "run:attempt", address: address, username: user, node: node.DeepCopy(), initialUID: node.UID, checkHost: func(context.Context, string) (string, error) { return "BYOH_OK", nil }}
	return f, client
}

func TestSuppliedBYOHInventory(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "duplicate", "owned", "alias", "extra-registration", "user", "disposable", "timestamp", "owner"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := lifecycleFixture(t)
			pool, cm := f.pool, f.instances
			pool.Annotations = nil
			pool.Data[f.address] = strings.Replace(pool.Data[f.address], "status: allocated", "status: available", 1)
			switch mode {
			case "missing":
				pool.Data[f.address] = strings.Replace(pool.Data[f.address], "username: Administrator\n", "", 1)
			case "duplicate":
				pool.Data[f.address] += "status: available\n"
			case "owned":
				pool.Data[f.address] = strings.Replace(pool.Data[f.address], "test-id: ", "test-id: foreign", 1)
			case "alias":
				pool.Data["host.example.test"] = pool.Data[f.address]
				delete(pool.Data, f.address)
			case "extra-registration":
				cm.Data["other.example.test"] = "username=other"
			case "user":
				cm.Data[f.address] = "username=other"
			case "disposable":
				pool.Data[f.address] = strings.Replace(pool.Data[f.address], "disposable: true", "disposable: false", 1)
			case "timestamp":
				pool.Data[f.address] = strings.Replace(pool.Data[f.address], "2026-11-01T00:00:00Z", "yesterday", 1)
			case "owner":
				cm.Annotations[lifecycleOwner] = "foreign"
			}
			address, _, err := suppliedHost(pool, cm)
			if (err == nil) != (mode == "valid") {
				t.Fatalf("%s: %v", mode, err)
			}
			if mode == "valid" && address != "192.0.2.3" {
				t.Fatalf("selected %s; want the disposable host 192.0.2.3", address)
			}
		})
	}
}

func TestSuppliedBYOHProvisionerAnnotations(t *testing.T) {
	f, _ := lifecycleFixture(t)
	f.pool.Annotations = map[string]string{"windowsmachineconfig.openshift.io/byoh-pool": "provisioner"}
	f.instances.Annotations = map[string]string{"windowsmachineconfig.openshift.io/byoh-registration": "provisioner"}
	f.pool.Data[f.address] = strings.Replace(f.pool.Data[f.address], "status: allocated", "status: available", 1)
	if _, _, err := suppliedHost(f.pool, f.instances); err != nil {
		t.Fatalf("provisioner annotations must not block adoption: %v", err)
	}
	f.pool.Annotations[lifecycleOwner] = "foreign"
	if _, _, err := suppliedHost(f.pool, f.instances); err == nil {
		t.Fatal("active lifecycle ownership must block adoption")
	}
}

func TestSuppliedBYOHRegistrationGuards(t *testing.T) {
	for _, mode := range []string{"success", "lost-response", "lost-readback", "pool-replaced", "claim-lost", "registry-replaced", "new-alias", "changed-user", "foreign-owner", "conflict", "node-replaced", "foreign-workload", "ip-changed", "pending-workload"} {
		t.Run(mode, func(t *testing.T) {
			f, client := lifecycleFixture(t)
			ctx := context.Background()
			cm, _ := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedInstances, metav1.GetOptions{})
			pool, _ := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedPool, metav1.GetOptions{})
			switch mode {
			case "pool-replaced":
				pool.UID = "replacement"
			case "claim-lost":
				pool.Data[f.address] = "status: available"
			case "registry-replaced":
				cm.UID = "replacement"
			case "new-alias":
				cm.Data["alias.example.test"] = "username=Administrator"
			case "changed-user":
				cm.Data[f.address] = "username=foreign"
			case "foreign-owner":
				cm.Annotations[lifecycleOwner] = "foreign"
			case "node-replaced":
				node := f.node.DeepCopy()
				node.UID = "replacement"
				_, _ = f.core.Nodes().Update(ctx, node, metav1.UpdateOptions{})
			case "foreign-workload":
				_, _ = f.core.Pods("other").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "workload"}, Spec: corev1.PodSpec{NodeName: f.node.Name}}, metav1.CreateOptions{})
			case "ip-changed":
				node := f.node.DeepCopy()
				node.Status.Addresses[0].Address = "192.0.2.99"
				_, _ = f.core.Nodes().Update(ctx, node, metav1.UpdateOptions{})
			case "pending-workload":
				_, _ = f.core.Pods("other").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pending"}}, metav1.CreateOptions{})
			}
			_, _ = f.core.ConfigMaps(wmcoNamespace).Update(ctx, cm, metav1.UpdateOptions{})
			_, _ = f.core.ConfigMaps(wmcoNamespace).Update(ctx, pool, metav1.UpdateOptions{})
			writes, failRead := 0, false
			client.PrependReactor("get", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
				if mode == "lost-readback" && failRead {
					failRead = false
					return true, nil, errors.New("readback lost")
				}
				return false, nil, nil
			})
			client.PrependReactor("update", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
				writes++
				if mode == "conflict" {
					return true, nil, errors.New("conflict")
				}
				if mode == "lost-response" || mode == "lost-readback" {
					wanted := action.(ktesting.UpdateAction).GetObject().(*corev1.ConfigMap)
					if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), wanted, wmcoNamespace); err != nil {
						t.Fatal(err)
					}
					failRead = true
					return true, nil, errors.New("response lost")
				}
				return false, nil, nil
			})
			err := f.setRegistration(ctx, false)
			success := mode == "success" || mode == "lost-response"
			if (err == nil) != success {
				t.Fatalf("%s: %v", mode, err)
			}
			wantWrites := 0
			if success || mode == "conflict" || mode == "lost-readback" {
				wantWrites = 1
			}
			if writes != wantWrites {
				t.Fatalf("unexpected mutation: %d", writes)
			}
			actual, _ := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedInstances, metav1.GetOptions{})
			if actual.UID != cm.UID || !maps.Equal(actual.Labels, cm.Labels) || actual.Annotations["unrelated"] != "keep" || actual.Data["192.0.2.2"] != cm.Data["192.0.2.2"] {
				t.Fatal("unrelated state changed")
			}
			if mode == "lost-readback" && !maps.Equal(f.instances.Data, actual.Data) {
				t.Fatal("lost attempted snapshot needed by cleanup")
			}
		})
	}
}

func TestSuppliedBYOHCompleteLifecycle(t *testing.T) {
	f, client := lifecycleFixture(t)
	ctx := context.Background()
	originalPool := f.pool.DeepCopy()
	template := f.node.DeepCopy()
	var checks []string
	f.checkHost = func(_ context.Context, command string) (string, error) {
		checks = append(checks, command)
		return "BYOH_OK", nil
	}
	client.PrependReactor("update", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		cm := action.(ktesting.UpdateAction).GetObject().(*corev1.ConfigMap)
		if cm.Name != suppliedInstances {
			return false, nil, nil
		}
		if cm.Data[f.address] == "" {
			_ = client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("nodes"), "", template.Name)
		} else {
			node := template.DeepCopy()
			node.UID = "new-node"
			if err := client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("nodes"), node, ""); err != nil {
				t.Fatal(err)
			}
		}
		return false, nil, nil
	})
	for _, stage := range []func(context.Context) error{f.deconfigure, f.register, f.cleanup} {
		if err := stage(ctx); err != nil {
			t.Fatal(err)
		}
	}
	cm, _ := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedInstances, metav1.GetOptions{})
	pool, _ := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedPool, metav1.GetOptions{})
	if cm.UID != "instances" || len(cm.Data) != 2 || cm.Data[f.address] != "" || !maps.Equal(pool.Data, originalPool.Data) {
		t.Fatal("unrelated registrations or reservation changed")
	}
	configured := 0
	for _, check := range checks {
		if check == lifecycleConfiguredCommand {
			configured++
		}
	}
	if configured != 3 {
		t.Fatalf("missing HostProcess configured checks: configured=%d", configured)
	}
	if f.mutated || pool.Annotations[lifecycleOwner] != "" || cm.Annotations[lifecycleOwner] != "" {
		t.Fatal("proved cleanup did not release the retry claim")
	}
	if pool.Annotations["unrelated"] != "keep" || cm.Annotations["unrelated"] != "keep" {
		t.Fatal("ownership release changed unrelated annotations")
	}
	if pool.Data[f.address] != originalPool.Data[f.address] {
		t.Fatal("proved cleanup changed the Terraform-owned allocation record")
	}
}

func TestSuppliedBYOHCleanupContract(t *testing.T) {
	for _, mode := range []string{"success", "pool-replaced", "instances-owner-changed", "pool-owner-changed"} {
		t.Run(mode, func(t *testing.T) {
			f, client := lifecycleFixture(t)
			ctx := context.Background()
			instances, err := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedInstances, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			delete(instances.Data, f.address)
			instances.Annotations[lifecycleOwner] = f.owner
			if _, err := f.core.ConfigMaps(wmcoNamespace).Update(ctx, instances, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			pool, err := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedPool, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			pool.Annotations[lifecycleOwner] = f.owner
			if _, err := f.core.ConfigMaps(wmcoNamespace).Update(ctx, pool, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "pool-replaced":
				replaced := pool.DeepCopy()
				replaced.UID = "replacement"
				if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), replaced, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
			case "instances-owner-changed":
				changed := instances.DeepCopy()
				changed.Annotations[lifecycleOwner] = "foreign"
				if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), changed, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
			case "pool-owner-changed":
				changed := pool.DeepCopy()
				changed.Annotations[lifecycleOwner] = "foreign"
				if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), changed, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
			}
			err = f.releaseOwnership(ctx)
			if (err == nil) != (mode == "success") {
				t.Fatalf("%s: %v", mode, err)
			}
			if mode != "success" {
				return
			}
			currentInstances, err := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedInstances, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			currentPool, err := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedPool, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if currentInstances.Annotations[lifecycleOwner] != "" || currentPool.Annotations[lifecycleOwner] != "" {
				t.Fatal("ownership annotations were not released")
			}
			if !maps.Equal(currentInstances.Data, instances.Data) || !maps.Equal(currentPool.Data, pool.Data) {
				t.Fatal("ownership release changed ConfigMap data")
			}
			if currentInstances.Data[f.address] != "" {
				t.Fatal("deconfigured registration was recreated")
			}
			if currentPool.Data[f.address] == "" {
				t.Fatal("Terraform-owned allocation record was removed")
			}
			if currentInstances.Annotations["unrelated"] != "keep" || currentPool.Annotations["unrelated"] != "keep" {
				t.Fatal("ownership release changed unrelated annotations")
			}
		})
	}
}

func TestBYOHLifecycleOptIn(t *testing.T) {
	for _, test := range []struct {
		name, suite string
		wantRun     bool
	}{
		{name: "dedicated suite", suite: "windows-machine-config-operator/byoh-pool", wantRun: true},
		{name: "ordinary suite", suite: "windows-machine-config-operator/all"},
		{name: "missing suite"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if run := byohLifecycleOptIn(test.suite); run != test.wantRun {
				t.Fatalf("run=%t, want %t", run, test.wantRun)
			}
		})
	}
}
