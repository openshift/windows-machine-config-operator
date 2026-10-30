package winc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func byohRotationGuardFixture(t *testing.T) (*byohTestFixture, *fake.Clientset, *byohHostLease) {
	t.Helper()
	fixture, client, _ := byohPreflightFixture(t)
	ctx := context.Background()
	proof, err := fixture.preflightBYOH(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	var lease byohHostLease
	for _, host := range proof.Hosts {
		if host.Lease.Disposable {
			lease = host.Lease
		} else if err := client.CoreV1().Nodes().Delete(ctx, host.Lease.NodeName, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	maps := client.CoreV1().ConfigMaps(wmcoNamespace)
	pool := proof.Pool.DeepCopy()
	for address, record := range proof.Records {
		pool.Data[address] = reserveBYOHProducerRecord(record, "run", time.Now())
	}
	if _, err := maps.Update(ctx, pool, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	cm := proof.Instances.DeepCopy()
	cm.Data = map[string]string{lease.Address: "username=" + lease.Username}
	cm.Annotations[registrationOwnershipAnnotation(lease.Address)] = lease.LeaseID
	if _, err := maps.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	session := &byohRunSession{Scope: byohRunScope{Run: "run", PoolUID: pool.UID, ClusterUID: "cluster"}, UID: "session", Nonce: "nonce", Phase: "Prepared", ActiveCase: "case", InstancesUID: cm.UID, SecretUID: proof.SecretUID, KeyHash: proof.KeyHash, Reservations: copyStringMap(pool.Data), Hosts: proof.Hosts}
	session.Name = byohSessionName(session.Scope)
	if _, err := maps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: session.Name, UID: session.UID, ResourceVersion: "1", Annotations: map[string]string{byohSessionAnnotation: mustBYOHJSON(session)}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	fixture.session, fixture.caseID, fixture.secretLease = session, "case", &lease
	fixture.instancesState = byohInstancesState{observed: true, initiallyExisted: true, initialUID: cm.UID, ownedUID: cm.UID}
	secret, err := client.CoreV1().Secrets(wmcoNamespace).Get(ctx, byohPrivateKeySecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secret.ResourceVersion = "1"
	if _, err := client.CoreV1().Secrets(wmcoNamespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	fixture.secretSnapshot, fixture.originalKeyHash = secret.DeepCopy(), proof.KeyHash
	fixture.keyConvergenceForTest = func(context.Context, *byohHostLease, string, []byte, string, bool) error { return nil }
	fixture.verifySSHForTest = func(context.Context, *byohHostLease, []byte) error { return nil }
	fixture.removeAuthorizedKeyForTest = func(context.Context, *byohHostLease, string) error { return nil }
	return fixture, client, &lease
}

func TestBYOHGuardPendingConsumers(t *testing.T) {
	for _, kind := range []string{"windows-os", "required-name", "os-selector", "unknown", "linux", "completed-probe"} {
		t.Run(kind, func(t *testing.T) {
			fixture, client, _ := byohPreflightFixture(t)
			ctx := context.Background()
			proof, err := fixture.preflightBYOH(ctx, "run")
			if err != nil {
				t.Fatal(err)
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "other"}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
			switch kind {
			case "windows-os":
				pod.Spec.OS = &corev1.PodOS{Name: corev1.Windows}
				fallthrough
			case "required-name":
				pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"host3.example.test"}}}}}}}}
				pod.Spec.Tolerations = []corev1.Toleration{{Key: "os", Operator: corev1.TolerationOpEqual, Value: "Windows", Effect: corev1.TaintEffectNoSchedule}}
			case "os-selector":
				pod.Spec.NodeSelector = map[string]string{"kubernetes.io/os": "windows"}
			case "linux":
				pod.Spec.OS = &corev1.PodOS{Name: corev1.Linux}
			case "completed-probe":
				pod.Spec.NodeName = "host3.example.test"
				pod.Status.Phase = corev1.PodSucceeded
			}
			allowed := kind == "linux" || kind == "completed-probe"
			if _, err := client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := revalidateBYOHPreflight(ctx, fixture, proof); (err == nil) != allowed {
				t.Fatalf("revalidation %s: %v", kind, err)
			}
			if _, err := fixture.preflightBYOH(ctx, "run"); (err == nil) != allowed {
				t.Fatalf("preflight %s: %v", kind, err)
			}
			rotation, rotationClient, lease := byohRotationGuardFixture(t)
			if _, err := rotationClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod.DeepCopy(), metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := rotation.validateBYOHRotationIsolation(ctx, lease); (err == nil) != allowed {
				t.Fatalf("rotation %s: %v", kind, err)
			}
		})
	}
}

func TestBYOHGuardInitialProbeDisappearance(t *testing.T) {
	for _, mode := range []string{"gone", "held", "replacement", "api"} {
		t.Run(mode, func(t *testing.T) {
			fixture, client, facts := byohPreflightFixture(t)
			limit := 750 * time.Millisecond
			if mode == "gone" {
				limit = 5 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), limit)
			defer cancel()
			fixture.preflightProbeForTest = nil
			client.PrependReactor("create", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
				cm := action.(clienttesting.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
				cm.UID = "session"
				cm.ResourceVersion = "1"
				return true, cm, client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, wmcoNamespace)
			})
			client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
				pod.UID = "probe"
				pod.ResourceVersion = "1"
				pod.Status.Phase = corev1.PodSucceeded
				return true, pod, client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, wmcoNamespace)
			})
			deletes := 0
			client.PrependReactor("delete", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				deletes++
				deletion := action.(clienttesting.DeleteAction)
				preconditions := deletion.GetDeleteOptions().Preconditions
				if preconditions == nil || preconditions.UID == nil || *preconditions.UID != "probe" || preconditions.ResourceVersion == nil || *preconditions.ResourceVersion != "1" {
					t.Fatal("missing exact UID/RV DELETE guards")
				}
				if mode == "gone" {
					return false, nil, nil
				}
				object, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), wmcoNamespace, deletion.GetName())
				if err != nil {
					t.Fatal(err)
				}
				pod := object.(*corev1.Pod).DeepCopy()
				now := metav1.Now()
				pod.DeletionTimestamp = &now
				pod.Finalizers = []string{"test/hold"}
				if mode == "replacement" {
					pod.UID = "foreign"
				}
				if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
				return true, nil, nil
			})
			client.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				if mode == "api" && deletes > 0 {
					return true, nil, apierrors.NewForbidden(corev1.Resource("pods"), "probe", errors.New("uncertain cleanup read"))
				}
				return false, nil, nil
			})
			fixture.hostProcessLogsForTest = func(ctx context.Context, _, name string, _ types.UID) ([]byte, error) {
				pod, err := client.CoreV1().Pods(wmcoNamespace).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return []byte(mustBYOHJSON(facts[pod.Spec.NodeName])), nil
			}
			fixture.waitForLeaseLog = func(ctx context.Context, lease *byohHostLease, message string, _ time.Time) error {
				if message == "instance has been deconfigured" {
					return client.CoreV1().Nodes().Delete(ctx, lease.NodeName, metav1.DeleteOptions{})
				}
				return nil
			}
			scope := byohRunScope{Run: "run", PoolUID: "pool", ClusterUID: "cluster"}
			_, err := fixture.ensureBYOHSession(ctx, scope)
			if (err == nil) != (mode == "gone") {
				t.Fatalf("mode=%s initialization=%v", mode, err)
			}
			cm, getErr := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohSessionName(scope), metav1.GetOptions{})
			if getErr != nil {
				t.Fatal(getErr)
			}
			owner, parseErr := decodeBYOHSession(cm, scope)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if (owner.Phase == "Prepared") != (mode == "gone") {
				t.Fatalf("mode=%s phase=%s", mode, owner.Phase)
			}
			if mode != "gone" && (deletes != 1 || len(cm.Data) != 0) {
				t.Fatalf("unsafe cleanup or availability: deletes=%d entries=%d", deletes, len(cm.Data))
			}
			if mode == "held" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("outer deadline not retained: %v", err)
			}
			t.Logf("mode=%s phase=%s deletes=%d err=%v", mode, owner.Phase, deletes, err)
		})
	}
}

func TestBYOHGuardWithdrawalProjection(t *testing.T) {
	for _, boundary := range []string{"before", "conflict", "readback"} {
		t.Run(boundary, func(t *testing.T) {
			fixture, client, proof, _ := byohGuardPreparation(t)
			maps := fixture.coreClient.ConfigMaps(wmcoNamespace)
			var leases []*byohHostLease
			for _, host := range proof.Hosts {
				for _, address := range host.RegistrationAliases {
					lease := host.Lease
					lease.Address = address
					leases = append(leases, &lease)
				}
			}
			state := &byohInstancesState{observed: true, initiallyExisted: true, initialUID: proof.Instances.UID}
			if err := adoptBYOHInitialRegistrations(context.Background(), maps, proof.Instances, leases, state); err != nil {
				t.Fatal(err)
			}
			addAlias := func(cm *corev1.ConfigMap) {
				cm.Data["new-host1.example.test"] = "username=test-user"
				cm.ResourceVersion = "2"
				if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
			}
			if boundary == "before" {
				object, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("configmaps"), wmcoNamespace, byohInstancesConfigMap)
				if err != nil {
					t.Fatal(err)
				}
				addAlias(object.(*corev1.ConfigMap).DeepCopy())
			}
			attempts := 0
			client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
				attempt := action.(clienttesting.UpdateAction).GetObject().(*corev1.ConfigMap)
				if attempt.Name != byohInstancesConfigMap {
					return false, nil, nil
				}
				attempts++
				if boundary == "readback" {
					addAlias(attempt.DeepCopy())
					return true, nil, errors.New("lost applied withdrawal response")
				}
				object, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("configmaps"), wmcoNamespace, byohInstancesConfigMap)
				if err != nil {
					t.Fatal(err)
				}
				addAlias(object.(*corev1.ConfigMap).DeepCopy())
				return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), attempt.Name, errors.New("new alias"))
			})
			if err := unregisterBYOHInstances(context.Background(), maps, leases, state, true, nil, nil); err == nil {
				t.Fatal("expanded registry accepted")
			}
			if (boundary == "before" && attempts != 0) || (boundary != "before" && attempts != 1) {
				t.Fatalf("unsafe retry attempts=%d", attempts)
			}
			cm, err := maps.Get(context.Background(), byohInstancesConfigMap, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if cm.Data["new-host1.example.test"] != "username=test-user" || cm.UID != proof.Instances.UID || cm.Annotations["unrelated"] != "preserve" {
				t.Fatal("unknown alias or metadata lost")
			}
		})
	}
}

func byohGuardPreparation(t *testing.T) (*byohTestFixture, *fake.Clientset, *byohPreflightProof, *byohRunSession) {
	t.Helper()
	fixture, client, _ := byohPreflightFixture(t)
	proof, err := fixture.preflightBYOH(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	session := &byohRunSession{Scope: byohRunScope{Run: "run", PoolUID: "pool", ClusterUID: "cluster"}, Nonce: "nonce", Phase: "Initializing", UID: "session"}
	session.Name = byohSessionName(session.Scope)
	ledger := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: session.Name, Namespace: wmcoNamespace, UID: session.UID, ResourceVersion: "1", Annotations: map[string]string{byohSessionAnnotation: mustBYOHJSON(session)}}}
	if _, err := client.CoreV1().ConfigMaps(wmcoNamespace).Create(context.Background(), ledger, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	fixture.waitForLeaseLog = func(context.Context, *byohHostLease, string, time.Time) error {
		return errors.New("no reset proof supplied by design reproducer")
	}
	return fixture, client, proof, session
}

func TestBYOHGuardNewSameHostAliasAfterSnapshot(t *testing.T) {
	fixture, _, proof, session := byohGuardPreparation(t)
	client := fixture.coreClient
	maps := client.ConfigMaps(wmcoNamespace)
	alias := "new-host1.example.test"
	originalResolver := fixture.preflightLookupForTest
	fixture.preflightLookupForTest = func(ctx context.Context, name string) ([]net.IP, error) {
		if name == alias {
			return []net.IP{net.ParseIP("192.0.2.1")}, nil
		}
		return originalResolver(ctx, name)
	}
	leases := []*byohHostLease{}
	for _, host := range proof.Hosts {
		for _, address := range host.RegistrationAliases {
			lease := host.Lease
			lease.Address = address
			leases = append(leases, &lease)
		}
	}
	current, err := maps.Get(context.Background(), byohInstancesConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	current.Data[alias] = "username=test-user"
	current.ResourceVersion = "2"
	if _, err := maps.Update(context.Background(), current, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	state := &byohInstancesState{observed: true, initiallyExisted: true, initialUID: proof.Instances.UID}
	adoptErr := adoptBYOHInitialRegistrations(context.Background(), maps, proof.Instances, leases, state)
	if adoptErr != nil {
		after, err := maps.Get(context.Background(), byohInstancesConfigMap, metav1.GetOptions{})
		if err != nil || !strings.Contains(adoptErr.Error(), "complete initial registration set changed") || !reflect.DeepEqual(after.Data, current.Data) || !reflect.DeepEqual(after.Annotations, proof.Instances.Annotations) {
			t.Fatalf("rejection did not preserve complete unadopted registry: %v / %v", adoptErr, err)
		}
		t.Logf("correctly rejected changed alias set: %v", adoptErr)
		return
	}
	withdrawErr := unregisterBYOHInstances(context.Background(), maps, leases, state, true, nil, nil)
	after, err := maps.Get(context.Background(), byohInstancesConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("session=%s snapshot=%d final=%d new_same_host_alias_retained=%t withdrawal_error=%v", session.Nonce, len(proof.Instances.Data), len(after.Data), after.Data[alias] == "username=test-user", withdrawErr)
	t.Errorf("adoption and withdrawal succeeded despite new unproved same-host alias; removed %d original registrations", len(proof.Instances.Data))
}

func TestBYOHGuardNewAliasDuringAdoptionConflict(t *testing.T) {
	fixture, client, proof, _ := byohGuardPreparation(t)
	maps := fixture.coreClient.ConfigMaps(wmcoNamespace)
	leases := []*byohHostLease{}
	for _, host := range proof.Hosts {
		for _, address := range host.RegistrationAliases {
			lease := host.Lease
			lease.Address = address
			leases = append(leases, &lease)
		}
	}
	conflicts := 0
	client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		attempt := action.(clienttesting.UpdateAction).GetObject().(*corev1.ConfigMap)
		if attempt.Name != byohInstancesConfigMap || conflicts != 0 {
			return false, nil, nil
		}
		conflicts++
		resource := corev1.SchemeGroupVersion.WithResource("configmaps")
		object, err := client.Tracker().Get(resource, wmcoNamespace, byohInstancesConfigMap)
		if err != nil {
			t.Fatal(err)
		}
		current := object.(*corev1.ConfigMap).DeepCopy()
		current.Data["new-host1.example.test"] = "username=test-user"
		current.ResourceVersion = "2"
		if err := client.Tracker().Update(resource, current, wmcoNamespace); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), attempt.Name, errors.New("new alias raced adoption"))
	})
	state := &byohInstancesState{observed: true, initiallyExisted: true, initialUID: proof.Instances.UID}
	adoptErr := adoptBYOHInitialRegistrations(context.Background(), maps, proof.Instances, leases, state)
	if adoptErr != nil {
		after, err := maps.Get(context.Background(), byohInstancesConfigMap, metav1.GetOptions{})
		expected := copyStringMap(proof.Instances.Data)
		expected["new-host1.example.test"] = "username=test-user"
		if err != nil || conflicts != 1 || !strings.Contains(adoptErr.Error(), "complete initial registration set changed") || !reflect.DeepEqual(after.Data, expected) || !reflect.DeepEqual(after.Annotations, proof.Instances.Annotations) {
			t.Fatalf("conflict retry mutated unproved alias set: %v / %v", adoptErr, err)
		}
		t.Logf("correctly rejected alias-set change on conflict retry: %v", adoptErr)
		return
	}
	withdrawErr := unregisterBYOHInstances(context.Background(), maps, leases, state, true, nil, nil)
	after, err := maps.Get(context.Background(), byohInstancesConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("injected_conflicts=%d adoption_error=%v withdrawal_error=%v leftover_aliases=%d", conflicts, adoptErr, withdrawErr, len(after.Data))
	t.Error("conflict retry accepted the expanded alias set without re-proving it")
}

func TestBYOHGuardOwnedPartialAdoptionCleanup(t *testing.T) {
	fixture, client, _ := byohPreflightFixture(t)
	proof, err := fixture.preflightBYOH(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	session := &byohRunSession{Scope: byohRunScope{Run: "run", PoolUID: "pool", ClusterUID: "cluster"}, Nonce: "nonce", Phase: "Initializing", UID: "session"}
	session.Name = byohSessionName(session.Scope)
	if _, err := client.CoreV1().ConfigMaps(wmcoNamespace).Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: session.Name, Namespace: wmcoNamespace, UID: session.UID, ResourceVersion: "1", Annotations: map[string]string{byohSessionAnnotation: mustBYOHJSON(session)}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	injected := false
	primary := errors.New("applied adoption response lost")
	var operations []string
	client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		if updated.Name == session.Name {
			var owner byohRunSession
			if err := json.Unmarshal([]byte(updated.Annotations[byohSessionAnnotation]), &owner); err != nil {
				t.Fatal(err)
			}
			operations = append(operations, owner.Phase)
		}
		if updated.Name != byohInstancesConfigMap || injected {
			if updated.Name == byohInstancesConfigMap {
				operations = append(operations, "partial-cleanup")
			}
			return false, nil, nil
		}
		injected = true
		operations = append(operations, "adopt")
		if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), updated, wmcoNamespace); err != nil {
			t.Fatal(err)
		}
		updated.Annotations[registrationOwnershipAnnotation("192.0.2.1")] = "foreign"
		if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), updated, wmcoNamespace); err != nil {
			t.Fatal(err)
		}
		return true, nil, primary
	})
	prepareErr := fixture.prepareBYOHHosts(context.Background(), proof, session)
	after, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohInstancesConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owned := 0
	for address := range after.Data {
		if after.Annotations[registrationOwnershipAnnotation(address)] == session.Scope.Run {
			owned++
		}
	}
	t.Logf("prepare_error=%v registrations=%d exact_run_owned_left=%d foreign_owner_preserved=%t phase=%s", prepareErr, len(after.Data), owned, after.Annotations[registrationOwnershipAnnotation("192.0.2.1")] == "foreign", session.Phase)
	if owned != 0 {
		t.Errorf("preparation returned without bounded cleanup of %d individually provable owned aliases", owned)
	}
	if !errors.Is(prepareErr, primary) || session.Phase != "Failed" || len(after.Data) != 1 || after.Data["192.0.2.1"] != "username=test-user" || after.Annotations[registrationOwnershipAnnotation("192.0.2.1")] != "foreign" || after.UID != proof.Instances.UID || after.Annotations["unrelated"] != "preserve" {
		t.Fatalf("partial cleanup lost guarded failure evidence: %v %#v", prepareErr, after.ObjectMeta)
	}
	pool, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohPoolConfigMap, metav1.GetOptions{})
	if err != nil || len(pool.Data) != 3 || !reflect.DeepEqual(pool.Data, session.Reservations) {
		t.Fatalf("raw run reservations changed: %v", err)
	}
	if !reflect.DeepEqual(operations, []string{"Preparing", "adopt", "partial-cleanup", "Failed"}) {
		t.Fatalf("unexpected operation order: %v", operations)
	}
	t.Logf("operations=%v removed=5 foreign=1 raw_reserved=3", operations)
}

func TestBYOHGuardSecretConflictAndRestoration(t *testing.T) {
	for _, change := range []string{"conflict", "failed-owned", "foreign-key", "foreign-nonce", "foreign-uid", "foreign-case"} {
		t.Run(change, func(t *testing.T) {
			fixture, client, lease := byohRotationGuardFixture(t)
			ctx := context.Background()
			replacement, err := generateTestPrivateKeyPEM()
			if err != nil {
				t.Fatal(err)
			}
			signer, err := ssh.ParsePrivateKey(replacement)
			if err != nil {
				t.Fatal(err)
			}
			fixture.replacementAuthorizedKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
			secret, err := client.CoreV1().Secrets(wmcoNamespace).Get(ctx, byohPrivateKeySecret, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if change != "conflict" {
				secret.Data[byohPrivateKeyDataKey] = replacement
				if change == "foreign-key" {
					secret.Data[byohPrivateKeyDataKey], err = generateTestPrivateKeyPEM()
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := client.CoreV1().Secrets(wmcoNamespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			mutateAuthority := func() {
				object, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("configmaps"), wmcoNamespace, fixture.session.Name)
				if err != nil {
					t.Fatal(err)
				}
				cm := object.(*corev1.ConfigMap).DeepCopy()
				owner, err := decodeBYOHSession(cm, fixture.session.Scope)
				if err != nil {
					t.Fatal(err)
				}
				owner.Phase = "Failed"
				switch change {
				case "foreign-nonce":
					owner.Nonce = "foreign"
				case "foreign-uid":
					cm.UID = "foreign"
				case "foreign-case":
					owner.ActiveCase = "foreign"
				}
				cm.Annotations[byohSessionAnnotation] = mustBYOHJSON(owner)
				if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
			}
			writes := 0
			client.PrependReactor("update", "secrets", func(action clienttesting.Action) (bool, runtime.Object, error) {
				writes++
				if change == "conflict" {
					mutateAuthority()
					return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), byohPrivateKeySecret, errors.New("concurrent status change"))
				}
				return false, nil, nil
			})
			before := append([]byte(nil), secret.Data[byohPrivateKeyDataKey]...)
			if change == "conflict" {
				err = fixture.updateBYOHPrivateKey(ctx, replacement)
			} else {
				mutateAuthority()
				err = fixture.restoreBYOHPrivateKeyWithContext(ctx)
			}
			after, getErr := client.CoreV1().Secrets(wmcoNamespace).Get(ctx, byohPrivateKeySecret, metav1.GetOptions{})
			if getErr != nil {
				t.Fatal(getErr)
			}
			if change == "failed-owned" {
				if err != nil || writes != 1 || !fixture.secretRestored || !reflect.DeepEqual(after.Data[byohPrivateKeyDataKey], fixture.secretSnapshot.Data[byohPrivateKeyDataKey]) {
					t.Fatalf("owned restore failed: writes=%d err=%v", writes, err)
				}
			} else if err == nil || !reflect.DeepEqual(before, after.Data[byohPrivateKeyDataKey]) || (change == "conflict" && writes != 1) || (change != "conflict" && writes != 0) {
				t.Fatalf("authority/key loss crossed mutation boundary: writes=%d err=%v host=%s", writes, err, lease.NodeName)
			}
			t.Logf("mode=%s writes=%d restored=%t err=%v", change, writes, fixture.secretRestored, err)
		})
	}
}
func TestBYOHGuardRotationAfterSessionAuthorityChanges(t *testing.T) {
	for _, change := range []string{"phase", "nonce", "uid", "case"} {
		t.Run(change, func(t *testing.T) {
			fixture, client, facts := byohPreflightFixture(t)
			maps := client.CoreV1().ConfigMaps(wmcoNamespace)
			secret, err := client.CoreV1().Secrets(wmcoNamespace).Get(context.Background(), byohPrivateKeySecret, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			originalHash, err := publicKeyHash(secret.Data[byohPrivateKeyDataKey])
			if err != nil {
				t.Fatal(err)
			}
			pool, err := maps.Get(context.Background(), byohPoolConfigMap, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for address, raw := range pool.Data {
				record, err := decodeBYOHProducerRecord(address, raw)
				if err != nil {
					t.Fatal(err)
				}
				pool.Data[address] = reserveBYOHProducerRecord(record, "run", time.Now())
			}
			if _, err := maps.Update(context.Background(), pool, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			for index := 1; index <= 2; index++ {
				if err := client.CoreV1().Nodes().Delete(context.Background(), fmt.Sprintf("host%d.example.test", index), metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			lease := &byohHostLease{Address: "192.0.2.3", Username: "test-user", LeaseID: "lease", NodeName: "host3.example.test", NodeUID: "node3", Disposable: true}
			hostFacts := facts[lease.NodeName]
			session := &byohRunSession{Scope: byohRunScope{Run: "run", PoolUID: "pool", ClusterUID: "cluster"}, UID: "session", Nonce: "nonce", Phase: "Prepared", ActiveCase: "case", SecretUID: string(secret.UID), KeyHash: originalHash, Reservations: copyStringMap(pool.Data), InstancesUID: "instances", Hosts: []byohPreparedHost{{Lease: *lease, Facts: hostFacts}}}
			session.Name = byohSessionName(session.Scope)
			fixture.session, fixture.caseID = session, "case"
			fixture.instancesState = byohInstancesState{observed: true, initiallyExisted: true, initialUID: "instances", ownedUID: "instances"}
			instances, err := maps.Get(context.Background(), byohInstancesConfigMap, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			instances.Data = map[string]string{lease.Address: "username=test-user"}
			instances.Annotations[registrationOwnershipAnnotation(lease.Address)] = lease.LeaseID
			if _, err := maps.Update(context.Background(), instances, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			ledger := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: session.Name, Namespace: wmcoNamespace, UID: session.UID, ResourceVersion: "1", Annotations: map[string]string{byohSessionAnnotation: mustBYOHJSON(session)}}}
			if _, err := maps.Create(context.Background(), ledger, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			fixture.placeAuthorizedKeyForTest = func(context.Context, *byohHostLease, string) error {
				owner := *session
				switch change {
				case "phase":
					owner.Phase = "Failed"
				case "nonce":
					owner.Nonce = "foreign"
				case "uid":
					ledger.UID = types.UID("replacement")
				case "case":
					owner.ActiveCase = "foreign-case"
				}
				ledger.Annotations[byohSessionAnnotation] = mustBYOHJSON(&owner)
				return client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), ledger, wmcoNamespace)
			}
			fixture.keyConvergenceForTest = func(context.Context, *byohHostLease, string, []byte, string, bool) error { return nil }
			fixture.verifySSHForTest = func(context.Context, *byohHostLease, []byte) error { return nil }
			fixture.configuredServicesForTest = func(context.Context, *byohHostLease) error { return nil }
			rotateErr := fixture.rotateBYOHPrivateKey(lease)
			after, err := client.CoreV1().Secrets(wmcoNamespace).Get(context.Background(), byohPrivateKeySecret, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			afterHash, err := publicKeyHash(after.Data[byohPrivateKeyDataKey])
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("authority_change=%s rotate_error=%v secret_key_changed=%t", change, rotateErr, afterHash != originalHash)
			if rotateErr == nil || afterHash != originalHash {
				t.Errorf("rotation failed to fence global Secret mutation after session authority changed")
			}
		})
	}
}
