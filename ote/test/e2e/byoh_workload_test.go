package winc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	appsv1defaults "k8s.io/kubernetes/pkg/apis/apps/v1"
)

func TestBYOHWorkloadUnknownUIDProductionCleanup(t *testing.T) {
	for _, scenario := range []string{"unapplied", "applied first lookup failed", "foreign deployment", "mutated spec",
		"replacement namespace", "foreign namespace", "lookup error", "cancelled", "deadline", "concurrent replacement", "concurrent creation",
		"refresh spec drift", "refresh error then drift", "namespace disappears", "fresh resource versions",
		"adoption delete error", "namespace delete error", "namespace lookup error", "namespace replaced on refresh", "refresh not found"} {
		t.Run(scenario, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			namespaceResource := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
			deploymentResource := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
			var attempted *appsv1.Deployment
			client.PrependReactor("create", "namespaces", func(action clienttesting.Action) (bool, runtime.Object, error) {
				namespace := action.(clienttesting.CreateAction).GetObject().(*corev1.Namespace).DeepCopy()
				namespace.UID, namespace.ResourceVersion = "namespace-uid", "namespace-rv"
				if err := client.Tracker().Create(namespaceResource, namespace, ""); err != nil {
					t.Fatal(err)
				}
				return true, namespace, nil
			})
			client.PrependReactor("create", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
				attempted = action.(clienttesting.CreateAction).GetObject().(*appsv1.Deployment).DeepCopy()
				attempted.UID, attempted.ResourceVersion = "deployment-uid", "create-rv"
				appsv1defaults.SetObjectDefaults_Deployment(attempted)
				if scenario != "unapplied" && scenario != "concurrent creation" && scenario != "namespace replaced on refresh" {
					if err := client.Tracker().Create(deploymentResource, attempted, "fixture"); err != nil {
						t.Fatal(err)
					}
				}
				return true, nil, errors.New("create response lost")
			})
			gets, cleanupStarted := 0, false
			client.PrependReactor("get", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
				gets++
				if !cleanupStarted && scenario != "unapplied" && scenario != "concurrent creation" {
					return true, nil, errors.New("first lookup failed")
				}
				if cleanupStarted && scenario == "lookup error" {
					return true, nil, errors.New("lookup unavailable")
				}
				if cleanupStarted && scenario == "fresh resource versions" && (gets == 2 || gets == 3) {
					current := attempted.DeepCopy()
					current.ResourceVersion = "rv-A"
					if gets == 3 {
						current.ResourceVersion = "rv-B"
					}
					return true, current, nil
				}
				if cleanupStarted && scenario == "refresh not found" && gets == 3 {
					return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "deployments"}, byohWorkloadDeployment)
				}
				if cleanupStarted && (scenario == "refresh spec drift" || scenario == "refresh error then drift") && gets >= 3 {
					if scenario == "refresh error then drift" && gets == 3 {
						return true, nil, errors.New("refresh unavailable")
					}
					current := attempted.DeepCopy()
					current.Spec.Template.Spec.Containers[0].Command = []string{"foreign"}
					return true, current, nil
				}
				if cleanupStarted && scenario == "concurrent creation" && gets >= 4 {
					return true, attempted.DeepCopy(), nil
				}
				if cleanupStarted && scenario == "concurrent replacement" && gets >= 3 {
					replacement := attempted.DeepCopy()
					replacement.UID = "replacement-uid"
					return true, replacement, nil
				}
				return false, nil, nil
			})
			fixture := &byohTestFixture{coreClient: client.CoreV1(), appsClient: client.AppsV1(), ctx: context.Background()}
			if err := fixture.createBYOHWebServer("fixture", []*byohHostLease{{NodeName: "node-a", NodeUID: "node-uid"}}); err == nil ||
				fixture.workloadNSUID != "namespace-uid" || fixture.workloadDeploymentUID != "" || !fixture.workloadDeploymentAttempted {
				t.Fatalf("actual create failure did not establish uncertain UID state: %+v err=%v", fixture.workloadDeploymentUID, err)
			}
			cleanupStarted = true
			if scenario == "namespace disappears" || scenario == "namespace lookup error" || scenario == "namespace replaced on refresh" {
				namespaceGets := 0
				client.PrependReactor("get", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
					namespaceGets++
					if scenario == "namespace lookup error" {
						return true, nil, errors.New("namespace lookup unavailable")
					}
					if namespaceGets == 2 {
						if scenario == "namespace replaced on refresh" {
							object, err := client.Tracker().Get(namespaceResource, "", "fixture")
							if err != nil {
								t.Fatal(err)
							}
							current := object.(*corev1.Namespace).DeepCopy()
							current.UID = "replacement-uid"
							return true, current, nil
						}
						return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "fixture")
					}
					return false, nil, nil
				})
			}
			if scenario == "foreign deployment" {
				attempted.Annotations[byohWorkloadOwnershipAnnotation] = "foreign"
			}
			if scenario == "mutated spec" {
				attempted.Spec.Template.Spec.Containers[0].Command = []string{"foreign"}
			}
			attempted.ResourceVersion = "fresh-rv"
			if scenario != "unapplied" && scenario != "concurrent creation" && scenario != "namespace replaced on refresh" {
				if err := client.Tracker().Update(deploymentResource, attempted, "fixture"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "replacement namespace" || scenario == "foreign namespace" {
				object, err := client.Tracker().Get(namespaceResource, "", "fixture")
				if err != nil {
					t.Fatal(err)
				}
				namespace := object.(*corev1.Namespace).DeepCopy()
				if scenario == "replacement namespace" {
					namespace.UID = "replacement-uid"
				} else {
					namespace.Annotations[byohWorkloadOwnershipAnnotation] = "foreign"
				}
				if err := client.Tracker().Update(namespaceResource, namespace, ""); err != nil {
					t.Fatal(err)
				}
			}
			order := []string{}
			for _, resource := range []string{"deployments", "namespaces"} {
				client.PrependReactor("delete", resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
					options := action.(clienttesting.DeleteAction).GetDeleteOptions()
					uid, version := types.UID("namespace-uid"), "namespace-rv"
					if action.GetResource().Resource == "deployments" {
						uid, version = "deployment-uid", "fresh-rv"
						if scenario == "fresh resource versions" {
							version = "rv-B"
						}
					}
					if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != uid ||
						options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != version {
						t.Fatalf("wrong delete preconditions: %+v", options)
					}
					order = append(order, action.GetResource().Resource)
					if scenario == "adoption delete error" && len(order) == 1 {
						return true, nil, errors.New("owned Deployment DELETE not applied")
					}
					if scenario == "namespace delete error" && len(order) == 2 {
						if fixture.workloadDeploymentSpec == nil || fixture.workloadDeploymentAttempted || fixture.workloadDeploymentUID != "" {
							t.Fatal("intermediate Deployment cleanup discarded attempt spec or retained completed identity")
						}
						return true, nil, errors.New("Namespace DELETE not applied")
					}
					return false, nil, nil
				})
			}
			ctx := context.Background()
			if scenario == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			if scenario == "deadline" {
				expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				ctx = expired
			}
			err := fixture.deleteWorkload(ctx)
			success := scenario == "unapplied" || scenario == "applied first lookup failed" || scenario == "fresh resource versions"
			if scenario == "adoption delete error" {
				if err == nil || fixture.workloadDeploymentUID != "deployment-uid" || !fixture.workloadDeploymentAttempted ||
					fixture.workloadDeploymentSpec == nil || !reflect.DeepEqual(order, []string{"deployments"}) {
					t.Fatalf("successful adoption lost identity on failed DELETE: order=%v uid=%q err=%v", order, fixture.workloadDeploymentUID, err)
				}
				if err := fixture.deleteWorkload(ctx); err != nil || !reflect.DeepEqual(order, []string{"deployments", "deployments", "namespaces"}) ||
					fixture.workloadDeploymentSpec != nil || fixture.workloadNS != "" {
					t.Fatalf("established adopted identity retry failed: order=%v err=%v", order, err)
				}
				t.Log("successful full adoption survives failed DELETE and guarded known-UID retry")
				return
			}
			if scenario == "namespace delete error" {
				if err == nil || fixture.workloadDeploymentSpec == nil || fixture.workloadDeploymentAttempted || fixture.workloadDeploymentUID != "" ||
					!reflect.DeepEqual(order, []string{"deployments", "namespaces"}) {
					t.Fatalf("Namespace failure lost intermediate spec: order=%v err=%v", order, err)
				}
				if err := client.Tracker().Create(deploymentResource, attempted, "fixture"); err != nil {
					t.Fatal(err)
				}
				if err := fixture.deleteWorkload(ctx); err == nil || len(order) != 2 || fixture.workloadDeploymentSpec == nil {
					t.Fatalf("retry skipped final child absence guard: order=%v err=%v", order, err)
				}
				if err := client.Tracker().Delete(deploymentResource, "fixture", byohWorkloadDeployment); err != nil {
					t.Fatal(err)
				}
				if err := fixture.deleteWorkload(ctx); err != nil || !reflect.DeepEqual(order, []string{"deployments", "namespaces", "namespaces"}) ||
					fixture.workloadDeploymentSpec != nil || fixture.workloadNS != "" {
					t.Fatalf("fresh absence retry failed: order=%v err=%v", order, err)
				}
				t.Log("spec retained through Deployment deletion and failed Namespace cleanup; observed child blocks retry; fresh absence permits terminal cleanup")
				return
			}
			if !success {
				if scenario == "namespace disappears" {
					if err != nil || !reflect.DeepEqual(order, []string{"deployments"}) || fixture.workloadDeploymentSpec != nil ||
						fixture.workloadNS != "" || fixture.workloadDeploymentAttempted {
						t.Fatalf("terminal Namespace absence retained state: order=%v fixture=%+v err=%v", order, fixture, err)
					}
					return
				}
				if err == nil || len(order) != 0 {
					t.Fatalf("unsafe cleanup: order=%v err=%v", order, err)
				}
				if scenario == "refresh not found" {
					if fixture.workloadDeploymentUID != "" || fixture.workloadDeploymentSpec == nil {
						t.Fatal("fresh NotFound promoted candidate or discarded attempt proof")
					}
					if err := fixture.deleteWorkload(ctx); err != nil || !reflect.DeepEqual(order, []string{"deployments", "namespaces"}) {
						t.Fatalf("fresh full adoption after uncertain refresh failed: order=%v err=%v", order, err)
					}
				}
				if scenario == "refresh spec drift" || scenario == "refresh error then drift" {
					if fixture.workloadDeploymentUID != "" || fixture.workloadDeploymentSpec == nil {
						t.Fatal("incomplete adoption promoted or discarded attempt state")
					}
					if err := fixture.deleteWorkload(ctx); err == nil || len(order) != 0 || fixture.workloadDeploymentUID != "" {
						t.Fatalf("unsafe direct retry: order=%v uid=%q err=%v", order, fixture.workloadDeploymentUID, err)
					}
					fixture.cancel = func() {}
					if err := fixture.cleanupWithBudget(time.Second, time.Millisecond); err == nil || len(order) != 0 || fixture.workloadDeploymentUID != "" {
						t.Fatalf("unsafe actual fixture fallback: order=%v uid=%q err=%v", order, fixture.workloadDeploymentUID, err)
					}
					t.Logf("create response loss -> initial GET failure -> exact cleanup GET -> rejected refresh -> direct retry -> actual cleanup/fallback: deployment GETs=%d deletes=%v UID=%q", gets, order, fixture.workloadDeploymentUID)
				}
				return
			}
			want := []string{"namespaces"}
			if scenario == "applied first lookup failed" || scenario == "fresh resource versions" {
				want = []string{"deployments", "namespaces"}
			}
			if err != nil || !reflect.DeepEqual(order, want) {
				t.Fatalf("reconciled cleanup failed: order=%v want=%v err=%v", order, want, err)
			}
			if gets < 3 {
				t.Fatal("absence was not freshly reconciled")
			}
			if fixture.workloadDeploymentSpec != nil || fixture.workloadDeploymentAttempted || fixture.workloadDeploymentUID != "" ||
				fixture.workloadNS != "" || fixture.workloadNSUID != "" || fixture.workloadOwnershipNonce != "" {
				t.Fatal("successful guarded cleanup left attempt bookkeeping")
			}
			if scenario == "fresh resource versions" {
				t.Log("first exact GET rv-A -> fresh exact GET rv-B -> actual Deployment DELETE UID=deployment-uid RV=rv-B -> Namespace DELETE")
			}
			if err := fixture.deleteWorkload(ctx); err != nil || !reflect.DeepEqual(order, want) {
				t.Fatalf("cleanup not idempotent: %v", err)
			}
			if _, err := client.CoreV1().Namespaces().Get(ctx, "fixture", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatal("Namespace remains")
			}
		})
	}
}

func TestBYOHWorkloadUnknownUIDRequestContext(t *testing.T) {
	for _, operation := range []string{"namespace", "deployment", "refresh"} {
		for _, mode := range []string{"cancel", "deadline", "cap"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				started, stopped, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var mutex sync.Mutex
				var namespace *corev1.Namespace
				var deployment *appsv1.Deployment
				cleanupStarted := false
				deploymentGets, deletes := 0, 0
				var deadlines []time.Time
				var events []string
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					writer.Header().Set("Content-Type", "application/json")
					mutex.Lock()
					events = append(events, request.Method+" "+request.URL.Path)
					mutex.Unlock()
					fail := func() {
						writer.WriteHeader(http.StatusInternalServerError)
						_ = json.NewEncoder(writer).Encode(&metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
							Status: metav1.StatusFailure, Reason: metav1.StatusReasonInternalError, Code: 500, Message: "response lost"})
					}
					block := func() {
						close(started)
						select {
						case <-request.Context().Done():
							close(stopped)
						case <-release:
						}
					}
					switch {
					case request.Method == http.MethodPost && request.URL.Path == "/api/v1/namespaces":
						var candidate corev1.Namespace
						if err := json.NewDecoder(request.Body).Decode(&candidate); err != nil {
							t.Errorf("decode Namespace create: %v", err)
							fail()
							return
						}
						candidate.UID, candidate.ResourceVersion = "namespace-uid", "namespace-rv"
						mutex.Lock()
						namespace = candidate.DeepCopy()
						mutex.Unlock()
						_ = json.NewEncoder(writer).Encode(&candidate)
					case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/deployments"):
						var candidate appsv1.Deployment
						if err := json.NewDecoder(request.Body).Decode(&candidate); err != nil {
							t.Errorf("decode Deployment create: %v", err)
							fail()
							return
						}
						appsv1defaults.SetObjectDefaults_Deployment(&candidate)
						candidate.UID, candidate.ResourceVersion = "deployment-uid", "rv-A"
						mutex.Lock()
						deployment = candidate.DeepCopy()
						mutex.Unlock()
						fail()
					case request.Method == http.MethodGet && request.URL.Path == "/api/v1/namespaces/fixture":
						if operation == "namespace" {
							block()
							return
						}
						mutex.Lock()
						current := namespace.DeepCopy()
						mutex.Unlock()
						_ = json.NewEncoder(writer).Encode(current)
					case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/deployments/"):
						mutex.Lock()
						inCleanup := cleanupStarted
						if inCleanup {
							deploymentGets++
						}
						currentGet, current := deploymentGets, deployment.DeepCopy()
						mutex.Unlock()
						if !inCleanup {
							fail()
							return
						}
						if operation == "deployment" || (operation == "refresh" && currentGet == 2) {
							block()
							return
						}
						_ = json.NewEncoder(writer).Encode(current)
					case request.Method == http.MethodDelete:
						mutex.Lock()
						deletes++
						mutex.Unlock()
						_ = json.NewEncoder(writer).Encode(&metav1.Status{Status: metav1.StatusSuccess})
					default:
						writer.WriteHeader(http.StatusNotFound)
					}
				}))
				defer server.Close()
				defer close(release)
				config := &rest.Config{Host: server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}}
				config.WrapTransport = func(transport http.RoundTripper) http.RoundTripper {
					return byohRequestObserver(func(request *http.Request) (*http.Response, error) {
						mutex.Lock()
						if cleanupStarted && request.Method == http.MethodGet {
							deadline, ok := request.Context().Deadline()
							if !ok || request.Context().Err() != nil {
								t.Error("unknown-UID GET lacks active inherited reconciliation context")
							}
							deadlines = append(deadlines, deadline)
						}
						mutex.Unlock()
						return transport.RoundTrip(request)
					})
				}
				client, err := kubernetes.NewForConfig(config)
				if err != nil {
					t.Fatal(err)
				}
				fixture := &byohTestFixture{coreClient: client.CoreV1(), appsClient: client.AppsV1(), ctx: context.Background()}
				if err := fixture.createBYOHWebServer("fixture", []*byohHostLease{{NodeName: "node-a", NodeUID: "node-uid"}}); err == nil ||
					fixture.workloadDeploymentUID != "" || fixture.workloadDeploymentSpec == nil {
					t.Fatalf("actual applied response loss did not retain unknown attempt: err=%v", err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				if mode != "cancel" {
					cancel()
					limit := time.Minute
					if mode == "deadline" {
						limit = 300 * time.Millisecond
					}
					ctx, cancel = context.WithTimeout(context.Background(), limit)
				}
				defer cancel()
				mutex.Lock()
				cleanupStarted = true
				mutex.Unlock()
				done := make(chan error, 1)
				go func() { done <- fixture.deleteWorkload(ctx) }()
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					cancel()
					t.Fatal("unknown-UID GET never reached HTTP server")
				}
				mutex.Lock()
				observedDeadline := deadlines[len(deadlines)-1]
				mutex.Unlock()
				remaining := time.Until(observedDeadline)
				if remaining <= 0 || remaining > byohHostProcessCleanup {
					t.Errorf("reconciliation request exceeded 30-second cap: %v", remaining)
				}
				if mode == "deadline" {
					outerDeadline, _ := ctx.Deadline()
					if !observedDeadline.Equal(outerDeadline) {
						t.Errorf("cleanup GET detached from outer deadline: got=%v want=%v", observedDeadline, outerDeadline)
					}
				} else {
					if remaining < byohHostProcessCleanup-time.Second {
						t.Errorf("observed reconciliation cap unexpectedly short: %v", remaining)
					}
					cancel()
				}
				select {
				case err = <-done:
				case <-time.After(time.Second):
					t.Fatal("in-flight cleanup GET did not return promptly")
				}
				select {
				case <-stopped:
				case <-time.After(time.Second):
					t.Fatal("server-side cleanup request context did not cancel")
				}
				mutex.Lock()
				defer mutex.Unlock()
				if err == nil || deletes != 0 || fixture.workloadDeploymentUID != "" || fixture.workloadDeploymentSpec == nil ||
					!fixture.workloadDeploymentAttempted || fixture.workloadNSUID != "namespace-uid" {
					t.Fatalf("interrupted read published or deleted attempt: deletes=%d uid=%q err=%v", deletes, fixture.workloadDeploymentUID, err)
				}
				t.Logf("actual GET interrupted; observed deadline remaining=%v; deletes=0 UID empty attempt preserved; events=%v", remaining, events)
			})
		}
	}
}

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
