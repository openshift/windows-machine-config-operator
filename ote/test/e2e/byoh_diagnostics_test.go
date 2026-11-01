package winc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestBYOHDiagnosticValidationBarrier(t *testing.T) {
	for _, failure := range []string{"first-json", "middle-dns", "none"} {
		t.Run(failure, func(t *testing.T) {
			fixture, client, facts := byohPreflightFixture(t)
			fixture.preparationDiagnostic = &byohPreparationDiagnostic{Phase: "validation"}
			if failure == "first-json" {
				fixture.preflightProbeForTest = func(context.Context, *byohHostLease) (byohHostFacts, error) {
					return decodeBYOHHostFacts(`{"addresses":"untrusted-private-value"}`, true)
				}
			}
			if failure == "middle-dns" {
				fixture.preflightLookupForTest = func(_ context.Context, name string) ([]net.IP, error) {
					for _, host := range facts {
						if host.DNS == name && name != "host2.example.test" {
							return []net.IP{net.ParseIP(host.Addresses[0])}, nil
						}
					}
					return nil, fmt.Errorf("untrusted-private-value")
				}
			}
			proof, err := fixture.preflightBYOH(context.Background(), "run")
			diagnostic := fixture.preparationDiagnostic
			if failure == "none" {
				if err != nil || proof == nil || !proof.Validated || diagnostic.Completed != 3 || diagnostic.Phase != "validated" {
					t.Fatalf("missing all-three barrier: %v %+v", err, diagnostic)
				}
			} else {
				if err == nil || proof != nil {
					t.Fatal("failed validation produced an adoption proof")
				}
				if failure == "first-json" && (diagnostic.Host != 1 || diagnostic.Completed != 0 || diagnostic.Stage != "JSON-decode" || diagnostic.Field != "addresses") {
					t.Fatalf("first-host cause lost: %s", diagnostic.summary())
				}
				if failure == "middle-dns" && (diagnostic.Host != 2 || diagnostic.Completed != 1 || diagnostic.Stage != "DNS") {
					t.Fatalf("middle-host progress lost: %s", diagnostic.summary())
				}
				if err := fixture.prepareBYOHHosts(context.Background(), &byohPreflightProof{}, &byohRunSession{}); err == nil {
					t.Fatal("preparation accepted an incomplete validation proof")
				}
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "get" && action.GetVerb() != "list" {
					t.Fatalf("guarded host mutation before all three validated: %s", action.GetVerb())
				}
			}
			t.Log(diagnostic.summary())
		})
	}
}

func persistBYOHDiagnosticForTest(t *testing.T, fixture *byohTestFixture) *byohPreparationDiagnostic {
	t.Helper()
	scope := byohRunScope{Run: "run", PoolUID: "pool", ClusterUID: "cluster"}
	session := &byohRunSession{Scope: scope, Name: byohSessionName(scope), UID: "session", Nonce: "nonce", Phase: "Preparing"}
	maps := fixture.coreClient.ConfigMaps(wmcoNamespace)
	_, err := maps.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: session.Name, UID: session.UID, Annotations: map[string]string{byohSessionAnnotation: mustBYOHJSON(session)}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := *fixture.preparationDiagnostic
	session.Phase, session.Failure = "Failed", &snapshot
	if _, err := fixture.writeBYOHSession(context.Background(), session, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := maps.Get(context.Background(), session.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeBYOHSession(stored, scope)
	if err != nil || decoded.Failure == nil {
		t.Fatalf("failure snapshot unavailable: %v", err)
	}
	message := decoded.Failure.summary()
	if strings.Contains(message, "untrusted-private-value") || strings.ContainsAny(message, "\r\n\x00") || len(message) > 400 {
		t.Fatalf("unsafe retained diagnostic: %q", message)
	}
	t.Logf("persisted: %s pod-create=%s pod-delete=%s", message, decoded.Failure.PodCreate, decoded.Failure.PodDelete)
	return decoded.Failure
}

func TestBYOHDiagnosticPodAttemptIsolation(t *testing.T) {
	first := readyBYOHNode("node-a", "node-a-uid", "192.0.2.1")
	second := readyBYOHNode("node-b", "node-b-uid", "192.0.2.2")
	client := fake.NewSimpleClientset(first, second)
	creates, deletes, secondGets := 0, 0, 0
	client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		creates++
		if creates == 2 {
			return true, nil, errors.New("untrusted-private-value ambiguous create")
		}
		pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		pod.UID, pod.ResourceVersion, pod.Status.Phase = "probe", "2", corev1.PodSucceeded
		if err := client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, wmcoNamespace); err != nil {
			t.Fatal(err)
		}
		return true, pod, nil
	})
	client.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		if deletes == 1 {
			secondGets++
			if secondGets == 3 {
				return true, nil, errors.New("untrusted-private-value readback unavailable")
			}
		}
		return false, nil, nil
	})
	client.PrependReactor("delete", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		deletes++
		options := action.(clienttesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "probe" || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != "2" {
			t.Fatal("cleanup lost UID/RV ownership guards")
		}
		return false, nil, nil
	})
	diagnostic := &byohPreparationDiagnostic{Host: 1, Phase: "validation", Stage: "HostProcess", Fact: "api", Transport: "HostProcess"}
	fixture := &byohTestFixture{coreClient: client.CoreV1(), preparationDiagnostic: diagnostic}
	logs := func(context.Context, string, string, types.UID) ([]byte, error) { return []byte("safe"), nil }
	lease := &byohHostLease{NodeName: first.Name, NodeUID: first.UID, HostID: "first", LeaseID: "run"}
	if _, err := runBYOHHostProcessPSDiagnostic(context.Background(), client.CoreV1(), lease, "safe", logs, diagnostic, true); err != nil || diagnostic.PodDelete != "confirmed" {
		t.Fatalf("first probe must actually complete and prove disappearance: %v %+v", err, diagnostic)
	}
	diagnostic.Host = 2
	lease = &byohHostLease{NodeName: second.Name, NodeUID: second.UID, HostID: "second", LeaseID: "run"}
	_, err := runBYOHHostProcessPSDiagnostic(context.Background(), client.CoreV1(), lease, "safe", logs, diagnostic, true)
	if err == nil || creates != 2 || deletes != 1 || secondGets != 3 || !lease.QuarantineRequired {
		t.Fatalf("wrong ambiguous second-attempt path: err=%v creates=%d deletes=%d gets=%d quarantine=%t", err, creates, deletes, secondGets, lease.QuarantineRequired)
	}
	retained := persistBYOHDiagnosticForTest(t, fixture)
	if retained.Host != 2 || retained.PodCreate != "unknown" || retained.PodDelete != "not-attempted" {
		t.Fatalf("second host inherited absence proof: %+v", retained)
	}
}

func TestBYOHDiagnosticRegistrationAttribution(t *testing.T) {
	for _, failure := range []string{"unresolved", "missing-second"} {
		t.Run(failure, func(t *testing.T) {
			fixture, client, _ := byohPreflightFixture(t)
			instances, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohInstancesConfigMap, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for address := range instances.Data {
				if failure == "unresolved" && net.ParseIP(address) != nil || failure == "missing-second" && (address == "192.0.2.2" || address == "host2.example.test") {
					delete(instances.Data, address)
				}
			}
			if _, err := client.CoreV1().ConfigMaps(wmcoNamespace).Update(context.Background(), instances, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			fixture.preparationDiagnostic = &byohPreparationDiagnostic{Phase: "validation"}
			lookup := fixture.preflightLookupForTest
			registrationLookups := 0
			fixture.preflightLookupForTest = func(ctx context.Context, name string) ([]net.IP, error) {
				if fixture.preparationDiagnostic.Completed == 3 {
					registrationLookups++
					if registrationLookups == 2 && failure == "unresolved" {
						return nil, errors.New("untrusted-private-value")
					}
				}
				return lookup(ctx, name)
			}
			proof, err := fixture.preflightBYOH(context.Background(), "run")
			if err == nil || proof != nil || failure == "unresolved" && registrationLookups != 2 || failure == "missing-second" && err.Error() != "initial host is not registered" {
				t.Fatalf("wrong registration failure path: %v lookups=%d", err, registrationLookups)
			}
			retained := persistBYOHDiagnosticForTest(t, fixture)
			wantHost := 0
			if failure == "missing-second" {
				wantHost = 2
			}
			if retained.Host != wantHost || retained.Stage != "DNS" || retained.Transport != "" || retained.Field != "" {
				t.Fatalf("registration attributed to wrong host or stale probe: %+v want host=%d", retained, wantHost)
			}
		})
	}
}

func TestBYOHDiagnosticSSHFailureAttribution(t *testing.T) {
	for _, failure := range []string{"canceled", "dial-failure", "authentication", "mismatch"} {
		t.Run(failure, func(t *testing.T) {
			fixture, client, _ := byohPreflightFixture(t)
			proof, err := fixture.preflightBYOH(context.Background(), "run")
			if err != nil {
				t.Fatal(err)
			}
			fixture.sshClientForTest = nil
			fixture.preparationDiagnostic = &byohPreparationDiagnostic{Host: 2, Phase: "validation", Stage: "identity", Field: "guid", Transport: "HostProcess"}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			lease := proof.Hosts[1].Lease
			if failure == "canceled" {
				client.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
					cancel()
					return false, nil, nil
				})
			} else if failure == "dial-failure" {
				fixture.sshDialForTest = func(context.Context, string, string) (net.Conn, error) {
					return nil, errors.New("untrusted-private-value host key")
				}
			} else {
				_, hostKey, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				signer, err := ssh.NewSignerFromKey(hostKey)
				if err != nil {
					t.Fatal(err)
				}
				serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
					return nil, errors.New("untrusted-private-value authentication denied")
				}}
				serverConfig.AddHostKey(signer)
				if failure == "authentication" {
					lease.TrustedHostKey = signer.PublicKey()
				}
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				done := make(chan struct{})
				go func() {
					defer close(done)
					connection, err := listener.Accept()
					if err != nil {
						return
					}
					defer connection.Close()
					_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
					server, _, _, err := ssh.NewServerConn(connection, serverConfig)
					if err == nil {
						_ = server.Close()
					}
				}()
				fixture.sshDialForTest = func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
				}
				defer func() {
					_ = listener.Close()
					select {
					case <-done:
					case <-time.After(6 * time.Second):
						t.Error("local SSH server did not terminate")
					}
				}()
			}
			_, err = fixture.sshClient(ctx, &lease)
			if err == nil || strings.Contains(err.Error(), "untrusted-private-value") {
				t.Fatalf("expected safe SSH failure: %v", err)
			}
			fixture.diagnosticFailure(err)
			retained := persistBYOHDiagnosticForTest(t, fixture)
			stage, fact := "SSH", "api"
			if failure == "mismatch" {
				stage, fact = "host-key", "publicKey"
			}
			if retained.Stage != stage || retained.Fact != fact || retained.Transport != "SSH" || retained.Field != "" || errors.Is(err, errBYOHHostKeyMismatch) != (failure == "mismatch") {
				t.Fatalf("incorrect SSH failure attribution: %v %+v", err, retained)
			}
		})
	}
}

func TestBYOHRetainedDiagnosticSanitization(t *testing.T) {
	const sensitive = "sensitive-private-value"
	scope := byohRunScope{Run: "run", PoolUID: "pool", ClusterUID: "cluster"}
	for _, input := range []string{
		`{"Host":1,"Completed":0,"Phase":"validation","Stage":"JSON-decode","Fact":"json-type","Field":"addresses"}`,
		`{"Host":99,"Completed":99,"Phase":"` + sensitive + `","Stage":"DNS\n` + sensitive + `","Fact":"` + sensitive + `","Field":"` + sensitive + `","PodCreate":"` + sensitive + `","arbitrary":"` + sensitive + `"}`,
		`{"Stage":"` + strings.Repeat(sensitive, 2048) + `"}`,
		`{"Host":"` + sensitive + `"}`,
		`{"Stage":`,
	} {
		session := &byohRunSession{Scope: scope, Nonce: "nonce", Phase: "Failed"}
		raw := strings.TrimSuffix(mustBYOHJSON(session), "}") + `,"Failure":` + input + `}`
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{UID: "session", Annotations: map[string]string{byohSessionAnnotation: raw}}}
		decoded, err := decodeBYOHSession(cm, scope)
		message := ""
		if err != nil {
			message = err.Error()
		} else {
			message = decoded.Failure.summary()
			decoded.Failure.checkpoint()
		}
		if strings.Contains(message, sensitive) || strings.ContainsAny(message, "\r\n\x00") || len(message) > 400 {
			t.Fatal("persisted diagnostic leaked untrusted data")
		}
		if strings.Contains(input, `"Fact":"json-type"`) && !strings.Contains(message, "field=addresses") {
			t.Fatal("allowlisted retained cause lost")
		}
	}
	if value := safeBYOHDiagnosticValue("DNS|host-key", "DNS|host-key"); value != "unknown" {
		t.Fatal("non-enum diagnostic accepted")
	}
}
