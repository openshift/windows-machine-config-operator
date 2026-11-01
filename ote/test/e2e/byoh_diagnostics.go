package winc

import (
	"errors"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

type byohPreparationDiagnostic struct {
	Host, Completed                                    int
	Phase, Stage, Fact, Field, Transport               string
	SessionCreate, SessionUpdate, PodCreate, PodDelete string
	Reservation, Adoption, Withdrawal, HostMutation    string
	Collection                                         *byohFactCollectionDiagnostic `json:",omitempty"`
}

type byohFactCollectionDiagnostic struct {
	RawBytes, TrimmedBytes               int
	Read, Decode, Exit, Reason, Finished string
}

func captureBYOHFactCollection(logs []byte, pod *corev1.Pod, decodeErr error) *byohFactCollectionDiagnostic {
	collection := &byohFactCollectionDiagnostic{RawBytes: min(len(logs), 32769),
		TrimmedBytes: min(len(strings.TrimSpace(string(logs))), 32769), Read: "complete", Decode: "valid",
		Exit: "unknown", Reason: "unknown", Finished: "unknown"}
	if decodeErr != nil {
		collection.Decode = "invalid"
		for _, category := range []string{"empty-eof", "unexpected-eof"} {
			if strings.Contains(decodeErr.Error(), ", "+category+")") {
				collection.Decode = category
			}
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "probe" || status.State.Terminated == nil {
			continue
		}
		terminated := status.State.Terminated
		collection.Exit = "nonzero"
		if terminated.ExitCode == 0 {
			collection.Exit = "zero"
		}
		collection.Reason = safeBYOHDiagnosticValue(terminated.Reason, "Completed|Error|OOMKilled|DeadlineExceeded")
		if !terminated.FinishedAt.IsZero() {
			collection.Finished = "observed"
		}
		break
	}
	return collection
}

func (collection *byohFactCollectionDiagnostic) summary() string {
	if collection == nil {
		return "collection=unknown"
	}
	count := func(value int) string {
		if value < 0 || value > 32769 {
			return "unknown"
		}
		return fmt.Sprintf("%d", value)
	}
	return fmt.Sprintf("collection-read=%s raw-bytes=%s trimmed-bytes=%s decode=%s exit=%s reason=%s finished=%s",
		safeBYOHDiagnosticValue(collection.Read, "complete|failed"), count(collection.RawBytes), count(collection.TrimmedBytes),
		safeBYOHDiagnosticValue(collection.Decode, "valid|invalid|empty-eof|unexpected-eof"),
		safeBYOHDiagnosticValue(collection.Exit, "zero|nonzero"),
		safeBYOHDiagnosticValue(collection.Reason, "Completed|Error|OOMKilled|DeadlineExceeded"),
		safeBYOHDiagnosticValue(collection.Finished, "observed"))
}

func safeBYOHDiagnosticValue(value, allowed string) string {
	if len(value) <= 64 && value != "" {
		for _, candidate := range strings.Split(allowed, "|") {
			if value == candidate {
				return value
			}
		}
	}
	return "unknown"
}

func (diagnostic *byohPreparationDiagnostic) summary() string {
	if diagnostic == nil {
		diagnostic = &byohPreparationDiagnostic{}
	}
	host, completed := "unknown", "unknown"
	if diagnostic.Host >= 1 && diagnostic.Host <= 3 {
		host = fmt.Sprintf("%d", diagnostic.Host)
	}
	if diagnostic.Completed >= 0 && diagnostic.Completed <= 3 {
		completed = fmt.Sprintf("%d", diagnostic.Completed)
	}
	phase := safeBYOHDiagnosticValue(diagnostic.Phase, "validation|validated|preparing")
	if phase == "unknown" {
		completed = "unknown"
	}
	stage := safeBYOHDiagnosticValue(diagnostic.Stage, "inventory|HostProcess|SSH|identity|DNS|host-key|JSON-decode|session|reservation|adoption|withdrawal|reset")
	fact := safeBYOHDiagnosticValue(diagnostic.Fact, "inventory|api|identity|uuid|guid|addresses|dns|publicKey|failure|json-syntax|json-incomplete|json-type|json-unknown-field|json-trailing-value|json-trailing-syntax|json-schema|size|capability")
	transport := safeBYOHDiagnosticValue(diagnostic.Transport, "HostProcess|SSH")
	field := safeBYOHDiagnosticValue(diagnostic.Field, "uuid|guid|addresses|dns|publicKey|failure|schema")
	return fmt.Sprintf("host=%s phase=%s stage=%s fact=%s field=%s transport=%s validation=%s/3 all-three-validated=%t", host, phase, stage, fact, field, transport, completed, (phase == "validated" || phase == "preparing") && completed == "3")
}

func (diagnostic *byohPreparationDiagnostic) checkpoint() {
	if diagnostic == nil {
		return
	}
	states := []string{diagnostic.SessionCreate, diagnostic.SessionUpdate, diagnostic.PodCreate, diagnostic.PodDelete, diagnostic.Reservation, diagnostic.Adoption, diagnostic.Withdrawal, diagnostic.HostMutation}
	for index := range states {
		states[index] = safeBYOHDiagnosticValue(states[index], "not-attempted|attempted|confirmed|unknown")
	}
	fmt.Fprintf(os.Stderr, "byoh-diagnostic: %s effects=session-create:%s,session-update:%s,pod-create:%s,pod-delete:%s,reservation:%s,adoption:%s,withdrawal:%s,host-mutation:%s %s\n", diagnostic.summary(), states[0], states[1], states[2], states[3], states[4], states[5], states[6], states[7], diagnostic.Collection.summary())
}

func (f *byohTestFixture) diagnosticStage(stage, fact string) {
	if f.preparationDiagnostic != nil {
		f.preparationDiagnostic.Stage, f.preparationDiagnostic.Fact = stage, fact
	}
}

type byohFactsDiagnosticError struct {
	error
	Stage, Fact, Field string
}

func byohFactsDiagnostic(err error) error {
	if err == nil {
		return nil
	}
	stage, fact := "identity", "identity"
	message := err.Error()
	for _, category := range []string{"json-syntax", "json-incomplete", "json-type", "json-unknown-field", "json-trailing-value", "json-trailing-syntax", "json-schema", "size"} {
		if strings.Contains(message, "("+category) {
			stage, fact = "JSON-decode", category
			break
		}
	}
	switch message {
	case "invalid host system UUID":
		fact = "uuid"
	case "invalid host MachineGuid":
		fact = "guid"
	case "invalid host interface identity":
		fact = "addresses"
	case "missing or invalid public host key":
		stage, fact = "host-key", "publicKey"
	default:
		if strings.HasPrefix(message, "host capability ") {
			stage, fact = "HostProcess", "capability"
		}
	}
	field := safeBYOHDiagnosticValue(fact, "uuid|guid|addresses|dns|publicKey|failure")
	for _, candidate := range []string{"uuid", "guid", "addresses", "dns", "publicKey", "failure", "schema"} {
		if strings.Contains(message, "field "+candidate+")") {
			field = candidate
		}
	}
	return &byohFactsDiagnosticError{error: err, Stage: stage, Fact: fact, Field: field}
}

func (f *byohTestFixture) diagnosticFailure(err error) {
	var facts *byohFactsDiagnosticError
	if errors.As(err, &facts) {
		f.diagnosticStage(facts.Stage, facts.Fact)
		if f.preparationDiagnostic != nil {
			f.preparationDiagnostic.Field = facts.Field
		}
	}
	f.preparationDiagnostic.checkpoint()
}
