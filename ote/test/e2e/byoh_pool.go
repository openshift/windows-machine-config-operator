package winc

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	"golang.org/x/crypto/ssh"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
)

type byohAddressType string

const (
	byohAddressIP  byohAddressType = "ip"
	byohAddressDNS byohAddressType = "dns"
)

// byohPoolEntry is one producer-owned alias record. trustedHostKey is derived from the serialized
// OpenSSH field and is never learned from the target host.
type byohPoolEntry struct {
	Status, Username         string
	AddressType              byohAddressType
	Platform, TestID         string
	AllocatedAt, LastUpdated string
	HostID, ResetPolicy      string
	Disposable               bool
	SSHAddress               string
	SSHHostPublicKey         string
	trustedHostKey           ssh.PublicKey
}

type byohHostRequest struct {
	AddressType       byohAddressType
	Disposable        bool
	RequireTrustedSSH bool
}

// byohHostLease retains whole-physical-host ownership and every identity needed for guarded API,
// HostProcess, and SSH operations until cleanup settles the host.
type byohHostLease struct {
	Address, Username, SSHAddress string
	AddressType                   byohAddressType
	HostID, LeaseID               string
	Platform, ResetPolicy         string
	AllocatedAt                   string
	Disposable                    bool
	NodeName                      string
	NodeUID                       types.UID
	NodeResourceVersion           string
	ClaimedAliases                []string
	ReconcileStarted              time.Time
	DeconfigurationStarted        time.Time
	RegistrationAttempted         bool
	RegistrationOwned             bool
	Registered, Unregistered      bool
	ResetVerified, CleanupDone    bool
	PreserveInstancesConfigMap    bool
	QuarantineRequired            bool
	TrustedHostKey                ssh.PublicKey
}

// requireBYOHPool is deliberately the first operation in every Batch 9 case. Generic jobs opt out by
// not setting the environment variable; an opted-in job treats every inventory problem as a failure.
func (f *byohTestFixture) requireBYOHPool() error {
	if os.Getenv(byohPoolRequiredEnv) != "true" {
		g.Skip(fmt.Sprintf("%s is not true; the dedicated BYOH pool fixture is not enabled", byohPoolRequiredEnv))
	}
	cm, err := f.coreClient.ConfigMaps(wmcoNamespace).Get(f.ctx, byohPoolConfigMap, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("required BYOH node pool is unavailable: %w", err)
	}
	_, err = validateBYOHPoolInventoryWithTrust(cm.Data, true)
	return err
}

// parseTrustedHostKey accepts exactly one non-certificate OpenSSH public host key. Inventory
// comments, options, multiple keys, and trailing data are rejected so aliases compare one
// unambiguous producer-supplied identity.
func parseTrustedHostKey(value string) (ssh.PublicKey, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(value))
	if err != nil || len(options) != 0 || comment != "" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("pool entry has invalid ssh-host-public-key")
	}
	if _, certificate := key.(*ssh.Certificate); certificate {
		return nil, fmt.Errorf("pool entry ssh-host-public-key cannot be a certificate")
	}
	return key, nil
}

func parseBYOHPoolEntry(address, raw string) (byohPoolEntry, error) {
	fields := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
			return byohPoolEntry{}, fmt.Errorf("pool entry has an invalid field")
		}
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if _, exists := fields[key]; exists {
			return byohPoolEntry{}, fmt.Errorf("pool entry has a duplicate field")
		}
		fields[key] = value
	}
	allowed := map[string]bool{
		"status": true, "username": true, "address-type": true, "platform": true, "test-id": true,
		"allocated-at": true, "last-updated": true, "host-id": true, "reset-policy": true,
		"disposable": true, "ssh-address": true, "ssh-host-public-key": true,
	}
	for key := range fields {
		if !allowed[key] {
			return byohPoolEntry{}, fmt.Errorf("pool entry contains an unsupported field")
		}
	}
	for key := range allowed {
		if key == "ssh-host-public-key" {
			continue
		}
		if _, present := fields[key]; !present {
			return byohPoolEntry{}, fmt.Errorf("pool entry is missing required field %s", key)
		}
	}
	for _, key := range []string{"status", "username", "address-type", "platform", "last-updated", "host-id",
		"reset-policy", "disposable", "ssh-address"} {
		if fields[key] == "" {
			return byohPoolEntry{}, fmt.Errorf("pool entry is missing required field %s", key)
		}
	}
	entry := byohPoolEntry{
		Status: fields["status"], Username: fields["username"], AddressType: byohAddressType(fields["address-type"]),
		Platform: fields["platform"], TestID: fields["test-id"], AllocatedAt: fields["allocated-at"],
		LastUpdated: fields["last-updated"], HostID: fields["host-id"], ResetPolicy: fields["reset-policy"],
		SSHAddress:       fields["ssh-address"],
		SSHHostPublicKey: strings.TrimSpace(fields["ssh-host-public-key"]),
	}
	var err error
	entry.trustedHostKey, err = parseTrustedHostKey(entry.SSHHostPublicKey)
	if err != nil {
		return byohPoolEntry{}, err
	}
	entry.Disposable, err = strconv.ParseBool(fields["disposable"])
	if err != nil {
		return byohPoolEntry{}, fmt.Errorf("pool entry has invalid disposable value")
	}
	if entry.AddressType != byohAddressIP && entry.AddressType != byohAddressDNS {
		return byohPoolEntry{}, fmt.Errorf("pool entry has unsupported address type")
	}
	isIP := net.ParseIP(address) != nil
	if (entry.AddressType == byohAddressIP) != isIP {
		return byohPoolEntry{}, fmt.Errorf("pool entry address does not match its declared type")
	}
	switch entry.Status {
	case "available", "unavailable":
		if entry.TestID != "" || entry.AllocatedAt != "" {
			return byohPoolEntry{}, fmt.Errorf("unowned pool entry contains lease metadata")
		}
	case "allocated", "releasing":
		if entry.TestID == "" || entry.AllocatedAt == "" {
			return byohPoolEntry{}, fmt.Errorf("owned pool entry is missing lease metadata")
		}
		if _, err := time.Parse(time.RFC3339, entry.AllocatedAt); err != nil {
			return byohPoolEntry{}, fmt.Errorf("pool entry has invalid allocation timestamp")
		}
	default:
		return byohPoolEntry{}, fmt.Errorf("pool entry has unsupported status")
	}
	if _, err := time.Parse(time.RFC3339, entry.LastUpdated); err != nil {
		return byohPoolEntry{}, fmt.Errorf("pool entry has invalid update timestamp")
	}
	if !entry.Disposable && entry.ResetPolicy != byohResetPolicy {
		return byohPoolEntry{}, fmt.Errorf("reusable pool entry has incompatible reset policy")
	}
	return entry, nil
}

func validateBYOHPoolInventory(data map[string]string) (map[string]byohPoolEntry, error) {
	return validateBYOHPoolInventoryWithTrust(data, false)
}

func validateBYOHPoolInventoryWithTrust(data map[string]string, requireTrustedSSH bool) (map[string]byohPoolEntry, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("required BYOH node pool is empty")
	}
	entries := make(map[string]byohPoolEntry, len(data))
	for address, raw := range data {
		entry, err := parseBYOHPoolEntry(address, raw)
		if err != nil {
			return nil, fmt.Errorf("required BYOH node pool is malformed: %w", err)
		}
		entries[address] = entry
		if requireTrustedSSH && entry.trustedHostKey == nil {
			return nil, fmt.Errorf("required BYOH node pool entry is missing ssh-host-public-key")
		}
	}
	byHost := map[string]byohPoolEntry{}
	for _, entry := range entries {
		if existing, ok := byHost[entry.HostID]; ok {
			if existing.Username != entry.Username || existing.Platform != entry.Platform ||
				existing.SSHAddress != entry.SSHAddress || existing.Disposable != entry.Disposable ||
				existing.ResetPolicy != entry.ResetPolicy {
				return nil, fmt.Errorf("aliases for one physical host disagree on immutable metadata")
			}
			if (existing.trustedHostKey == nil) != (entry.trustedHostKey == nil) ||
				(existing.trustedHostKey != nil && !bytes.Equal(existing.trustedHostKey.Marshal(), entry.trustedHostKey.Marshal())) {
				return nil, fmt.Errorf("aliases for one physical host disagree on ssh-host-public-key")
			}
			if existing.Status != entry.Status || existing.TestID != entry.TestID ||
				existing.AllocatedAt != entry.AllocatedAt {
				return nil, fmt.Errorf("aliases for one physical host disagree on lease ownership")
			}
		} else {
			byHost[entry.HostID] = entry
		}
	}
	return entries, nil
}

func serializeBYOHPoolEntry(entry byohPoolEntry) string {
	return fmt.Sprintf("status: %s\nusername: %s\naddress-type: %s\nplatform: %s\ntest-id: %s\n"+
		"allocated-at: %s\nlast-updated: %s\nhost-id: %s\nreset-policy: %s\ndisposable: %t\nssh-address: %s\nssh-host-public-key: %s\n",
		entry.Status, entry.Username, entry.AddressType, entry.Platform, entry.TestID, entry.AllocatedAt,
		entry.LastUpdated, entry.HostID, entry.ResetPolicy, entry.Disposable, entry.SSHAddress, entry.SSHHostPublicKey)
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func newBYOHLeaseID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate lease identity: %w", err)
	}
	return fmt.Sprintf("winc-1974-%x", random), nil
}

func claimBYOHHosts(data map[string]string, requests []byohHostRequest, leaseID string,
	now time.Time) (map[string]string, []*byohHostLease, error) {
	requireTrustedSSH := false
	for _, request := range requests {
		requireTrustedSSH = requireTrustedSSH || request.RequireTrustedSSH
	}
	entries, err := validateBYOHPoolInventoryWithTrust(data, requireTrustedSSH)
	if err != nil {
		return nil, nil, err
	}
	addresses := make([]string, 0, len(entries))
	for address := range entries {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	selectedHosts := map[string]bool{}
	leases := make([]*byohHostLease, 0, len(requests))
	for _, request := range requests {
		var selected string
		for _, address := range addresses {
			entry := entries[address]
			if selectedHosts[entry.HostID] || entry.AddressType != request.AddressType ||
				entry.Disposable != request.Disposable || entry.Status != "available" ||
				(request.RequireTrustedSSH && entry.trustedHostKey == nil) {
				continue
			}
			aliasesAvailable := true
			for _, alias := range entries {
				if alias.HostID == entry.HostID && alias.Status != "available" {
					aliasesAvailable = false
					break
				}
			}
			if aliasesAvailable {
				selected = address
				break
			}
		}
		if selected == "" {
			return nil, nil, fmt.Errorf("required BYOH pool has insufficient compatible physical hosts")
		}
		entry := entries[selected]
		aliases := []string{}
		for address, alias := range entries {
			if alias.HostID == entry.HostID {
				aliases = append(aliases, address)
			}
		}
		sort.Strings(aliases)
		leases = append(leases, &byohHostLease{Address: selected, Username: entry.Username,
			SSHAddress: entry.SSHAddress, AddressType: entry.AddressType, HostID: entry.HostID,
			LeaseID: leaseID, Platform: entry.Platform, ResetPolicy: entry.ResetPolicy,
			Disposable: entry.Disposable, ClaimedAliases: aliases,
			TrustedHostKey: entry.trustedHostKey})
		selectedHosts[entry.HostID] = true
	}
	updated := copyStringMap(data)
	stamp := now.UTC().Format(time.RFC3339)
	for _, lease := range leases {
		lease.AllocatedAt = stamp
		for _, alias := range lease.ClaimedAliases {
			entry := entries[alias]
			entry.Status, entry.TestID, entry.AllocatedAt, entry.LastUpdated = "allocated", leaseID, stamp, stamp
			updated[alias] = serializeBYOHPoolEntry(entry)
		}
	}
	return updated, leases, nil
}

func updateBYOHPoolLeases(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	requests []byohHostRequest) ([]*byohHostLease, error) {
	leaseID, err := newBYOHLeaseID()
	if err != nil {
		return nil, err
	}
	var leases []*byohHostLease
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := configMaps.Get(ctx, byohPoolConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		updated, selected, err := claimBYOHHosts(cm.Data, requests, leaseID, time.Now())
		if err != nil {
			return err
		}
		// Retain the selected aliases before attempting the write. A successful API mutation can
		// still be followed by a client-side timeout, and cleanup must know what to reconcile.
		leases = selected
		cm.Data = updated
		if _, err = configMaps.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		claimErr := fmt.Errorf("claim BYOH physical hosts: %w", err)
		if len(leases) == 0 {
			return nil, claimErr
		}
		if reconcileErr := reconcileBYOHPoolClaim(ctx, configMaps, leases); reconcileErr != nil {
			for _, lease := range leases {
				lease.QuarantineRequired = true
			}
			quarantineErr := quarantineBYOHHosts(ctx, configMaps, leases)
			return leases, errors.Join(claimErr,
				fmt.Errorf("reconcile ambiguous BYOH claim: %w", reconcileErr),
				wrapOptionalError("quarantine ambiguous BYOH claim", quarantineErr))
		}
		// The live object proves that the ambiguous request was applied. Return the ownership
		// state together with the original error so the fixture aborts and cleanup unregisters or
		// quarantines it rather than proceeding as though the write were unambiguous.
		return leases, claimErr
	}
	if err := reconcileBYOHPoolClaim(ctx, configMaps, leases); err != nil {
		verifyErr := fmt.Errorf("verify BYOH physical-host claim: %w", err)
		for _, lease := range leases {
			lease.QuarantineRequired = true
		}
		quarantineErr := quarantineBYOHHosts(ctx, configMaps, leases)
		return leases, errors.Join(verifyErr, wrapOptionalError("quarantine failed BYOH claim", quarantineErr))
	}
	return leases, nil
}

func verifyBYOHPoolClaim(data map[string]string, leases []*byohHostLease) error {
	entries, err := validateBYOHPoolInventory(data)
	if err != nil {
		return err
	}
	for _, lease := range leases {
		liveAliases := make([]string, 0, len(lease.ClaimedAliases))
		for address, entry := range entries {
			if entry.HostID == lease.HostID {
				liveAliases = append(liveAliases, address)
			}
		}
		sort.Strings(liveAliases)
		retainedAliases := append([]string(nil), lease.ClaimedAliases...)
		sort.Strings(retainedAliases)
		if !slices.Equal(liveAliases, retainedAliases) {
			return fmt.Errorf("BYOH physical-host alias set changed after claim")
		}
		for _, alias := range retainedAliases {
			entry, ok := entries[alias]
			if !ok || !poolEntryMatchesLease(entry, lease) || entry.Status != "allocated" ||
				entry.TestID != lease.LeaseID || entry.AllocatedAt != lease.AllocatedAt {
				return fmt.Errorf("BYOH physical-host claim ownership mismatch")
			}
		}
	}
	return nil
}

func poolEntryMatchesLease(entry byohPoolEntry, lease *byohHostLease) bool {
	trustMatches := entry.trustedHostKey == nil && lease != nil && lease.TrustedHostKey == nil
	if entry.trustedHostKey != nil && lease != nil && lease.TrustedHostKey != nil {
		trustMatches = bytes.Equal(entry.trustedHostKey.Marshal(), lease.TrustedHostKey.Marshal())
	}
	return lease != nil && entry.HostID == lease.HostID && entry.Username == lease.Username &&
		entry.SSHAddress == lease.SSHAddress && entry.Disposable == lease.Disposable &&
		entry.Platform == lease.Platform && entry.ResetPolicy == lease.ResetPolicy && trustMatches
}

func reconcileBYOHPoolClaim(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease) error {
	var lastErr error
	err := retry.OnError(retry.DefaultBackoff, func(error) bool { return true }, func() error {
		cm, err := configMaps.Get(ctx, byohPoolConfigMap, metav1.GetOptions{})
		if err != nil {
			lastErr = err
			return err
		}
		if err := verifyBYOHPoolClaim(cm.Data, leases); err != nil {
			lastErr = err
			return err
		}
		lastErr = nil
		return nil
	})
	if err != nil {
		return lastErr
	}
	return nil
}

func transitionBYOHHosts(data map[string]string, leases []*byohHostLease, target string,
	clearOwnership bool, now time.Time) (map[string]string, error) {
	entries, err := validateBYOHPoolInventory(data)
	if err != nil {
		return nil, err
	}
	updated := copyStringMap(data)
	stamp := now.UTC().Format(time.RFC3339)
	for _, lease := range leases {
		liveAliases := make([]string, 0, len(lease.ClaimedAliases))
		for address, entry := range entries {
			if entry.HostID == lease.HostID {
				liveAliases = append(liveAliases, address)
			}
		}
		sort.Strings(liveAliases)
		retainedAliases := append([]string(nil), lease.ClaimedAliases...)
		sort.Strings(retainedAliases)
		if !slices.Equal(liveAliases, retainedAliases) {
			return nil, fmt.Errorf("BYOH physical-host alias set changed before transition")
		}
		for _, alias := range lease.ClaimedAliases {
			entry, ok := entries[alias]
			if !ok || !poolEntryMatchesLease(entry, lease) || entry.TestID != lease.LeaseID ||
				entry.AllocatedAt != lease.AllocatedAt ||
				(entry.Status != "allocated" && entry.Status != "releasing") {
				return nil, fmt.Errorf("BYOH host ownership changed before transition")
			}
			entry.Status, entry.LastUpdated = target, stamp
			if clearOwnership {
				entry.TestID, entry.AllocatedAt = "", ""
			}
			updated[alias] = serializeBYOHPoolEntry(entry)
		}
	}
	return updated, nil
}

type byohTransitionState string

const (
	byohTransitionApplied   byohTransitionState = "applied"
	byohTransitionUnapplied byohTransitionState = "unapplied"
)

// inspectBYOHTransition proves an attempted whole-host mutation from a fresh API read. It accepts
// only the exact retained alias set and immutable metadata. A later claimant is deliberately not a
// successful settlement: cleanup must fail closed without ever rewriting that claimant's lease.
func inspectBYOHTransition(data map[string]string, leases []*byohHostLease, target string,
	clearOwnership bool) (byohTransitionState, error) {
	entries, err := validateBYOHPoolInventory(data)
	if err != nil {
		return "", fmt.Errorf("live pool state is malformed or partial: %w", err)
	}
	allApplied, allUnapplied := true, true
	for _, lease := range leases {
		liveAliases := make([]string, 0, len(lease.ClaimedAliases))
		for address, entry := range entries {
			if entry.HostID == lease.HostID {
				liveAliases = append(liveAliases, address)
			}
		}
		sort.Strings(liveAliases)
		retainedAliases := append([]string(nil), lease.ClaimedAliases...)
		sort.Strings(retainedAliases)
		if !slices.Equal(liveAliases, retainedAliases) {
			return "", fmt.Errorf("live BYOH physical-host alias set drifted")
		}
		for _, alias := range retainedAliases {
			entry := entries[alias]
			if !poolEntryMatchesLease(entry, lease) {
				return "", fmt.Errorf("live BYOH physical-host immutable metadata drifted")
			}
			applied := entry.Status == target
			if clearOwnership {
				applied = applied && entry.TestID == "" && entry.AllocatedAt == ""
			} else {
				applied = applied && entry.TestID == lease.LeaseID && entry.AllocatedAt == lease.AllocatedAt
			}
			unapplied := (entry.Status == "allocated" || entry.Status == "releasing") &&
				entry.TestID == lease.LeaseID && entry.AllocatedAt == lease.AllocatedAt
			if entry.TestID != "" && entry.TestID != lease.LeaseID {
				return "", fmt.Errorf("live BYOH physical host has a later or changed owner")
			}
			allApplied = allApplied && applied
			allUnapplied = allUnapplied && unapplied
		}
	}
	if allApplied {
		return byohTransitionApplied, nil
	}
	if allUnapplied {
		return byohTransitionUnapplied, nil
	}
	return "", fmt.Errorf("live BYOH physical-host transition is partial or drifted")
}

func reconcileBYOHTransition(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease, target string, clearOwnership bool) (byohTransitionState, error) {
	cm, err := configMaps.Get(ctx, byohPoolConfigMap, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read live BYOH pool transition state: %w", err)
	}
	return inspectBYOHTransition(cm.Data, leases, target, clearOwnership)
}

func mutateBYOHHostStatus(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease, target string, clearOwnership bool) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := configMaps.Get(ctx, byohPoolConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		updated, err := transitionBYOHHosts(cm.Data, leases, target, clearOwnership, time.Now())
		if err != nil {
			return err
		}
		cm.Data = updated
		_, updateErr := configMaps.Update(ctx, cm, metav1.UpdateOptions{})
		state, reconcileErr := reconcileBYOHTransition(ctx, configMaps, leases, target, clearOwnership)
		if reconcileErr == nil && state == byohTransitionApplied {
			return nil
		}
		if updateErr != nil {
			if apierrors.IsConflict(updateErr) && reconcileErr == nil && state == byohTransitionUnapplied {
				return updateErr
			}
			return errors.Join(fmt.Errorf("update BYOH pool transition: %w", updateErr),
				wrapOptionalError("reconcile BYOH pool transition", reconcileErr))
		}
		if reconcileErr != nil {
			return fmt.Errorf("verify applied BYOH pool transition: %w", reconcileErr)
		}
		return fmt.Errorf("verify applied BYOH pool transition: live transition was %s", state)
	})
}

func releaseBYOHHosts(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease) error {
	for _, lease := range leases {
		if !lease.ResetVerified || lease.Disposable {
			return fmt.Errorf("BYOH physical host cannot be returned without verified reusable reset")
		}
	}
	if err := mutateBYOHHostStatus(ctx, configMaps, leases, "releasing", false); err != nil {
		quarantineErr := quarantineBYOHHosts(ctx, configMaps, leases)
		return errors.Join(fmt.Errorf("mark BYOH physical hosts releasing: %w", err),
			wrapOptionalError("quarantine after release failure", quarantineErr))
	}
	if err := mutateBYOHHostStatus(ctx, configMaps, leases, "available", true); err != nil {
		quarantineErr := quarantineBYOHHosts(ctx, configMaps, leases)
		return errors.Join(fmt.Errorf("release BYOH physical hosts: %w", err),
			wrapOptionalError("quarantine after release failure", quarantineErr))
	}
	for _, lease := range leases {
		lease.CleanupDone = true
	}
	return nil
}

func quarantineBYOHHosts(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease) error {
	if len(leases) == 0 {
		return nil
	}
	err := mutateBYOHHostStatus(ctx, configMaps, leases, "unavailable", true)
	if err == nil {
		for _, lease := range leases {
			lease.CleanupDone = true
		}
	}
	return err
}

func (f *byohTestFixture) allocateBYOHHosts(requests ...byohHostRequest) ([]*byohHostLease, error) {
	leases, err := updateBYOHPoolLeases(f.ctx, f.coreClient.ConfigMaps(wmcoNamespace), requests)
	f.leases = append(f.leases, leases...)
	return leases, err
}
