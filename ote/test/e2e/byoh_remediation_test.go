package winc

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

func TestBYOHRegistrationAuthority(t *testing.T) {
	for _, change := range []string{"control", "phase", "nonce", "uid", "case", "conflict", "missing-registry", "missing-registry-conflict", "reservation", "reservation-conflict", "lease", "lease-conflict"} {
		t.Run(change, func(t *testing.T) {
			fixture, client, _ := byohPreflightFixture(t)
			fixture.ctx, fixture.cancel = context.WithTimeout(context.Background(), 10*time.Second)
			defer fixture.cancel()
			const run = "c2fb493a-7430-45a3-acdc-05482c181084"
			t.Setenv(byohPoolRequiredEnv, "true")
			t.Setenv("JOB_SPEC", `{"prowjobid":"`+run+`"}`)
			if _, err := client.CoreV1().Namespaces().Create(fixture.ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster"}}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			proof, err := fixture.preflightBYOH(fixture.ctx, run)
			if err != nil {
				t.Fatal(err)
			}
			session := &byohRunSession{Scope: byohRunScope{Run: run, PoolUID: "pool", ClusterUID: "cluster"}, Nonce: "nonce", Phase: "Initializing", UID: "session"}
			session.Name = byohSessionName(session.Scope)
			maps := client.CoreV1().ConfigMaps(wmcoNamespace)
			if _, err := maps.Create(fixture.ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: session.Name, Namespace: wmcoNamespace, UID: session.UID, ResourceVersion: "1", Annotations: map[string]string{byohSessionAnnotation: mustBYOHJSON(session)}}}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			fixture.waitForLeaseLog = func(ctx context.Context, lease *byohHostLease, message string, _ time.Time) error {
				if message == "instance has been deconfigured" {
					return client.CoreV1().Nodes().Delete(ctx, lease.NodeName, metav1.DeleteOptions{})
				}
				return nil
			}
			if err := fixture.prepareBYOHHosts(fixture.ctx, proof, session); err != nil {
				t.Fatal(err)
			}
			if err := fixture.attachBYOHSession(); err != nil {
				t.Fatal(err)
			}
			leases, err := fixture.allocateBYOHHosts(byohHostRequest{AddressType: byohAddressIP, RequireTrustedSSH: true})
			if err != nil || len(leases) != 1 {
				t.Fatalf("actual allocation: count=%d err=%v", len(leases), err)
			}
			ledger, err := maps.Get(fixture.ctx, session.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			owner, err := decodeBYOHSession(ledger, session.Scope)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "phase":
				owner.Phase = "Failed"
			case "nonce":
				owner.Nonce = "foreign"
			case "uid":
				ledger.UID = "foreign"
			case "case":
				owner.ActiveCase = "foreign"
			}
			ledger.ResourceVersion = "2"
			ledger.Annotations[byohSessionAnnotation] = mustBYOHJSON(owner)
			loseClaim := func() {
				for _, alias := range leases[0].ClaimedAliases {
					entry, err := parseBYOHPoolEntry(alias, ledger.Data[alias])
					if err != nil {
						t.Fatal(err)
					}
					entry.TestID = "foreign"
					ledger.Data[alias] = serializeBYOHPoolEntry(entry)
				}
				if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), ledger, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
			}
			loseReservation := func() {
				object, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("configmaps"), wmcoNamespace, byohPoolConfigMap)
				if err != nil {
					t.Fatal(err)
				}
				pool := object.(*corev1.ConfigMap).DeepCopy()
				delete(pool.Data, leases[0].Address)
				if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), pool, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := maps.Update(fixture.ctx, ledger, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if change == "lease" {
				loseClaim()
			}
			if change == "reservation" {
				loseReservation()
			}
			conflict := change == "conflict" || change == "missing-registry-conflict" || change == "reservation-conflict" || change == "lease-conflict"
			injected := false
			if conflict {
				client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
					attempt := action.(clienttesting.UpdateAction).GetObject().(*corev1.ConfigMap)
					if attempt.Name != byohInstancesConfigMap || injected {
						return false, nil, nil
					}
					injected = true
					switch change {
					case "conflict":
						owner.Phase = "Failed"
						ledger.Annotations[byohSessionAnnotation] = mustBYOHJSON(owner)
						if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), ledger, wmcoNamespace); err != nil {
							t.Fatal(err)
						}
					case "missing-registry-conflict":
						if err := client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("configmaps"), wmcoNamespace, byohInstancesConfigMap); err != nil {
							t.Fatal(err)
						}
					case "reservation-conflict":
						loseReservation()
					case "lease-conflict":
						loseClaim()
					}
					return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), attempt.Name, nil)
				})
			}
			if change == "missing-registry" {
				if err := maps.Delete(fixture.ctx, byohInstancesConfigMap, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
				client.PrependReactor("create", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
					created := action.(clienttesting.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
					if created.Name != byohInstancesConfigMap {
						return false, nil, nil
					}
					created.UID, created.ResourceVersion = "replacement-registry", "3"
					return true, created, client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("configmaps"), created, wmcoNamespace)
				})
			}
			client.ClearActions()
			registerErr := fixture.registerBYOHInstances(leases)
			ledgerReads, writes := 0, 0
			for _, action := range client.Actions() {
				if action.GetResource().Resource != "configmaps" {
					continue
				}
				if action.GetVerb() == "get" {
					if action.(interface{ GetName() string }).GetName() == session.Name {
						ledgerReads++
					}
				}
				if action.GetVerb() == "update" || action.GetVerb() == "create" {
					writes++
				}
			}
			instances, err := maps.Get(fixture.ctx, byohInstancesConfigMap, metav1.GetOptions{})
			if (change == "missing-registry" || change == "missing-registry-conflict") && apierrors.IsNotFound(err) {
				instances, err = &corev1.ConfigMap{}, nil
			} else if change == "missing-registry" || change == "missing-registry-conflict" {
				t.Fatalf("lost original registry must remain NotFound: %v", err)
			}
			if err != nil {
				t.Fatal(err)
			}
			registered := instances.Data[leases[0].Address] == "username="+leases[0].Username
			t.Logf("authority=%s register_error=%v registered=%t registry_writes=%d fresh_session_reads=%d registry_uid=%s", change, registerErr, registered, writes, ledgerReads, instances.UID)
			if change == "control" {
				if registerErr != nil || !registered || writes != 1 || ledgerReads == 0 {
					t.Fatal("valid authority control did not register")
				}
			} else {
				allowedAttempts := 0
				if conflict {
					allowedAttempts = 1
					if !injected {
						t.Fatal("conflict did not reach first rejected update")
					}
				}
				if registerErr == nil || registered || writes != allowedAttempts {
					t.Error("registration crossed destructive API boundary after already-persisted authority loss")
				}
				minimumReads := 1
				if conflict && change != "reservation-conflict" {
					minimumReads = 2
				}
				if change != "reservation" && ledgerReads < minimumReads {
					t.Errorf("missing fresh per-attempt session read: got %d want >= %d", ledgerReads, minimumReads)
				}
			}
		})
	}
}

func TestBYOHAcquiredCaseStartFailureSettlement(t *testing.T) {
	for _, acquired := range []bool{false, true} {
		name := "never_acquired"
		if acquired {
			name = "acquired_then_identity_probe_failed"
		}
		t.Run(name, func(t *testing.T) {
			fixture, client, _ := byohPreflightFixture(t)
			fixture.ctx, fixture.cancel = context.WithTimeout(context.Background(), 10*time.Second)
			defer fixture.cancel()
			run := "c2fb493a-7430-45a3-acdc-05482c181084"
			t.Setenv(byohPoolRequiredEnv, "true")
			t.Setenv("JOB_SPEC", `{"prowjobid":"`+run+`"}`)
			_, err := client.CoreV1().Namespaces().Create(fixture.ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster"}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			proof, err := fixture.preflightBYOH(fixture.ctx, run)
			if err != nil {
				t.Fatal(err)
			}
			session := &byohRunSession{Scope: byohRunScope{Run: run, PoolUID: "pool", ClusterUID: "cluster"}, Nonce: "nonce", Phase: "Initializing", UID: "session"}
			session.Name = byohSessionName(session.Scope)
			_, err = client.CoreV1().ConfigMaps(wmcoNamespace).Create(fixture.ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: session.Name, Namespace: wmcoNamespace, UID: session.UID, ResourceVersion: "1", Annotations: map[string]string{byohSessionAnnotation: mustBYOHJSON(session)}}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			fixture.waitForLeaseLog = func(ctx context.Context, lease *byohHostLease, message string, _ time.Time) error {
				if message == "instance has been deconfigured" {
					return client.CoreV1().Nodes().Delete(ctx, lease.NodeName, metav1.DeleteOptions{})
				}
				return nil
			}
			if err := fixture.prepareBYOHHosts(fixture.ctx, proof, session); err != nil {
				t.Fatal(err)
			}
			if acquired {
				fixture.sshClientForTest = func(context.Context, *byohHostLease) (*windowsSSHClient, error) {
					return nil, errors.New("independent identity probe unavailable")
				}
			} else {
				ledger, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(fixture.ctx, session.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				owner := *session
				owner.ActiveCase = "another-owner"
				ledger.Annotations[byohSessionAnnotation] = mustBYOHJSON(&owner)
				if _, err := client.CoreV1().ConfigMaps(wmcoNamespace).Update(fixture.ctx, ledger, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				pool, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(fixture.ctx, byohPoolConfigMap, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				pool.Data["192.0.2.1"] = legacyBYOHRecord(false)
				if _, err := client.CoreV1().ConfigMaps(wmcoNamespace).Update(fixture.ctx, pool, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			attachErr := fixture.attachBYOHSession()
			if attachErr == nil {
				t.Fatal("expected case-start validation failure")
			}
			ledger, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), session.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			before, err := decodeBYOHSession(ledger, session.Scope)
			if err != nil {
				t.Fatal(err)
			}
			if (before.ActiveCase == fixture.caseID) != acquired {
				t.Fatalf("unexpected ownership: acquired=%t active=%q case=%q", acquired, before.ActiveCase, fixture.caseID)
			}
			cleanupErr := fixture.cleanup()
			if acquired && !errors.Is(cleanupErr, attachErr) {
				t.Fatalf("cleanup lost primary attach error: attach=%v cleanup=%v", attachErr, cleanupErr)
			}
			ledger, err = client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), session.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			after, err := decodeBYOHSession(ledger, session.Scope)
			if err != nil {
				t.Fatal(err)
			}
			available := 0
			for alias, raw := range ledger.Data {
				entry, err := parseBYOHPoolEntry(alias, raw)
				if err != nil {
					t.Fatal(err)
				}
				if entry.Status == "available" {
					available++
				}
			}
			t.Logf("acquired=%t attachErr=%v cleanupErr=%v phase=%s active=%q availableAliases=%d", acquired, attachErr, cleanupErr, after.Phase, after.ActiveCase, available)
			if acquired && (after.Phase != "Failed" || available != 0) {
				t.Fatal("uncertain case-start host proof was silently settled as a healthy reusable session")
			}
			if !acquired && (after.Phase != before.Phase || after.ActiveCase != before.ActiveCase) {
				t.Fatal("never-acquired cleanup mutated session ownership")
			}
		})
	}
}

func TestBYOHPreflightFreshNodeProjection(t *testing.T) {
	for _, change := range []string{"heartbeat", "empty-rv", "uid", "owner", "version", "provider", "address", "not-ready"} {
		t.Run(change, func(t *testing.T) {
			fixture, client, _ := byohPreflightFixture(t)
			proof, err := fixture.preflightBYOH(context.Background(), "run")
			if err != nil {
				t.Fatal(err)
			}
			node, err := client.CoreV1().Nodes().Get(context.Background(), proof.Hosts[0].Node.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			node.ResourceVersion = "2"
			node.Status.Conditions[0].LastHeartbeatTime = metav1.Now()
			switch change {
			case "empty-rv":
				node.ResourceVersion = ""
			case "uid":
				node.UID = "replacement"
			case "owner":
				node.Annotations["machine.openshift.io/machine"] = "foreign"
			case "version":
				node.Annotations[byohDesiredVersionAnno] = "changed"
			case "provider":
				node.Spec.ProviderID = "foreign"
			case "address":
				node.Status.Addresses[0].Address = "192.0.2.99"
			case "not-ready":
				node.Status.Conditions[0].Status = corev1.ConditionFalse
			}
			if _, err := client.CoreV1().Nodes().UpdateStatus(context.Background(), node, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			err = revalidateBYOHPreflight(context.Background(), fixture, proof)
			if (err == nil) != (change == "heartbeat") {
				t.Fatalf("change=%s revalidation=%v", change, err)
			}
			t.Logf("change=%s revalidation=%v", change, err)
		})
	}
}
