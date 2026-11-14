package winc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	exutil "github.com/openshift/origin/test/extended/util/compat_otp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type byohProcessSSHSession struct {
	byohFixtureSSHSession
	reset func()
}

func (session *byohProcessSSHSession) CombinedOutput(command string) ([]byte, error) {
	output, err := session.byohFixtureSSHSession.CombinedOutput(command)
	if err == nil && command == "powershell.exe -NoLogo -NoProfile -NonInteractive -EncodedCommand "+encodePowerShell(managedDirectoriesRemovedScript()) {
		session.reset()
	}
	return output, err
}

func TestBYOHSessionSuccessfulCrossProcessLifecycle(t *testing.T) {
	const run = "18d74027-cdc4-4347-8b32-81dd83a83001"
	const auth = "Bearer synthetic-local-test-identity"
	if address := os.Getenv("WMCO_BYOH_SUCCESS_SERVER"); address != "" {
		role, directory := os.Getenv("WMCO_BYOH_SUCCESS_ROLE"), os.Getenv("WMCO_BYOH_SUCCESS_DIR")
		request := func(method, path string, result interface{}) {
			t.Helper()
			req, err := http.NewRequest(method, address+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", auth)
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("test transport %s: %s", path, response.Status)
			}
			if result != nil {
				if err := json.NewDecoder(response.Body).Decode(result); err != nil {
					t.Fatal(err)
				}
			}
		}
		facts := map[string]byohHostFacts{}
		request(http.MethodGet, "/test/facts", &facts)
		cleanup := false
		configure := func(fixture *byohTestFixture) {
			config := &rest.Config{Host: address, BearerToken: "synthetic-local-test-identity", ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}}
			client, err := kubernetes.NewForConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			machineClient, err := dynamic.NewForConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			fixture.coreClient, fixture.appsClient, fixture.dynamicClient = client.CoreV1(), client.AppsV1(), machineClient
			fixture.preflightProbeForTest = func(_ context.Context, lease *byohHostLease) (byohHostFacts, error) {
				request(http.MethodPost, "/test/probe", nil)
				return facts[lease.NodeName], nil
			}
			fixture.preflightLookupForTest = func(_ context.Context, name string) ([]net.IP, error) {
				for _, host := range facts {
					if host.DNS == name {
						return []net.IP{net.ParseIP(host.Addresses[0])}, nil
					}
				}
				return nil, fmt.Errorf("unproved fake DNS")
			}
			fixture.sshClientForTest = func(_ context.Context, lease *byohHostLease) (*windowsSSHClient, error) {
				var selected byohHostFacts
				for _, host := range facts {
					if containsBYOHAddress(host.Addresses, lease.SSHAddress) {
						selected = host
						break
					}
				}
				return &windowsSSHClient{newSession: func() (sshCommandSession, error) {
					return &byohProcessSSHSession{byohFixtureSSHSession: byohFixtureSSHSession{facts: selected}, reset: func() {
						stage := "prepare"
						if cleanup {
							stage = role
						}
						request(http.MethodPost, "/test/reset/"+stage+"/"+selected.DNS, nil)
					}}, nil
				}}, nil
			}
			fixture.waitForLeaseLog = func(ctx context.Context, lease *byohHostLease, message string, _ time.Time) error {
				if message == "instance has been deconfigured" {
					return fixture.coreClient.Nodes().Delete(ctx, lease.NodeName, metav1.DeleteOptions{})
				}
				return nil
			}
		}
		byohSessionTransportForTest = configure
		defer func() { byohSessionTransportForTest = nil }()
		if err := InitializeBYOHSession(); err != nil {
			t.Fatalf("enabled public initializer: %v", err)
		}
		newFixture := func() *byohTestFixture {
			fixture := newBYOHTestFixture(exutil.NewCLIWithoutNamespace("default"))
			configure(fixture)
			return fixture
		}
		waitFile := func(name string) {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
				_, err := os.Stat(filepath.Join(directory, name))
				return err == nil, nil
			}); err != nil {
				t.Fatalf("waiting for %s: %v", name, err)
			}
		}
		signal := func(name string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(directory, name), []byte("observed"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if role == "second" {
			blocked := newFixture()
			blocked.cancel()
			blocked.ctx, blocked.cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
			if err := blocked.attachBYOHSession(); err == nil || blocked.session.ActiveCase == blocked.caseID {
				t.Fatal("second process stole active case")
			}
			blocked.cancel()
			signal("second-blocked")
			waitFile("first-done")
		}
		fixture := newFixture()
		if err := fixture.attachBYOHSession(); err != nil {
			t.Fatalf("actual attachment: %v", err)
		}
		leases, err := fixture.allocateBYOHHosts(byohHostRequest{AddressType: byohAddressIP, RequireTrustedSSH: true})
		if err != nil || len(leases) != 1 {
			t.Fatalf("normalized allocation: leases=%d err=%v", len(leases), err)
		}
		if role == "first" {
			signal("first-attached")
			waitFile("release-first")
		}
		cleanup = true
		if err := fixture.cleanup(); err != nil {
			t.Fatalf("actual reset/cleanup/settlement: %v", err)
		}
		if !leases[0].ResetVerified || !leases[0].CleanupDone {
			t.Fatal("cleanup bypassed reset/release")
		}
		signal(role + "-done")
		t.Logf("process=%s public_initializer=enabled case_attached=true allocation=normalized reset=true cleanup=true", role)
		return
	}
	_, client, facts := byohPreflightFixture(t)
	if err := client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("namespaces"), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster"}}, ""); err != nil {
		t.Fatal(err)
	}
	var mutex sync.Mutex
	var reserved map[string]string
	resets := map[string]int{}
	probes, poolWrites, sessionCreates, handoffs, releases, revision := 0, 0, 0, 0, 0, 1
	scope := byohRunScope{Run: run, PoolUID: "pool", ClusterUID: "cluster"}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		response.Header().Set("Content-Type", "application/json")
		if request.Header.Get("Authorization") != auth {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/test/") {
			switch request.URL.Path {
			case "/test/facts":
				_ = json.NewEncoder(response).Encode(facts)
			case "/test/probe":
				probes++
			default:
				resets[strings.TrimPrefix(request.URL.Path, "/test/reset/")]++
			}
			return
		}
		if strings.HasSuffix(request.URL.Path, "/machines") {
			_ = json.NewEncoder(response).Encode(map[string]interface{}{"apiVersion": "machine.openshift.io/v1beta1", "kind": "MachineList", "items": []interface{}{}})
			return
		}
		parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
		namespace, resource, name := "", "", ""
		if len(parts) >= 5 && parts[2] == "namespaces" {
			namespace, resource = parts[3], parts[4]
			if len(parts) > 5 {
				name = parts[5]
			}
		} else if len(parts) >= 3 {
			resource = parts[2]
			if len(parts) > 3 {
				name = parts[3]
			}
		}
		kind := map[string]string{"nodes": "Node", "pods": "Pod", "configmaps": "ConfigMap", "secrets": "Secret", "namespaces": "Namespace"}[resource]
		gvr := corev1.SchemeGroupVersion.WithResource(resource)
		var result runtime.Object
		var err error
		switch request.Method {
		case http.MethodGet:
			if name == "" {
				result, err = client.Tracker().List(gvr, corev1.SchemeGroupVersion.WithKind(kind), namespace)
			} else {
				result, err = client.Tracker().Get(gvr, namespace, name)
			}
		case http.MethodDelete:
			err = client.Tracker().Delete(gvr, namespace, name)
			result = &metav1.Status{Status: "Success"}
		case http.MethodPost, http.MethodPut:
			cm := &corev1.ConfigMap{}
			if decodeErr := json.NewDecoder(request.Body).Decode(cm); decodeErr != nil {
				t.Error(decodeErr)
				response.WriteHeader(400)
				return
			}
			old, getErr := client.Tracker().Get(gvr, namespace, cm.Name)
			if request.Method == http.MethodPost && getErr == nil {
				err = apierrors.NewAlreadyExists(corev1.Resource(resource), cm.Name)
				break
			}
			if request.Method == http.MethodPut && (getErr != nil || cm.ResourceVersion != old.(*corev1.ConfigMap).ResourceVersion || cm.UID != old.(*corev1.ConfigMap).UID) {
				err = apierrors.NewConflict(corev1.Resource(resource), cm.Name, fmt.Errorf("UID/RV precondition"))
				break
			}
			revision++
			cm.ResourceVersion = strconv.Itoa(revision)
			if cm.UID == "" {
				cm.UID = types.UID("authenticated-session")
				sessionCreates++
			}
			if cm.Name == byohPoolConfigMap {
				poolWrites++
				reserved = copyStringMap(cm.Data)
				if len(reserved) != 3 {
					t.Error("not all three hosts reserved")
				}
				for address, raw := range reserved {
					record, decodeErr := decodeBYOHProducerRecord(address, raw)
					if decodeErr != nil || record.Fields["test-id"] != run || record.Fields["status"] != "allocated" {
						t.Error("raw reservation is not run owned")
					}
				}
			}
			if cm.Name == byohSessionName(scope) {
				owner, decodeErr := decodeBYOHSession(cm, scope)
				if decodeErr != nil {
					t.Error(decodeErr)
				}
				if reserved != nil {
					pool, poolErr := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("configmaps"), wmcoNamespace, byohPoolConfigMap)
					if poolErr != nil || !reflect.DeepEqual(pool.(*corev1.ConfigMap).Data, reserved) {
						t.Error("raw reservations changed at handoff")
					}
				}
				if owner.Phase == "Prepared" {
					for host := range facts {
						if resets["prepare/"+host] == 0 {
							t.Error("Prepared before actual reset proof")
						}
					}
				}
				if getErr == nil {
					previous, _ := decodeBYOHSession(old.(*corev1.ConfigMap), scope)
					if previous.ActiveCase != "" && owner.ActiveCase == "" {
						handoffs++
					}
					for alias, raw := range cm.Data {
						entry, parseErr := parseBYOHPoolEntry(alias, raw)
						before, beforeErr := parseBYOHPoolEntry(alias, old.(*corev1.ConfigMap).Data[alias])
						if parseErr == nil && beforeErr == nil && entry.Status == "available" && before.Status == "releasing" {
							stage := "first"
							if handoffs > 0 {
								stage = "second"
							}
							found := false
							for host, hostFacts := range facts {
								if containsBYOHAddress(hostFacts.Addresses, entry.SSHAddress) && resets[stage+"/"+host] > 0 {
									found = true
								}
							}
							if !found {
								t.Error("private availability preceded actual case reset")
							}
							releases++
						}
					}
				}
			}
			if request.Method == http.MethodPost {
				err = client.Tracker().Create(gvr, cm, namespace)
			} else {
				err = client.Tracker().Update(gvr, cm, namespace)
			}
			result = cm
		default:
			t.Errorf("unexpected fake API operation: %s %s", request.Method, request.URL.Path)
			response.WriteHeader(400)
			return
		}
		if err != nil {
			status := metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Message: err.Error(), Code: 500}
			if apiStatus, ok := err.(apierrors.APIStatus); ok {
				status = apiStatus.Status()
			}
			response.WriteHeader(int(status.Code))
			_ = json.NewEncoder(response).Encode(status)
			return
		}
		if accessor, accessorErr := meta.TypeAccessor(result); accessorErr == nil {
			accessor.SetAPIVersion("v1")
			if name == "" && request.Method == http.MethodGet {
				accessor.SetKind(kind + "List")
			} else {
				accessor.SetKind(kind)
			}
		}
		_ = json.NewEncoder(response).Encode(result)
	}))
	defer server.Close()
	directory := t.TempDir()
	kubeconfig := filepath.Join(directory, "kubeconfig")
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: fake\n  cluster:\n    server: %s\nusers:\n- name: fake\n  user:\n    token: synthetic-local-test-identity\ncontexts:\n- name: fake\n  context:\n    cluster: fake\n    user: fake\ncurrent-context: fake\n", server.URL)
	if err := os.WriteFile(kubeconfig, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	start := func(role string) (*exec.Cmd, *bytes.Buffer) {
		t.Helper()
		command := exec.Command(os.Args[0], "-test.run=^TestBYOHSessionSuccessfulCrossProcessLifecycle$", "-test.timeout=45s", "-test.v")
		command.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig, byohPoolRequiredEnv+"=true", `JOB_SPEC={"prowjobid":"`+run+`"}`, "WMCO_BYOH_SUCCESS_SERVER="+server.URL, "WMCO_BYOH_SUCCESS_ROLE="+role, "WMCO_BYOH_SUCCESS_DIR="+directory)
		output := &bytes.Buffer{}
		command.Stdout, command.Stderr = output, output
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		return command, output
	}
	waitFile := func(name string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if err := wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
			_, err := os.Stat(filepath.Join(directory, name))
			return err == nil, nil
		}); err != nil {
			t.Errorf("process signal %s: %v", name, err)
		}
	}
	first, firstOutput := start("first")
	waitFile("first-attached")
	second, secondOutput := start("second")
	waitFile("second-blocked")
	if err := os.WriteFile(filepath.Join(directory, "release-first"), []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	for index, command := range []*exec.Cmd{first, second} {
		if err := command.Wait(); err != nil {
			t.Errorf("process %d: %v", index+1, err)
		}
	}
	t.Logf("first process:\n%ssecond process:\n%s", firstOutput.String(), secondOutput.String())
	mutex.Lock()
	defer mutex.Unlock()
	if probes != 3 || poolWrites != 1 || sessionCreates != 1 || handoffs != 2 || releases != 4 {
		t.Fatalf("protocol events: probes=%d pool_writes=%d session_creates=%d handoffs=%d alias_releases=%d", probes, poolWrites, sessionCreates, handoffs, releases)
	}
	object, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("configmaps"), wmcoNamespace, byohSessionName(scope))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := decodeBYOHSession(object.(*corev1.ConfigMap), scope)
	if err != nil || owner.Phase != "Prepared" || owner.ActiveCase != "" {
		t.Fatalf("final session: %v", err)
	}
	available := 0
	for alias, raw := range object.(*corev1.ConfigMap).Data {
		entry, err := parseBYOHPoolEntry(alias, raw)
		if err != nil {
			t.Fatal(err)
		}
		if entry.Status == "available" {
			available++
		}
	}
	if available != 6 {
		t.Fatalf("final private available aliases=%d, want six initially reset/unused or case-reset aliases", available)
	}
	t.Logf("authenticated API: prepare_once=1 public_probes=3 raw_reserved_unchanged=3 exclusive_handoffs=%d reset_before_alias_releases=%d", handoffs, releases)
}
