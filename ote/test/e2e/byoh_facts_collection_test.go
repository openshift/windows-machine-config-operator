package winc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestBYOHFactsCollectionBeforeCleanup(t *testing.T) {
	facts := byohHostFacts{UUID: "18d74027-cdc4-4347-8b32-81dd83a83001", GUID: "18d74027-cdc4-4347-8b32-81dd83a83002", Addresses: []string{"192.0.2.1"}, DNS: "host.example.test", PublicKey: testHostPublicKey(t)}
	valid := mustBYOHJSON(facts)
	for _, entry := range []struct{ name, input, category, decode string }{
		{"empty", "", "json-incomplete", "empty-eof"},
		{"whitespace", " \r\n\t", "json-incomplete", "empty-eof"},
		{"partial", `{"uuid":"`, "json-incomplete", "unexpected-eof"},
		{"mixed-prefix", "diagnostic\r\n" + valid, "json-syntax", "invalid"},
		{"mixed-suffix", valid + "\r\ndiagnostic", "json-trailing-syntax", "invalid"},
		{"oversized", valid + strings.Repeat(" ", 32769), "size", "invalid"},
		{"unknown-field", strings.TrimSuffix(valid, "}") + `,"untrusted":"private-value"}`, "json-unknown-field", "invalid"},
		{"second-object", valid + " {}", "json-trailing-value", "invalid"},
		{"valid", valid, "", "valid"},
		{"multiline", "\r\n" + strings.ReplaceAll(valid, ",", ",\r\n") + "\r\n", "", "valid"},
	} {
		t.Run(entry.name, func(t *testing.T) {
			node := readyBYOHNode("node-a", "node-uid", "192.0.2.1")
			client := fake.NewSimpleClientset(node)
			diagnostic := &byohPreparationDiagnostic{Host: 1, Phase: "validation"}
			fixture := &byohTestFixture{coreClient: client.CoreV1(), preparationDiagnostic: diagnostic}
			creates, reads, deletes := 0, 0, 0
			client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				creates++
				pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
				pod.UID, pod.ResourceVersion, pod.Status.Phase = "probe-uid", "1", corev1.PodSucceeded
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "probe", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed", Message: "private-value", FinishedAt: metav1.Now()}}}}
				if err := client.Tracker().Create(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, pod, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
				return true, pod, nil
			})
			fixture.hostProcessLogsForTest = func(_ context.Context, _, _ string, uid types.UID) ([]byte, error) {
				reads++
				if uid != "probe-uid" {
					t.Fatal("wrong log owner")
				}
				return []byte(entry.input), nil
			}
			client.PrependReactor("delete", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				deletes++
				if entry.category != "" && diagnostic.Stage != "JSON-decode" {
					t.Error("probe evidence deleted before strict facts validation")
				}
				encoded, _ := json.Marshal(diagnostic)
				var snapshot map[string]interface{}
				_ = json.Unmarshal(encoded, &snapshot)
				collection, ok := snapshot["Collection"].(map[string]interface{})
				if !ok || collection["Decode"] != entry.decode || collection["Exit"] != "zero" || collection["Reason"] != "Completed" || collection["Read"] != "complete" {
					t.Error("missing safe first-attempt snapshot before cleanup")
				}
				if ok && (collection["RawBytes"] != float64(min(len(entry.input), 32769)) || collection["TrimmedBytes"] != float64(min(len(strings.TrimSpace(entry.input)), 32769))) {
					t.Error("incorrect bounded byte counts")
				}
				if strings.Contains(string(encoded), "private-value") {
					t.Error("private termination message retained")
				}
				options := action.(clienttesting.DeleteAction).GetDeleteOptions()
				if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "probe-uid" || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != "1" {
					t.Error("cleanup lost UID/RV guards")
				}
				return false, nil, nil
			})
			lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID, LeaseID: "lease", HostID: "host"}
			result, err := fixture.probeBYOHHost(context.Background(), lease)
			if entry.category == "" {
				if err != nil || result.UUID != facts.UUID {
					t.Fatal("complete strict facts rejected")
				}
			} else if err == nil || !strings.Contains(err.Error(), entry.category) || result.UUID != "" {
				t.Fatal("invalid response accepted or partially validated facts escaped")
			}
			if creates != 1 || reads != 1 || deletes != 1 {
				t.Fatalf("attempt counts: %d/%d/%d", creates, reads, deletes)
			}
			for _, action := range client.Actions() {
				if action.GetResource().Resource != "pods" && action.GetVerb() != "get" {
					t.Fatal("preparation mutation after failed facts")
				}
			}
		})
	}
}

func TestBYOHFactsCollectionFailureBarrier(t *testing.T) {
	fixture, client, _ := byohPreflightFixture(t)
	fixture.preflightProbeForTest = nil
	fixture.preparationDiagnostic = &byohPreparationDiagnostic{Phase: "validation"}
	reads := 0
	fixture.hostProcessLogsForTest = func(context.Context, string, string, types.UID) ([]byte, error) {
		reads++
		return []byte(`{"uuid":"`), nil
	}
	client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		pod.UID, pod.ResourceVersion, pod.Status.Phase = "probe-uid", "1", corev1.PodSucceeded
		if err := client.Tracker().Create(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, pod, wmcoNamespace); err != nil {
			t.Fatal(err)
		}
		return true, pod, nil
	})
	proof, err := fixture.preflightBYOH(context.Background(), "run")
	if proof != nil || err == nil || reads != 1 || fixture.preparationDiagnostic.Completed != 0 {
		t.Fatal("incomplete initial facts passed the three-host barrier or retried")
	}
	if err := fixture.prepareBYOHHosts(context.Background(), &byohPreflightProof{}, &byohRunSession{}); err == nil {
		t.Fatal("preparation accepted failed facts")
	}
	for _, action := range client.Actions() {
		if action.GetResource().Resource != "pods" && action.GetVerb() != "get" && action.GetVerb() != "list" {
			t.Fatal("adoption/reservation mutation after failed facts")
		}
	}
}

func TestBYOHFactsCollectionSafeSnapshot(t *testing.T) {
	collection := captureBYOHFactCollection([]byte(" \r\n\t"), &corev1.Pod{}, classifyBYOHHostFactsDecodeError(io.EOF))
	if collection.Decode != "empty-eof" || collection.RawBytes != 4 || collection.TrimmedBytes != 0 || collection.Exit != "unknown" || collection.Reason != "unknown" || collection.Finished != "unknown" {
		t.Fatal("missing metadata was inferred")
	}
	diagnostic := &byohPreparationDiagnostic{Collection: collection}
	snapshot := *diagnostic
	diagnostic.Collection = captureBYOHFactCollection([]byte(`{"uuid":"`), &corev1.Pod{}, classifyBYOHHostFactsDecodeError(io.ErrUnexpectedEOF))
	if snapshot.Collection.Decode != "empty-eof" || diagnostic.Collection.Decode != "unexpected-eof" {
		t.Fatal("original failure snapshot mutated")
	}
	scope := byohRunScope{Run: "run", PoolUID: "pool", ClusterUID: "cluster"}
	session := &byohRunSession{Scope: scope, Nonce: "nonce", Phase: "Failed", Failure: &snapshot}
	decoded, err := decodeBYOHSession(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{UID: "session", Annotations: map[string]string{byohSessionAnnotation: mustBYOHJSON(session)}}}, scope)
	if err != nil || decoded.Failure.Collection.Decode != "empty-eof" || decoded.Failure.Collection.RawBytes != 4 {
		t.Fatal("first failure lost during retained session decode")
	}
	for _, untrusted := range []string{"private-value", strings.Repeat("private-value", 100), "Completed\nprivate-value"} {
		forged := &byohFactCollectionDiagnostic{RawBytes: -1, TrimmedBytes: 999999, Read: untrusted, Decode: untrusted, Exit: untrusted, Reason: untrusted, Finished: untrusted}
		if strings.Contains(forged.summary(), "private-value") || strings.ContainsAny(forged.summary(), "\r\n") {
			t.Fatal("untrusted metadata leaked")
		}
	}
	if !strings.Contains(classifyBYOHHostFactsDecodeError(io.ErrUnexpectedEOF).Error(), "unexpected-eof") || !strings.Contains(classifyBYOHHostFactsDecodeError(io.EOF).Error(), "empty-eof") {
		t.Fatal("empty and partial JSON were conflated")
	}
	if errors.Is(classifyBYOHHostFactsDecodeError(io.EOF), io.EOF) {
		t.Fatal("raw decode error escaped sanitization")
	}
}

func TestBYOHFactsCollectionFailedRead(t *testing.T) {
	fixture, client, _ := byohPreflightFixture(t)
	fixture.preflightProbeForTest = nil
	fixture.preparationDiagnostic = &byohPreparationDiagnostic{Phase: "validation"}
	reads, deletes := 0, 0
	fixture.hostProcessLogsForTest = func(context.Context, string, string, types.UID) ([]byte, error) {
		reads++
		return nil, errors.New("private-value")
	}
	client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		pod.UID, pod.ResourceVersion, pod.Status.Phase = "probe-uid", "1", corev1.PodSucceeded
		if err := client.Tracker().Create(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, pod, wmcoNamespace); err != nil {
			t.Fatal(err)
		}
		return true, pod, nil
	})
	client.PrependReactor("delete", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		deletes++
		collection := fixture.preparationDiagnostic.Collection
		if collection == nil || collection.Read != "failed" || collection.Decode != "unknown" || collection.RawBytes != -1 || collection.TrimmedBytes != -1 || collection.Exit != "unknown" {
			t.Error("failed read was classified as empty JSON or inferred termination")
		}
		return false, nil, nil
	})
	proof, err := fixture.preflightBYOH(context.Background(), "run")
	if err == nil || proof != nil || strings.Contains(err.Error(), "private-value") || reads != 1 || deletes != 1 {
		t.Fatal("unsafe failed-read result or retry")
	}
}
