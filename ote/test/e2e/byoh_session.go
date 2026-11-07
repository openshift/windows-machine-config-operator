package winc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"time"

	"github.com/google/uuid"
	exutil "github.com/openshift/origin/test/extended/util/compat_otp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
)

const byohSessionAnnotation = "windowsmachineconfig.openshift.io/byoh-session"

var byohSessionTransportForTest func(*byohTestFixture)

type byohPreparationError struct {
	summary string
	cause   error
}

func (err *byohPreparationError) Error() string {
	return err.summary
}

func (err *byohPreparationError) Unwrap() error {
	return err.cause
}

type byohRunScope struct {
	Run        string
	PoolUID    types.UID
	ClusterUID types.UID
}

type byohRunSession struct {
	Scope        byohRunScope
	Nonce        string
	Phase        string
	ActiveCase   string
	InstancesUID types.UID
	SecretUID    string
	KeyHash      string
	Reservations map[string]string
	Hosts        []byohPreparedHost
	Failure      *byohPreparationDiagnostic `json:",omitempty"`
	UID          types.UID                  `json:"-"`
	Name         string                     `json:"-"`
}

func byohSessionName(scope byohRunScope) string {
	digest := sha256.Sum256([]byte(string(scope.ClusterUID) + "/" + string(scope.PoolUID)))
	return fmt.Sprintf("winc-byoh-session-%x", digest[:12])
}

func newBYOHNonce() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", random), nil
}

func InitializeBYOHSession() error {
	if os.Getenv(byohPoolRequiredEnv) != "true" {
		return nil
	}
	f := newBYOHTestFixture(exutil.NewCLIWithoutNamespace("default"))
	defer f.cancel()
	if byohSessionTransportForTest != nil {
		byohSessionTransportForTest(f)
	}
	scope, err := f.byohRunScope(f.ctx)
	if err != nil {
		return err
	}
	_, err = f.ensureBYOHSession(f.ctx, scope)
	return err
}

func (f *byohTestFixture) byohRunScope(ctx context.Context) (byohRunScope, error) {
	var spec struct {
		ProwJobID string `json:"prowjobid"`
	}
	if err := json.Unmarshal([]byte(os.Getenv("JOB_SPEC")), &spec); err != nil {
		return byohRunScope{}, fmt.Errorf("BYOH requires a valid Prow run identity")
	}
	if _, err := uuid.Parse(spec.ProwJobID); err != nil {
		return byohRunScope{}, fmt.Errorf("BYOH requires a nonempty Prow run identity")
	}
	if spec.ProwJobID == uuid.Nil.String() {
		return byohRunScope{}, fmt.Errorf("BYOH run identity cannot be zero")
	}
	pool, err := f.coreClient.ConfigMaps(wmcoNamespace).Get(ctx, byohPoolConfigMap, metav1.GetOptions{})
	if err != nil {
		return byohRunScope{}, err
	}
	cluster, err := f.coreClient.Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return byohRunScope{}, err
	}
	if pool.UID == "" || cluster.UID == "" {
		return byohRunScope{}, fmt.Errorf("BYOH requires authenticated pool and cluster identities")
	}
	return byohRunScope{Run: spec.ProwJobID, PoolUID: pool.UID, ClusterUID: cluster.UID}, nil
}

func decodeBYOHSession(cm *corev1.ConfigMap, scope byohRunScope) (*byohRunSession, error) {
	var session byohRunSession
	if cm == nil || cm.UID == "" || len(cm.Annotations[byohSessionAnnotation]) > 1048576 || json.Unmarshal([]byte(cm.Annotations[byohSessionAnnotation]), &session) != nil || session.Scope != scope || session.Nonce == "" {
		return nil, fmt.Errorf("foreign, replaced or incomplete BYOH session")
	}
	session.UID, session.Name = cm.UID, cm.Name
	return &session, nil
}

func (f *byohTestFixture) ensureBYOHSession(ctx context.Context, scope byohRunScope) (*byohRunSession, error) {
	f.preparationDiagnostic = &byohPreparationDiagnostic{Phase: "validation", Stage: "session", Fact: "api", SessionCreate: "not-attempted", SessionUpdate: "not-attempted", PodCreate: "not-attempted", PodDelete: "not-attempted", Reservation: "not-attempted", Adoption: "not-attempted", Withdrawal: "not-attempted", HostMutation: "not-attempted"}
	diagnostic := f.preparationDiagnostic
	maps := f.coreClient.ConfigMaps(wmcoNamespace)
	name := byohSessionName(scope)
	nonce, err := newBYOHNonce()
	if err != nil {
		return nil, err
	}
	session := &byohRunSession{Scope: scope, Nonce: nonce, Phase: "Initializing", Name: name}
	attempt := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: wmcoNamespace, Annotations: map[string]string{byohSessionAnnotation: mustBYOHJSON(session)}}}
	diagnostic.SessionCreate = "attempted"
	diagnostic.checkpoint()
	created, createErr := maps.Create(ctx, attempt, metav1.CreateOptions{})
	if createErr != nil {
		diagnostic.SessionCreate = "unknown"
		diagnostic.checkpoint()
		current, readErr := maps.Get(ctx, name, metav1.GetOptions{})
		if readErr != nil {
			return nil, errors.Join(fmt.Errorf("create/read BYOH session"), readErr)
		}
		observed, err := decodeBYOHSession(current, scope)
		if err != nil {
			return nil, err
		}
		if observed.Nonce != nonce {
			var joined *byohRunSession
			err = wait.PollUntilContextTimeout(ctx, 2*time.Second, 30*time.Minute, true, func(ctx context.Context) (bool, error) {
				cm, err := maps.Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				joined, err = decodeBYOHSession(cm, scope)
				if err != nil {
					return false, err
				}
				if joined.UID != observed.UID || joined.Nonce != observed.Nonce {
					return false, fmt.Errorf("BYOH initializer identity changed")
				}
				switch joined.Phase {
				case "Prepared":
					return true, nil
				case "Failed":
					joined.Failure.checkpoint()
					return false, fmt.Errorf("preparation previously failed: %s; restart prohibited by retained session", joined.Failure.summary())
				}
				return false, nil
			})
			return joined, err
		}
		created = current
	}
	diagnostic.SessionCreate = "confirmed"
	diagnostic.checkpoint()
	if created.UID == "" {
		return nil, fmt.Errorf("BYOH session create returned no UID")
	}
	session.UID = created.UID
	fmt.Fprintln(os.Stderr, "byoh-preflight: validating all three existing hosts before adoption")
	proof, err := f.preflightBYOH(ctx, scope.Run)
	if err == nil && proof.Pool.UID != scope.PoolUID {
		err = fmt.Errorf("pool identity changed during initial preflight")
	}
	if err == nil {
		fmt.Fprintln(os.Stderr, "byoh-prepare: reserving and resetting the three proved hosts")
		err = f.prepareBYOHHosts(ctx, proof, session)
	}
	if err != nil {
		f.diagnosticFailure(err)
		session.Phase = "Failed"
		if session.Failure == nil {
			snapshot := *diagnostic
			snapshot.SessionUpdate = "attempted"
			session.Failure = &snapshot
		}
		_, saveErr := f.writeBYOHSession(ctx, session, nil)
		summary := fmt.Sprintf("BYOH preflight/preparation failed: %s", session.Failure.summary())
		if saveErr != nil {
			summary += "; failed-session-save=unknown"
		}
		return nil, &byohPreparationError{summary: summary, cause: errors.Join(err, saveErr)}
	}
	return session, nil
}

func (f *byohTestFixture) writeBYOHSession(ctx context.Context, session *byohRunSession, data map[string]string) (*corev1.ConfigMap, error) {
	var result *corev1.ConfigMap
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		maps := f.coreClient.ConfigMaps(wmcoNamespace)
		current, err := maps.Get(ctx, session.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		owner, err := decodeBYOHSession(current, session.Scope)
		if err != nil {
			return err
		}
		if current.UID != session.UID || owner.Nonce != session.Nonce || owner.ActiveCase != session.ActiveCase {
			return fmt.Errorf("BYOH session ownership changed")
		}
		current.Annotations[byohSessionAnnotation] = mustBYOHJSON(session)
		if data != nil {
			current.Data = data
		}
		if f.preparationDiagnostic != nil {
			f.preparationDiagnostic.SessionUpdate = "attempted"
			f.preparationDiagnostic.checkpoint()
		}
		result, err = maps.Update(ctx, current, metav1.UpdateOptions{})
		if err == nil {
			if f.preparationDiagnostic != nil {
				f.preparationDiagnostic.SessionUpdate = "confirmed"
				f.preparationDiagnostic.checkpoint()
			}
			return nil
		}
		readback, readErr := maps.Get(ctx, session.Name, metav1.GetOptions{})
		if readErr == nil && readback.UID == session.UID && readback.Annotations[byohSessionAnnotation] == current.Annotations[byohSessionAnnotation] && reflect.DeepEqual(readback.Data, current.Data) {
			result = readback
			if f.preparationDiagnostic != nil {
				f.preparationDiagnostic.SessionUpdate = "confirmed"
				f.preparationDiagnostic.checkpoint()
			}
			return nil
		}
		if f.preparationDiagnostic != nil {
			f.preparationDiagnostic.SessionUpdate = "unknown"
			f.preparationDiagnostic.checkpoint()
		}
		return err
	})
	return result, err
}

func (f *byohTestFixture) prepareBYOHHosts(ctx context.Context, proof *byohPreflightProof, session *byohRunSession) error {
	if proof == nil || !proof.Validated || len(proof.Hosts) != 3 {
		return fmt.Errorf("all three hosts must complete validation before preparation")
	}
	if f.preparationDiagnostic != nil {
		f.preparationDiagnostic.Phase = "preparing"
		f.preparationDiagnostic.Host = 0
		f.preparationDiagnostic.checkpoint()
	}
	if err := revalidateBYOHPreflight(ctx, f, proof); err != nil {
		return err
	}
	maps := f.coreClient.ConfigMaps(wmcoNamespace)
	session.InstancesUID = proof.Instances.UID
	session.SecretUID = proof.SecretUID
	session.KeyHash = proof.KeyHash
	session.Hosts = proof.Hosts
	session.Reservations = map[string]string{}
	stamp := time.Now()
	for address, record := range proof.Records {
		session.Reservations[address] = reserveBYOHProducerRecord(record, session.Scope.Run, stamp)
	}
	session.Phase = "Preparing"
	if _, err := f.writeBYOHSession(ctx, session, nil); err != nil {
		return err
	}
	f.diagnosticStage("reservation", "inventory")
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		pool, err := maps.Get(ctx, byohPoolConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if pool.UID != proof.Pool.UID || !reflect.DeepEqual(pool.Data, proof.Pool.Data) {
			return fmt.Errorf("producer inventory changed before run reservation")
		}
		pool.Data = copyStringMap(session.Reservations)
		if f.preparationDiagnostic != nil {
			f.preparationDiagnostic.Reservation = "attempted"
			f.preparationDiagnostic.checkpoint()
		}
		_, err = maps.Update(ctx, pool, metav1.UpdateOptions{})
		if err == nil {
			if f.preparationDiagnostic != nil {
				f.preparationDiagnostic.Reservation = "confirmed"
				f.preparationDiagnostic.checkpoint()
			}
			return nil
		}
		observed, readErr := maps.Get(ctx, byohPoolConfigMap, metav1.GetOptions{})
		if readErr == nil && observed.UID == pool.UID && reflect.DeepEqual(observed.Data, session.Reservations) {
			if f.preparationDiagnostic != nil {
				f.preparationDiagnostic.Reservation = "confirmed"
				f.preparationDiagnostic.checkpoint()
			}
			return nil
		}
		if f.preparationDiagnostic != nil {
			f.preparationDiagnostic.Reservation = "unknown"
			f.preparationDiagnostic.checkpoint()
		}
		return err
	}); err != nil {
		return err
	}
	preparation := []*byohHostLease{}
	for index := range proof.Hosts {
		host := &proof.Hosts[index]
		for _, alias := range host.RegistrationAliases {
			lease := host.Lease
			lease.Address = alias
			lease.ClaimedAliases = nil
			preparation = append(preparation, &lease)
		}
	}
	state := &byohInstancesState{observed: true, initiallyExisted: true, initialUID: proof.Instances.UID}
	cleanupCtx, _, cancel := newBYOHCleanupContexts(byohCleanupLimit, byohFallbackLimit)
	defer cancel()
	f.diagnosticStage("adoption", "inventory")
	if f.preparationDiagnostic != nil {
		f.preparationDiagnostic.Adoption = "attempted"
		f.preparationDiagnostic.checkpoint()
	}
	if err := adoptBYOHInitialRegistrations(ctx, maps, proof.Instances, preparation, state); err != nil {
		if f.preparationDiagnostic != nil {
			f.preparationDiagnostic.Adoption = "unknown"
			f.preparationDiagnostic.checkpoint()
		}
		cleanupErr := cleanupBYOHPartialAdoption(cleanupCtx, maps, proof.Instances, preparation, state)
		session.Phase = "Failed"
		if f.preparationDiagnostic != nil && session.Failure == nil {
			snapshot := *f.preparationDiagnostic
			snapshot.SessionUpdate = "attempted"
			session.Failure = &snapshot
		}
		_, saveErr := f.writeBYOHSession(cleanupCtx, session, nil)
		return errors.Join(err, cleanupErr, saveErr)
	}
	if f.preparationDiagnostic != nil {
		f.preparationDiagnostic.Adoption = "confirmed"
		f.preparationDiagnostic.checkpoint()
	}
	for _, lease := range preparation {
		captureDeconfigurationBoundary(lease)
	}
	f.diagnosticStage("withdrawal", "inventory")
	if f.preparationDiagnostic != nil {
		f.preparationDiagnostic.Withdrawal = "attempted"
		f.preparationDiagnostic.HostMutation = "unknown"
		f.preparationDiagnostic.checkpoint()
	}
	withdrawErr := unregisterBYOHInstances(cleanupCtx, maps, preparation, state, true, nil, nil)
	var resetErrors []error
	if withdrawErr != nil {
		resetErrors = append(resetErrors, withdrawErr)
		withdrawErr = unregisterBYOHInstances(cleanupCtx, maps, preparation, state, true, nil, nil)
		if withdrawErr != nil {
			resetErrors = append(resetErrors, withdrawErr)
		}
	}
	if f.preparationDiagnostic != nil {
		f.preparationDiagnostic.Withdrawal = "unknown"
		if withdrawErr == nil {
			f.preparationDiagnostic.Withdrawal = "confirmed"
		}
		f.preparationDiagnostic.checkpoint()
	}
	f.diagnosticStage("reset", "identity")
	for index := range proof.Hosts {
		host := &proof.Hosts[index]
		lease := host.Lease
		lease.DeconfigurationStarted = preparation[0].DeconfigurationStarted
		if withdrawErr == nil {
			if err := f.verifyLeaseDeconfigured(cleanupCtx, &lease); err != nil {
				resetErrors = append(resetErrors, err)
			}
		}
		host.Lease.ResetVerified = lease.ResetVerified
	}
	if len(resetErrors) > 0 {
		return errors.Join(resetErrors...)
	}
	if f.preparationDiagnostic != nil {
		f.preparationDiagnostic.HostMutation = "confirmed"
		f.preparationDiagnostic.checkpoint()
	}
	session.Hosts = proof.Hosts
	data := map[string]string{}
	for _, host := range proof.Hosts {
		if !host.Lease.ResetVerified {
			return fmt.Errorf("initial host reset was not proved")
		}
		for _, alias := range host.Lease.ClaimedAliases {
			kind := byohAddressDNS
			if net.ParseIP(alias) != nil {
				kind = byohAddressIP
			}
			entry := byohPoolEntry{Status: "available", Username: host.Lease.Username, AddressType: kind, Platform: host.Lease.Platform, LastUpdated: stamp.UTC().Format(time.RFC3339), HostID: host.Lease.HostID, ResetPolicy: byohResetPolicy, Disposable: host.Lease.Disposable, SSHAddress: host.Lease.SSHAddress, SSHHostPublicKey: host.Facts.PublicKey}
			data[alias] = serializeBYOHPoolEntry(entry)
		}
	}
	session.Phase = "Prepared"
	_, err := f.writeBYOHSession(ctx, session, data)
	return err
}

func adoptBYOHInitialRegistrations(ctx context.Context, maps corev1client.ConfigMapInterface, snapshot *corev1.ConfigMap, leases []*byohHostLease, state *byohInstancesState) error {
	state.initialRegistrations = copyStringMap(snapshot.Data)
	var attempted *corev1.ConfigMap
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current, err := maps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.UID != snapshot.UID {
			return fmt.Errorf("initial registration ConfigMap was replaced")
		}
		if current.ResourceVersion == "" || !reflect.DeepEqual(current.Data, snapshot.Data) {
			return fmt.Errorf("complete initial registration set changed before adoption")
		}
		if attempted != nil && registrationEntriesOwned(current, leases, state) {
			return markRegistrationOwned(current, leases, state)
		}
		for _, lease := range leases {
			key := registrationOwnershipAnnotation(lease.Address)
			if current.Data[lease.Address] != snapshot.Data[lease.Address] || current.Data[lease.Address] != "username="+lease.Username || current.Annotations[key] != snapshot.Annotations[key] || current.Annotations[key] != "" {
				return fmt.Errorf("initial registration ownership changed")
			}
		}
		if err := addRegistrationOwnership(current, leases, state); err != nil {
			return err
		}
		attempted = current.DeepCopy()
		updated, err := maps.Update(ctx, current, metav1.UpdateOptions{})
		if err == nil {
			if updated.UID != snapshot.UID || updated.ResourceVersion == "" || !reflect.DeepEqual(updated.Data, snapshot.Data) {
				return fmt.Errorf("complete registration set changed in adoption response")
			}
			return markRegistrationOwned(updated, leases, state)
		}
		readback, readErr := maps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
		if readErr == nil && readback.ResourceVersion != "" && reflect.DeepEqual(readback.Data, snapshot.Data) && registrationEntriesOwned(readback, leases, state) {
			return markRegistrationOwned(readback, leases, state)
		}
		return err
	})
	return err
}

func cleanupBYOHPartialAdoption(ctx context.Context, maps corev1client.ConfigMapInterface, snapshot *corev1.ConfigMap, leases []*byohHostLease, state *byohInstancesState) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current, err := maps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.UID != snapshot.UID || current.ResourceVersion == "" {
			return fmt.Errorf("partial adoption cleanup lost original ConfigMap identity")
		}
		var owned []*byohHostLease
		for _, lease := range leases {
			if snapshot.Data[lease.Address] == "username="+lease.Username && current.Data[lease.Address] == snapshot.Data[lease.Address] && current.Annotations[registrationOwnershipAnnotation(lease.Address)] == lease.LeaseID {
				owned = append(owned, lease)
			}
		}
		if len(owned) == 0 {
			return nil
		}
		for _, lease := range owned {
			delete(current.Data, lease.Address)
		}
		if err := restoreRegistrationOwnership(current, owned, state); err != nil {
			return err
		}
		_, err = maps.Update(ctx, current, metav1.UpdateOptions{})
		if err != nil {
			readback, readErr := maps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
			if readErr == nil && readback.UID == snapshot.UID && readback.ResourceVersion != "" {
				removed := true
				for _, lease := range owned {
					if _, exists := readback.Data[lease.Address]; exists || readback.Annotations[registrationOwnershipAnnotation(lease.Address)] == lease.LeaseID {
						removed = false
					}
				}
				if removed {
					return nil
				}
			}
		}
		return err
	})
}

type byohSessionPoolClient struct {
	corev1client.ConfigMapInterface
	fixture *byohTestFixture
}

func (f *byohTestFixture) poolClient() corev1client.ConfigMapInterface {
	maps := f.coreClient.ConfigMaps(wmcoNamespace)
	if f.session == nil {
		return maps
	}
	return &byohSessionPoolClient{ConfigMapInterface: maps, fixture: f}
}

func (client *byohSessionPoolClient) Get(ctx context.Context, name string, options metav1.GetOptions) (*corev1.ConfigMap, error) {
	if name != byohPoolConfigMap {
		return nil, fmt.Errorf("session pool client only reads normalized inventory")
	}
	if err := client.fixture.validateBYOHReservation(ctx); err != nil {
		return nil, err
	}
	cm, err := client.ConfigMapInterface.Get(ctx, client.fixture.session.Name, options)
	if err != nil {
		return nil, err
	}
	session, err := decodeBYOHSession(cm, client.fixture.session.Scope)
	if err != nil {
		return nil, err
	}
	if session.UID != client.fixture.session.UID || session.Nonce != client.fixture.session.Nonce || session.Phase != "Prepared" || session.ActiveCase != client.fixture.caseID {
		return nil, fmt.Errorf("private BYOH case ownership changed")
	}
	cm.Name = byohPoolConfigMap
	return cm, nil
}

func (client *byohSessionPoolClient) Update(ctx context.Context, cm *corev1.ConfigMap, options metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	if cm.Name != byohPoolConfigMap || cm.UID != client.fixture.session.UID {
		return nil, fmt.Errorf("invalid normalized pool update")
	}
	if err := client.fixture.validateBYOHReservation(ctx); err != nil {
		return nil, err
	}
	current, err := client.ConfigMapInterface.Get(ctx, client.fixture.session.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	owner, err := decodeBYOHSession(current, client.fixture.session.Scope)
	if err != nil {
		return nil, err
	}
	if current.UID != cm.UID || owner.Nonce != client.fixture.session.Nonce || owner.ActiveCase != client.fixture.caseID || owner.Phase != "Prepared" {
		return nil, fmt.Errorf("private BYOH lease ownership changed")
	}
	if current.ResourceVersion != cm.ResourceVersion {
		return nil, apierrors.NewConflict(corev1.Resource("configmaps"), cm.Name, fmt.Errorf("ledger changed"))
	}
	current.Data = cm.Data
	result, err := client.ConfigMapInterface.Update(ctx, current, options)
	if result != nil {
		result.Name = byohPoolConfigMap
	}
	return result, err
}

func (f *byohTestFixture) validateBYOHReservation(ctx context.Context) error {
	return f.validateBYOHReservationKey(ctx, false)
}

func (f *byohTestFixture) validateBYOHReservationKey(ctx context.Context, allowOwnedKey bool) error {
	session := f.session
	if session == nil {
		return fmt.Errorf("BYOH session is not attached")
	}
	pool, err := f.coreClient.ConfigMaps(wmcoNamespace).Get(ctx, byohPoolConfigMap, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if pool.UID != session.Scope.PoolUID || !reflect.DeepEqual(pool.Data, session.Reservations) {
		return fmt.Errorf("run-owned producer reservation changed")
	}
	secret, err := f.coreClient.Secrets(wmcoNamespace).Get(ctx, byohPrivateKeySecret, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(secret.UID) != session.SecretUID {
		return fmt.Errorf("original Secret identity changed")
	}
	hash, err := publicKeyHash(secret.Data[byohPrivateKeyDataKey])
	ownedHash := fmt.Sprintf("%x", sha256.Sum256([]byte(f.replacementAuthorizedKey)))
	if err != nil || (hash != session.KeyHash && !(allowOwnedKey && f.replacementAuthorizedKey != "" && hash == ownedHash)) {
		return fmt.Errorf("run-owned original Secret key changed")
	}
	return nil
}

func (f *byohTestFixture) attachBYOHSession() error {
	if os.Getenv(byohPoolRequiredEnv) != "true" {
		return f.requireBYOHPool()
	}
	scope, err := f.byohRunScope(f.ctx)
	if err != nil {
		return err
	}
	cm, err := f.coreClient.ConfigMaps(wmcoNamespace).Get(f.ctx, byohSessionName(scope), metav1.GetOptions{})
	if err != nil {
		return err
	}
	session, err := decodeBYOHSession(cm, scope)
	if err != nil {
		return err
	}
	if session.Phase != "Prepared" {
		return fmt.Errorf("BYOH preparation is incomplete or failed")
	}
	f.session = session
	f.caseID, err = newBYOHNonce()
	if err != nil {
		return err
	}
	if err := f.validateBYOHReservation(f.ctx); err != nil {
		return err
	}
	err = wait.PollUntilContextTimeout(f.ctx, 2*time.Second, 30*time.Minute, true, func(ctx context.Context) (bool, error) {
		maps := f.coreClient.ConfigMaps(wmcoNamespace)
		current, err := maps.Get(ctx, session.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		owner, err := decodeBYOHSession(current, scope)
		if err != nil {
			return false, err
		}
		if owner.UID != session.UID || owner.Nonce != session.Nonce || owner.Phase != "Prepared" {
			return false, fmt.Errorf("BYOH session changed while waiting for case")
		}
		if owner.ActiveCase != "" {
			return false, nil
		}
		owner.ActiveCase = f.caseID
		current.Annotations[byohSessionAnnotation] = mustBYOHJSON(owner)
		_, err = maps.Update(ctx, current, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			return false, nil
		}
		if err != nil {
			readback, readErr := maps.Get(ctx, session.Name, metav1.GetOptions{})
			if readErr != nil {
				return false, err
			}
			confirmed, parseErr := decodeBYOHSession(readback, scope)
			if parseErr != nil || confirmed.UID != session.UID || confirmed.ActiveCase != f.caseID {
				return false, err
			}
		}
		f.session.ActiveCase = f.caseID
		return true, nil
	})
	if err != nil {
		return err
	}
	f.instancesState = byohInstancesState{exclusive: true, observed: true, initiallyExisted: true, initialUID: session.InstancesUID}
	f.caseStartErr = f.validateBYOHCaseStart(f.ctx)
	return f.caseStartErr
}

func (f *byohTestFixture) validateBYOHCaseStart(ctx context.Context) error {
	if err := f.validateBYOHReservation(ctx); err != nil {
		return err
	}
	session := f.session
	if len(session.Hosts) != 3 {
		return fmt.Errorf("prepared evidence does not contain exactly three reset hosts")
	}
	for _, host := range session.Hosts {
		if !host.Lease.ResetVerified || host.Lease.NodeUID == "" {
			return fmt.Errorf("initial host reset evidence is incomplete")
		}
	}
	if err := f.validateBYOHIsolation(ctx, session.Hosts); err != nil {
		return err
	}
	instances, err := f.coreClient.ConfigMaps(wmcoNamespace).Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if instances.UID != session.InstancesUID || len(instances.Data) != 0 {
		return fmt.Errorf("registration state changed between BYOH cases")
	}
	secret, err := f.coreClient.Secrets(wmcoNamespace).Get(ctx, byohPrivateKeySecret, metav1.GetOptions{})
	if err != nil {
		return err
	}
	hash, err := publicKeyHash(secret.Data[byohPrivateKeyDataKey])
	if err != nil || hash != session.KeyHash {
		return fmt.Errorf("original key state changed between BYOH cases")
	}
	nodes, err := f.coreClient.Nodes().List(ctx, metav1.ListOptions{LabelSelector: "kubernetes.io/os=windows"})
	if err != nil {
		return err
	}
	if len(nodes.Items) != 0 {
		return fmt.Errorf("unexpected Windows Node between BYOH cases")
	}
	for _, host := range session.Hosts {
		lease := host.Lease
		if _, err := decodeBYOHHostFacts(mustBYOHJSON(host.Facts), true); err != nil {
			return err
		}
		lease.TrustedHostKey, err = parseTrustedHostKey(host.Facts.PublicKey)
		if err != nil {
			return err
		}
		client, err := f.sshClient(ctx, &lease)
		if err != nil {
			return err
		}
		resetErr := f.verifyBYOHCurrentIdentity(ctx, client, &lease, host.Facts.Addresses, host.Lease.ClaimedAliases)
		if resetErr == nil {
			resetErr = assertManagedServicesStopped(ctx, client)
		}
		if resetErr == nil {
			resetErr = assertManagedDirectoriesRemoved(ctx, client)
		}
		closeErr := client.close()
		if resetErr != nil || closeErr != nil {
			return errors.Join(resetErr, closeErr)
		}
	}
	return nil
}

func (f *byohTestFixture) verifyBYOHCurrentIdentity(ctx context.Context, client *windowsSSHClient, lease *byohHostLease, expectedAddresses, aliases []string) error {
	output, err := runPowerShellOverSSH(ctx, client, byohIdentityScript)
	if err != nil {
		return err
	}
	facts, err := decodeBYOHHostFacts(output, false)
	if err != nil {
		return err
	}
	if !sameBYOHPhysicalHost(byohHostFacts{UUID: lease.PhysicalUUID, GUID: lease.MachineGUID, Addresses: expectedAddresses}, facts) {
		return fmt.Errorf("current physical host correlation changed")
	}
	for _, alias := range aliases {
		resolved, err := resolveBYOHAddress(ctx, alias, f.byohResolver())
		if err != nil {
			return err
		}
		for _, ip := range resolved {
			if !containsBYOHAddress(facts.Addresses, ip) {
				return fmt.Errorf("current DNS alias resolves outside the proved host")
			}
		}
	}
	return nil
}

func (f *byohTestFixture) verifyBYOHRegistrationEndpoint(ctx context.Context, lease *byohHostLease) error {
	client, err := f.sshClient(ctx, lease)
	if err != nil {
		return err
	}
	err = f.verifyBYOHCurrentIdentity(ctx, client, lease, []string{lease.SSHAddress}, []string{lease.Address})
	return errors.Join(err, client.close())
}

func (f *byohTestFixture) settleBYOHCase(ctx context.Context, cleanupErr error) error {
	if f.session == nil || f.session.ActiveCase != f.caseID {
		return cleanupErr
	}
	return errors.Join(cleanupErr, retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		maps := f.coreClient.ConfigMaps(wmcoNamespace)
		cm, err := maps.Get(ctx, f.session.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		owner, err := decodeBYOHSession(cm, f.session.Scope)
		if err != nil {
			return err
		}
		if owner.UID != f.session.UID || owner.Nonce != f.session.Nonce || owner.ActiveCase != f.caseID {
			return fmt.Errorf("cannot settle foreign BYOH case")
		}
		if cleanupErr != nil {
			owner.Phase = "Failed"
			for alias, raw := range cm.Data {
				entry, err := parseBYOHPoolEntry(alias, raw)
				if err != nil {
					return err
				}
				entry.Status = "unavailable"
				entry.TestID = ""
				entry.AllocatedAt = ""
				cm.Data[alias] = serializeBYOHPoolEntry(entry)
			}
		}
		owner.ActiveCase = ""
		cm.Annotations[byohSessionAnnotation] = mustBYOHJSON(owner)
		_, err = maps.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	}))
}

func waitForBYOHOriginalNodeAbsent(ctx context.Context, nodes corev1client.NodeInterface, lease *byohHostLease) error {
	if lease.NodeName == "" || lease.NodeUID == "" {
		return fmt.Errorf("initial Node identity is missing")
	}
	return wait.PollUntilContextTimeout(ctx, 10*time.Second, byohDeconfigurationLimit, true, func(ctx context.Context) (bool, error) {
		node, err := nodes.Get(ctx, lease.NodeName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if node.UID != lease.NodeUID {
			return false, fmt.Errorf("unexpected replacement Node during deconfiguration")
		}
		return false, nil
	})
}

func (f *byohTestFixture) verifyBYOHUnusedReset(ctx context.Context, lease *byohHostLease) error {
	if err := f.validateBYOHReservation(ctx); err != nil {
		return err
	}
	instances, err := f.coreClient.ConfigMaps(wmcoNamespace).Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if instances.UID != f.session.InstancesUID || len(instances.Data) != 0 {
		return fmt.Errorf("cannot prove unused host reset with changed registrations")
	}
	nodes, err := f.coreClient.Nodes().List(ctx, metav1.ListOptions{LabelSelector: "kubernetes.io/os=windows"})
	if err != nil {
		return err
	}
	for _, node := range nodes.Items {
		for _, address := range node.Status.Addresses {
			if net.ParseIP(address.Address) != nil && net.ParseIP(address.Address).Equal(net.ParseIP(lease.SSHAddress)) {
				return fmt.Errorf("unused host has an unexpected Node")
			}
		}
	}
	client, err := f.sshClient(ctx, lease)
	if err != nil {
		return err
	}
	resetErr := assertManagedServicesStopped(ctx, client)
	if resetErr == nil {
		resetErr = assertManagedDirectoriesRemoved(ctx, client)
	}
	closeErr := client.close()
	if resetErr != nil || closeErr != nil {
		return errors.Join(resetErr, closeErr)
	}
	lease.ResetVerified = true
	return nil
}

func (f *byohTestFixture) validateBYOHSessionAuthority(ctx context.Context, restoring bool, leases ...*byohHostLease) error {
	if f.session == nil || f.caseID == "" || f.session.ActiveCase != f.caseID {
		return fmt.Errorf("BYOH case authority is missing")
	}
	cm, err := f.coreClient.ConfigMaps(wmcoNamespace).Get(ctx, f.session.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	owner, err := decodeBYOHSession(cm, f.session.Scope)
	if err != nil {
		return err
	}
	if cm.ResourceVersion == "" || owner.UID != f.session.UID || owner.Nonce != f.session.Nonce || owner.ActiveCase != f.caseID || (owner.Phase != "Prepared" && !(restoring && owner.Phase == "Failed")) {
		return fmt.Errorf("live BYOH rotation authority changed")
	}
	if len(leases) > 0 {
		return verifyBYOHPoolClaim(cm.Data, leases)
	}
	return nil
}

func (f *byohTestFixture) validateBYOHRotationIsolation(ctx context.Context, lease *byohHostLease, restoreOwned ...bool) error {
	restoring := len(restoreOwned) > 0 && restoreOwned[0]
	if lease == nil {
		return fmt.Errorf("rotation host authority is missing")
	}
	if err := f.validateBYOHSessionAuthority(ctx, restoring); err != nil {
		return err
	}
	if err := f.validateBYOHIsolation(ctx, []byohPreparedHost{{Lease: *lease}}); err != nil {
		return err
	}
	if !lease.Disposable {
		return fmt.Errorf("key rotation requires the genuine disposable host")
	}
	if err := f.validateBYOHReservationKey(ctx, restoring); err != nil {
		return err
	}
	nodes, err := f.coreClient.Nodes().List(ctx, metav1.ListOptions{LabelSelector: "kubernetes.io/os=windows"})
	if err != nil {
		return err
	}
	if len(nodes.Items) != 1 || nodes.Items[0].UID != lease.NodeUID || nodes.Items[0].Annotations["machine.openshift.io/machine"] != "" {
		return fmt.Errorf("unrelated Windows consumer prevents global Secret rotation")
	}
	cm, err := f.coreClient.ConfigMaps(wmcoNamespace).Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cm.UID != f.session.InstancesUID || len(cm.Data) != 1 || !registrationEntriesOwned(cm, []*byohHostLease{lease}, &f.instancesState) {
		return fmt.Errorf("global Secret rotation registration isolation changed")
	}
	return nil
}

func (f *byohTestFixture) validateBYOHCurrentNode(ctx context.Context, lease *byohHostLease) error {
	if f.session == nil {
		return nil
	}
	node, err := f.coreClient.Nodes().Get(ctx, lease.NodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for _, host := range f.session.Hosts {
		if host.Lease.HostID != lease.HostID {
			continue
		}
		if node.UID != lease.NodeUID || node.Annotations["machine.openshift.io/machine"] != "" || len(node.OwnerReferences) != 0 ||
			(host.InitialProviderID != "" && node.Spec.ProviderID != host.InitialProviderID) {
			return fmt.Errorf("current BYOH Node identity or provider ownership changed")
		}
		for _, address := range node.Status.Addresses {
			if net.ParseIP(address.Address) != nil && containsBYOHAddress(host.Facts.Addresses, address.Address) {
				return nil
			}
		}
		return fmt.Errorf("current BYOH Node has no proved physical-host address")
	}
	return fmt.Errorf("current BYOH Node has no prepared host identity")
}

func (f *byohTestFixture) updateBYOHPrivateKey(ctx context.Context, key []byte) error {
	secrets := f.coreClient.Secrets(wmcoNamespace)
	if f.session == nil {
		return updatePrivateKeySecret(ctx, secrets, byohPrivateKeySecret, byohPrivateKeyDataKey, f.secretSnapshot.UID, key)
	}
	replacementHash := fmt.Sprintf("%x", sha256.Sum256([]byte(f.replacementAuthorizedKey)))
	if f.secretSnapshot == nil {
		return fmt.Errorf("rotation Secret snapshot is missing")
	}
	restoring := reflect.DeepEqual(key, f.secretSnapshot.Data[byohPrivateKeyDataKey])
	targetHash, err := publicKeyHash(key)
	if err != nil || (targetHash != f.originalKeyHash && (f.replacementAuthorizedKey == "" || targetHash != replacementHash)) {
		return fmt.Errorf("rotation target key is not owned")
	}
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := f.validateBYOHRotationIsolation(ctx, f.secretLease, restoring); err != nil {
			return err
		}
		secret, err := secrets.Get(ctx, byohPrivateKeySecret, metav1.GetOptions{})
		if err != nil {
			return err
		}
		hash, hashErr := publicKeyHash(secret.Data[byohPrivateKeyDataKey])
		if secret.UID != f.secretSnapshot.UID || secret.ResourceVersion == "" || hashErr != nil ||
			(hash != f.originalKeyHash && (f.replacementAuthorizedKey == "" || hash != replacementHash)) {
			return fmt.Errorf("refusing to overwrite foreign cloud private key state")
		}
		secret.Data[byohPrivateKeyDataKey] = append([]byte(nil), key...)
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
		if err != nil && privateKeySecretMatches(ctx, secrets, byohPrivateKeySecret, byohPrivateKeyDataKey, f.secretSnapshot.UID, key) == nil {
			return f.validateBYOHSessionAuthority(ctx, restoring)
		}
		return err
	})
	if err != nil {
		return err
	}
	if err := f.validateBYOHSessionAuthority(ctx, restoring); err != nil {
		return err
	}
	return privateKeySecretMatches(ctx, secrets, byohPrivateKeySecret, byohPrivateKeyDataKey, f.secretSnapshot.UID, key)
}
