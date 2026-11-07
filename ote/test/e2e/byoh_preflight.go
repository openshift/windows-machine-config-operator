package winc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
)

type byohProducerRecord struct {
	Raw    string
	Fields map[string]string
}

type byohHostFacts struct {
	Failure   string   `json:"failure,omitempty"`
	UUID      string   `json:"uuid"`
	GUID      string   `json:"guid"`
	Addresses []string `json:"addresses"`
	DNS       string   `json:"dns"`
	PublicKey string   `json:"publicKey,omitempty"`
}

type byohPreparedHost struct {
	Lease               byohHostLease
	Facts               byohHostFacts
	Node                corev1.Node `json:"-"`
	InitialProviderID   string
	RegistrationAliases []string
}

type byohPreflightProof struct {
	Validated bool
	Pool      *corev1.ConfigMap
	Instances *corev1.ConfigMap
	Records   map[string]byohProducerRecord
	Hosts     []byohPreparedHost
	SecretUID string
	KeyHash   string
}

func decodeBYOHProducerRecord(address, raw string) (byohProducerRecord, error) {
	record := byohProducerRecord{Raw: raw, Fields: map[string]string{}}
	allowed := map[string]bool{"status": true, "username": true, "address-type": true, "platform": true,
		"test-id": true, "allocated-at": true, "last-updated": true, "lastupdated": true, "disposable": true,
		"host-id": true, "reset-policy": true, "ssh-address": true, "ssh-host-public-key": true}
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return record, fmt.Errorf("malformed producer record")
		}
		name, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if _, exists := record.Fields[name]; exists || !allowed[name] {
			return record, fmt.Errorf("duplicate or unsupported producer field")
		}
		record.Fields[name] = value
	}
	for _, name := range []string{"status", "username", "address-type", "platform", "test-id", "allocated-at", "disposable"} {
		value, present := record.Fields[name]
		if !present || (value == "" && name != "test-id" && name != "allocated-at") {
			return record, fmt.Errorf("missing producer field %s", name)
		}
	}
	stamp, updated := record.Fields["last-updated"], record.Fields["lastupdated"]
	if stamp != "" && updated != "" && stamp != updated {
		return record, fmt.Errorf("contradictory producer update timestamps")
	}
	if stamp == "" {
		stamp = updated
	}
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		return record, fmt.Errorf("invalid producer update timestamp")
	}
	if _, err := strconv.ParseBool(record.Fields["disposable"]); err != nil {
		return record, fmt.Errorf("invalid producer disposable flag")
	}
	if strings.ContainsAny(record.Fields["username"], "\r\n=\x00") {
		return record, fmt.Errorf("invalid producer username")
	}
	kind := record.Fields["address-type"]
	if (kind != "ip" && kind != "dns") || ((net.ParseIP(address) != nil) != (kind == "ip")) || strings.TrimSpace(address) != address || address == "" {
		return record, fmt.Errorf("invalid producer address type")
	}
	switch record.Fields["status"] {
	case "available", "unavailable":
		if record.Fields["test-id"] != "" || record.Fields["allocated-at"] != "" {
			return record, fmt.Errorf("unowned producer record has claim")
		}
	case "allocated", "releasing":
		if record.Fields["test-id"] == "" {
			return record, fmt.Errorf("owned producer record has no claim")
		}
		if _, err := time.Parse(time.RFC3339, record.Fields["allocated-at"]); err != nil {
			return record, fmt.Errorf("owned producer record has invalid timestamp")
		}
	default:
		return record, fmt.Errorf("invalid producer status")
	}
	if value, present := record.Fields["ssh-host-public-key"]; present {
		key, err := parseTrustedHostKey(value)
		if err != nil || key == nil {
			return record, fmt.Errorf("invalid supplied producer host key")
		}
	}
	for _, name := range []string{"host-id", "reset-policy", "ssh-address"} {
		if value, present := record.Fields[name]; present && value == "" {
			return record, fmt.Errorf("provided producer field %s is empty", name)
		}
	}
	return record, nil
}

func reserveBYOHProducerRecord(record byohProducerRecord, run string, now time.Time) string {
	lines := strings.Split(record.Raw, "\n")
	for index, line := range lines {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		value := ""
		switch strings.TrimSpace(parts[0]) {
		case "status":
			value = "allocated"
		case "test-id":
			value = run
		case "allocated-at", "last-updated", "lastupdated":
			value = now.UTC().Format(time.RFC3339)
		default:
			continue
		}
		lines[index] = parts[0] + ": " + value
	}
	return strings.Join(lines, "\n")
}

const byohIdentityScript = `$ErrorActionPreference='Stop';
$uuid=[string](Get-CimInstance Win32_ComputerSystemProduct).UUID;
$guid=[string](Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Cryptography').MachineGuid;
$addresses=@(Get-NetIPAddress -AddressFamily IPv4 | Where-Object {$_.IPAddress -ne '127.0.0.1'} | ForEach-Object {$_.IPAddress});
$dns=[System.Net.Dns]::GetHostEntry($env:COMPUTERNAME).HostName;
@{uuid=$uuid;guid=$guid;addresses=$addresses;dns=$dns} | ConvertTo-Json -Compress`

const byohPublicHostKeyScript = `$ErrorActionPreference='Stop';
try {
$service=Get-CimInstance Win32_Service -Filter "Name='sshd'";
if($service.State -ne 'Running'){throw 'sshd-not-running'};
$command=$service.PathName;
if($command -notmatch '^"?([A-Za-z]:\\[^"\r\n]*?\\sshd\.exe)"?\s*$'){throw 'unsupported-sshd-service-command'};
$exe=$Matches[1];
$effective=@(& $exe -T 2>$null);if($LASTEXITCODE -ne 0){throw 'sshd-effective-config'};
if(@($effective | Where-Object {$_ -match '^hostcertificate\s+(?!none\s*$)'}).Count -ne 0){throw 'host-certificate-not-supported'};
$keys=@($effective | Where-Object {$_ -match '^hostkey\s+'} | ForEach-Object {($_ -replace '^hostkey\s+','').Replace('/','\')});
$allowed=@('C:\ProgramData\ssh\ssh_host_ed25519_key','C:\ProgramData\ssh\ssh_host_ecdsa_key','C:\ProgramData\ssh\ssh_host_rsa_key');
$selected=@($allowed | Where-Object {$keys -contains $_});
if($selected.Count -eq 0){throw 'no-allowlisted-active-hostkey'};
$path=$selected[0]+'.pub';
foreach($part in @('C:\ProgramData','C:\ProgramData\ssh',$path)){
 $item=Get-Item -LiteralPath $part -Force;if(($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0){throw 'hostkey-reparse'}
};
$file=Get-Item -LiteralPath $path;if($file.Length -lt 32 -or $file.Length -gt 16384){throw 'public-key-size'};
$line=([IO.File]::ReadAllText($path)).Trim();
if($line -notmatch '^(ssh-ed25519|ecdsa-sha2-nistp256|ecdsa-sha2-nistp384|ecdsa-sha2-nistp521|ssh-rsa) ([A-Za-z0-9+/]+={0,2})( [^\r\n]*)?$'){throw 'public-key-syntax'};
$algorithm=$Matches[1];$encoded=$Matches[2];$wire=[Convert]::FromBase64String($encoded);
if($wire.Length -lt 8 -or $wire.Length -gt 12288){throw 'public-key-wire-size'};
$length=([int]$wire[0]*16777216)+([int]$wire[1]*65536)+([int]$wire[2]*256)+[int]$wire[3];
if($length -ne $algorithm.Length -or [Text.Encoding]::ASCII.GetString($wire,4,$length) -ne $algorithm){throw 'public-key-wire-type'};
$script:cursor=4+$length;
function Read-PublicField {
 if($script:cursor+4 -gt $wire.Length){throw 'public-field-header'};
 $size=([int]$wire[$script:cursor]*16777216)+([int]$wire[$script:cursor+1]*65536)+([int]$wire[$script:cursor+2]*256)+[int]$wire[$script:cursor+3];
 $script:cursor+=4;if($size -lt 1 -or $size -gt 8192 -or $script:cursor+$size -gt $wire.Length){throw 'public-field-size'};
 $field=$wire[$script:cursor..($script:cursor+$size-1)];$script:cursor+=$size;return ,$field
};
switch($algorithm){
 'ssh-ed25519' {if((Read-PublicField).Length -ne 32){throw 'ed25519-public-size'}}
 'ssh-rsa' {$exponent=Read-PublicField;$modulus=Read-PublicField;if($exponent.Length -gt 8 -or $modulus.Length -lt 32 -or $modulus.Length -gt 1025){throw 'rsa-public-size'}}
 default {$curve=Read-PublicField;$point=Read-PublicField;$curveName=$algorithm.Substring('ecdsa-sha2-'.Length);$pointSize=@{nistp256=65;nistp384=97;nistp521=133}[$curveName];if([Text.Encoding]::ASCII.GetString($curve) -ne $curveName -or $point.Length -ne $pointSize -or $point[0] -ne 4){throw 'ecdsa-public-shape'}}
};
if($script:cursor -ne $wire.Length){throw 'trailing-nonpublic-data'};
$uuid=[string](Get-CimInstance Win32_ComputerSystemProduct).UUID;
$guid=[string](Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Cryptography').MachineGuid;
$addresses=@(Get-NetIPAddress -AddressFamily IPv4 | Where-Object {$_.IPAddress -ne '127.0.0.1'} | ForEach-Object {$_.IPAddress});
$dns=[System.Net.Dns]::GetHostEntry($env:COMPUTERNAME).HostName;
foreach($name in @('kubelet','containerd','windows-instance-config-daemon')){if((Get-Service $name).Status -ne 'Running'){throw 'managed-service-not-running'}};
if(!(Test-Path -LiteralPath 'C:\k\containerd' -PathType Container)){throw 'containerd-directory-missing'};
@{uuid=$uuid;guid=$guid;addresses=$addresses;dns=$dns;publicKey=($algorithm+' '+$encoded)} | ConvertTo-Json -Compress
} catch {
 $reason=$_.Exception.Message;
 if($reason -notin @('sshd-not-running','unsupported-sshd-service-command','sshd-effective-config','host-certificate-not-supported','no-allowlisted-active-hostkey','hostkey-reparse','public-key-size','public-key-syntax','public-key-wire-size','public-key-wire-type','public-field-header','public-field-size','ed25519-public-size','rsa-public-size','ecdsa-public-shape','trailing-nonpublic-data','managed-service-not-running','containerd-directory-missing')){$reason='public-host-probe'};
 @{failure=$reason} | ConvertTo-Json -Compress
}`

func decodeBYOHHostFacts(output string, requireKey bool) (result byohHostFacts, retErr error) {
	defer func() { retErr = byohFactsDiagnostic(retErr) }()
	var facts byohHostFacts
	if len(output) > 32768 {
		return facts, fmt.Errorf("host probe output exceeds bound (size)")
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&facts); err != nil {
		return facts, classifyBYOHHostFactsDecodeError(err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return facts, fmt.Errorf("trailing host facts (json-trailing-value)")
		}
		return facts, fmt.Errorf("trailing host facts (json-trailing-syntax)")
	}
	if facts.Failure != "" {
		allowed := []string{"sshd-not-running", "unsupported-sshd-service-command", "sshd-effective-config", "host-certificate-not-supported", "no-allowlisted-active-hostkey", "hostkey-reparse", "public-key-size", "public-key-syntax", "public-key-wire-size", "public-key-wire-type", "public-field-header", "public-field-size", "ed25519-public-size", "rsa-public-size", "ecdsa-public-shape", "trailing-nonpublic-data", "managed-service-not-running", "containerd-directory-missing", "public-host-probe"}
		for _, reason := range allowed {
			if facts.Failure == reason {
				return facts, fmt.Errorf("host capability failed (capability: %s)", reason)
			}
		}
		return facts, fmt.Errorf("host capability returned an invalid failure class (capability-unknown)")
	}
	if _, err := uuid.Parse(facts.UUID); err != nil {
		return facts, fmt.Errorf("invalid host system UUID")
	}
	if _, err := uuid.Parse(facts.GUID); err != nil {
		return facts, fmt.Errorf("invalid host MachineGuid")
	}
	if facts.UUID == uuid.Nil.String() || facts.GUID == uuid.Nil.String() || len(facts.Addresses) == 0 || len(facts.Addresses) > 64 {
		return facts, fmt.Errorf("incomplete host identity")
	}
	seen := map[string]bool{}
	for _, address := range facts.Addresses {
		ip := net.ParseIP(address)
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || seen[address] {
			return facts, fmt.Errorf("invalid host interface identity")
		}
		seen[address] = true
	}
	if requireKey {
		key, err := parseTrustedHostKey(facts.PublicKey)
		if err != nil || key == nil {
			return facts, fmt.Errorf("missing or invalid public host key")
		}
	}
	return facts, nil
}

func classifyBYOHHostFactsDecodeError(err error) error {
	var syntax *json.SyntaxError
	var mismatch *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return fmt.Errorf("invalid public host facts (json-syntax, offset %d)", syntax.Offset)
	case errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("invalid public host facts (json-incomplete, unexpected-eof)")
	case errors.Is(err, io.EOF):
		return fmt.Errorf("invalid public host facts (json-incomplete, empty-eof)")
	case errors.As(err, &mismatch):
		field := "schema"
		switch mismatch.Field {
		case "uuid", "guid", "addresses", "dns", "publicKey", "failure":
			field = mismatch.Field
		}
		return fmt.Errorf("invalid public host facts (json-type, field %s)", field)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return fmt.Errorf("invalid public host facts (json-unknown-field)")
	default:
		return fmt.Errorf("invalid public host facts (json-schema)")
	}
}

func sameBYOHPhysicalHost(expected, actual byohHostFacts) bool {
	if !strings.EqualFold(expected.UUID, actual.UUID) || !strings.EqualFold(expected.GUID, actual.GUID) {
		return false
	}
	for _, address := range expected.Addresses {
		if !containsBYOHAddress(actual.Addresses, address) {
			return false
		}
	}
	return true
}

func containsBYOHAddress(addresses []string, address string) bool {
	for _, candidate := range addresses {
		if net.ParseIP(candidate).Equal(net.ParseIP(address)) {
			return true
		}
	}
	return false
}

func resolveBYOHAddress(ctx context.Context, address string, lookup byohLookupIP) ([]string, error) {
	if ip := net.ParseIP(address); ip != nil {
		return []string{ip.String()}, nil
	}
	ips, err := lookup(ctx, address)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("DNS resolution failed for selected registration")
	}
	result := []string{}
	for _, ip := range ips {
		result = append(result, ip.String())
	}
	return result, nil
}

func (f *byohTestFixture) byohResolver() byohLookupIP {
	if f.preflightLookupForTest != nil {
		return f.preflightLookupForTest
	}
	return func(ctx context.Context, address string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip4", address)
	}
}

func (f *byohTestFixture) probeBYOHHost(ctx context.Context, lease *byohHostLease) (byohHostFacts, error) {
	f.diagnosticStage("HostProcess", "api")
	if f.preparationDiagnostic != nil {
		f.preparationDiagnostic.Transport = "HostProcess"
		f.preparationDiagnostic.Collection = nil
		f.preparationDiagnostic.Field = ""
	}
	if f.preflightProbeForTest != nil {
		return f.preflightProbeForTest(ctx, lease)
	}
	var facts byohHostFacts
	_, err := f.runConfiguredHostProcessPSValidated(ctx, lease, byohPublicHostKeyScript,
		func(logs []byte, pod *corev1.Pod, readErr error) error {
			if readErr != nil {
				if f.preparationDiagnostic != nil {
					f.preparationDiagnostic.Collection = captureBYOHFactCollection(nil, pod, nil)
					f.preparationDiagnostic.Collection.Read, f.preparationDiagnostic.Collection.Decode = "failed", "unknown"
					f.preparationDiagnostic.Collection.RawBytes, f.preparationDiagnostic.Collection.TrimmedBytes = -1, -1
					f.preparationDiagnostic.checkpoint()
				}
				return readErr
			}
			validated, decodeErr := decodeBYOHHostFacts(string(logs), true)
			if f.preparationDiagnostic != nil {
				f.preparationDiagnostic.Collection = captureBYOHFactCollection(logs, pod, decodeErr)
				if decodeErr != nil {
					f.diagnosticFailure(decodeErr)
				}
			}
			if decodeErr == nil {
				facts = validated
			}
			return decodeErr
		}, true)
	if err != nil {
		return byohHostFacts{}, fmt.Errorf("authenticated public-key/capability probe: %w", err)
	}
	return facts, nil
}

func (f *byohTestFixture) preflightBYOH(ctx context.Context, run string) (result *byohPreflightProof, retErr error) {
	f.diagnosticStage("inventory", "inventory")
	defer func() {
		if retErr != nil {
			f.diagnosticFailure(retErr)
		}
	}()
	maps := f.coreClient.ConfigMaps(wmcoNamespace)
	pool, err := maps.Get(ctx, byohPoolConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read producer pool: %w", err)
	}
	proof := &byohPreflightProof{Pool: pool, Records: map[string]byohProducerRecord{}}
	for address, raw := range pool.Data {
		record, err := decodeBYOHProducerRecord(address, raw)
		if err != nil {
			return nil, err
		}
		if record.Fields["status"] != "available" {
			return nil, fmt.Errorf("producer host is unavailable or already claimed")
		}
		proof.Records[address] = record
	}
	if len(proof.Records) < 3 || len(proof.Records) > 24 || pool.UID == "" || pool.ResourceVersion == "" {
		return nil, fmt.Errorf("invalid three-host inventory snapshot")
	}
	instances, err := maps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read initial registrations: %w", err)
	}
	if instances.UID == "" || instances.ResourceVersion == "" {
		return nil, fmt.Errorf("initial registration identity is missing")
	}
	proof.Instances = instances
	nodes, err := f.coreClient.Nodes().List(ctx, metav1.ListOptions{LabelSelector: "kubernetes.io/os=windows"})
	if err != nil {
		return nil, err
	}
	if len(nodes.Items) != 3 {
		return nil, fmt.Errorf("dedicated BYOH run requires exactly three isolated Windows Nodes")
	}
	secret, err := f.coreClient.Secrets(wmcoNamespace).Get(ctx, byohPrivateKeySecret, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	proof.SecretUID = string(secret.UID)
	proof.KeyHash, err = publicKeyHash(secret.Data[byohPrivateKeyDataKey])
	if err != nil || proof.SecretUID == "" {
		return nil, fmt.Errorf("invalid original Secret identity")
	}
	resolver := f.byohResolver()
	assigned := map[string]int{}
	sort.Slice(nodes.Items, func(first, second int) bool { return nodes.Items[first].UID < nodes.Items[second].UID })
	for nodeIndex, node := range nodes.Items {
		if f.preparationDiagnostic != nil {
			f.preparationDiagnostic.Host = nodeIndex + 1
			f.preparationDiagnostic.Collection = nil
			f.preparationDiagnostic.Field = ""
		}
		f.diagnosticStage("identity", "identity")
		if !nodeReadyAndConverged(&node) || node.UID == "" || node.ResourceVersion == "" || node.Annotations["machine.openshift.io/machine"] != "" || len(node.OwnerReferences) != 0 {
			return nil, fmt.Errorf("initial Windows Node is not isolated converged BYOH")
		}
		if node.Annotations[byohPublicKeyHashAnno] != proof.KeyHash {
			return nil, fmt.Errorf("initial Node key hash mismatch")
		}
		username, err := decryptWMCOUsername(node.Annotations[byohUsernameAnno], secret.Data[byohPrivateKeyDataKey])
		if err != nil {
			return nil, fmt.Errorf("initial Node username proof failed")
		}
		matched := []string{}
		for address, record := range proof.Records {
			f.diagnosticStage("DNS", "dns")
			resolved, err := resolveBYOHAddress(ctx, address, resolver)
			if err != nil {
				return nil, err
			}
			for _, candidate := range node.Status.Addresses {
				if candidate.Type == corev1.NodeInternalIP && containsBYOHAddress(resolved, candidate.Address) {
					if record.Fields["username"] != username {
						return nil, fmt.Errorf("inventory/Node username mismatch")
					}
					if _, exists := assigned[address]; exists {
						return nil, fmt.Errorf("inventory alias matches multiple Nodes")
					}
					assigned[address] = len(proof.Hosts)
					matched = append(matched, address)
					break
				}
			}
		}
		if len(matched) == 0 {
			return nil, fmt.Errorf("initial Node has no authenticated inventory match")
		}
		sort.Strings(matched)
		lease := byohHostLease{NodeName: node.Name, NodeUID: node.UID, NodeResourceVersion: node.ResourceVersion, LeaseID: run, HostID: string(node.UID), Username: username, PreserveInstancesConfigMap: true}
		facts, err := f.probeBYOHHost(ctx, &lease)
		if err != nil {
			return nil, err
		}
		if _, err := decodeBYOHHostFacts(mustBYOHJSON(facts), true); err != nil {
			return nil, err
		}
		f.diagnosticStage("identity", "identity")
		for _, other := range proof.Hosts {
			if other.Lease.NodeUID == node.UID || (node.Spec.ProviderID != "" && other.InitialProviderID == node.Spec.ProviderID) || strings.EqualFold(other.Facts.UUID, facts.UUID) || strings.EqualFold(other.Facts.GUID, facts.GUID) {
				return nil, fmt.Errorf("duplicate physical host identity")
			}
			for _, address := range facts.Addresses {
				if containsBYOHAddress(other.Facts.Addresses, address) {
					return nil, fmt.Errorf("physical host NIC identities overlap")
				}
			}
		}
		for _, candidate := range node.Status.Addresses {
			if candidate.Type == corev1.NodeInternalIP && !containsBYOHAddress(facts.Addresses, candidate.Address) {
				return nil, fmt.Errorf("Node and actual NIC identities disagree")
			}
		}
		lease.HostID = strings.ToLower(facts.UUID + "/" + facts.GUID)
		lease.PhysicalUUID, lease.MachineGUID = facts.UUID, facts.GUID
		key, _ := parseTrustedHostKey(facts.PublicKey)
		for _, address := range matched {
			f.diagnosticStage("DNS", "dns")
			record := proof.Records[address]
			if !strings.EqualFold(record.Fields["platform"], proof.Records[matched[0]].Fields["platform"]) {
				return nil, fmt.Errorf("aliases disagree on supplied platform")
			}
			resolved, err := resolveBYOHAddress(ctx, address, resolver)
			if err != nil {
				return nil, err
			}
			for _, ip := range resolved {
				if !containsBYOHAddress(facts.Addresses, ip) {
					return nil, fmt.Errorf("inventory alias resolves outside the proved host")
				}
			}
			if value := record.Fields["host-id"]; value != "" && value != lease.HostID && !strings.EqualFold(value, facts.UUID) {
				return nil, fmt.Errorf("supplied host identity disagrees with probe")
			}
			if value := record.Fields["reset-policy"]; value != "" && value != byohResetPolicy {
				return nil, fmt.Errorf("supplied reset policy is incompatible")
			}
			if value := record.Fields["ssh-address"]; value != "" {
				resolved, err := resolveBYOHAddress(ctx, value, resolver)
				if err != nil {
					return nil, err
				}
				for _, ip := range resolved {
					if !containsBYOHAddress(facts.Addresses, ip) {
						return nil, fmt.Errorf("supplied SSH endpoint disagrees with host")
					}
				}
			}
			if value := record.Fields["ssh-host-public-key"]; value != "" {
				f.diagnosticStage("host-key", "publicKey")
				provided, err := parseTrustedHostKey(value)
				if err != nil || provided == nil || !bytes.Equal(provided.Marshal(), key.Marshal()) {
					return nil, fmt.Errorf("supplied host public key disagrees with authenticated probe")
				}
			}
		}
		lease.TrustedHostKey = key
		for _, address := range matched {
			if net.ParseIP(address) != nil {
				lease.Address, lease.SSHAddress = address, address
				break
			}
		}
		if lease.Address == "" {
			return nil, fmt.Errorf("host has no real inventory IP")
		}
		lease.AddressType = byohAddressIP
		lease.ResetPolicy = byohResetPolicy
		lease.Platform = proof.Records[lease.Address].Fields["platform"]
		lease.Disposable, _ = strconv.ParseBool(proof.Records[lease.Address].Fields["disposable"])
		for _, address := range matched {
			disposable, _ := strconv.ParseBool(proof.Records[address].Fields["disposable"])
			if disposable != lease.Disposable {
				return nil, fmt.Errorf("contradictory disposable host rights")
			}
		}
		f.diagnosticStage("SSH", "api")
		if f.preparationDiagnostic != nil {
			f.preparationDiagnostic.Transport = "SSH"
		}
		client, err := f.sshClient(ctx, &lease)
		if err != nil {
			return nil, fmt.Errorf("preflight direct pinned SSH: %w", err)
		}
		if err := client.close(); err != nil {
			return nil, fmt.Errorf("preflight SSH cleanup failed")
		}
		proof.Hosts = append(proof.Hosts, byohPreparedHost{Lease: lease, Facts: facts, Node: node, InitialProviderID: node.Spec.ProviderID})
		if f.preparationDiagnostic != nil {
			f.preparationDiagnostic.Collection = nil
		}
	}
	if len(assigned) != len(proof.Records) {
		return nil, fmt.Errorf("inventory contains unproven host aliases")
	}
	disposableCount := 0
	for index := range proof.Hosts {
		if f.preparationDiagnostic != nil {
			f.preparationDiagnostic.Host = index + 1
		}
		f.diagnosticStage("DNS", "dns")
		host := &proof.Hosts[index]
		if host.Lease.Disposable {
			disposableCount++
		}
		dnsCandidates := []string{host.Facts.DNS}
		for _, candidate := range host.Node.Status.Addresses {
			if candidate.Type == corev1.NodeInternalDNS || candidate.Type == corev1.NodeHostName {
				dnsCandidates = append(dnsCandidates, candidate.Address)
			}
		}
		for _, address := range dnsCandidates {
			if address == "" || net.ParseIP(address) != nil {
				continue
			}
			resolved, err := resolveBYOHAddress(ctx, address, resolver)
			if err != nil {
				continue
			}
			valid := true
			for _, ip := range resolved {
				valid = valid && containsBYOHAddress(host.Facts.Addresses, ip)
			}
			if valid {
				host.Lease.ClaimedAliases = append(host.Lease.ClaimedAliases, address)
				break
			}
		}
		if len(host.Lease.ClaimedAliases) == 0 {
			return nil, fmt.Errorf("host has no genuine same-host DNS registration address")
		}
		for address, owner := range assigned {
			if owner == index {
				host.Lease.ClaimedAliases = append(host.Lease.ClaimedAliases, address)
			}
		}
		sort.Strings(host.Lease.ClaimedAliases)
		host.Lease.ClaimedAliases = uniqueBYOHStrings(host.Lease.ClaimedAliases)
		if f.preparationDiagnostic != nil {
			f.preparationDiagnostic.Completed = index + 1
			f.preparationDiagnostic.checkpoint()
		}
	}
	if f.preparationDiagnostic != nil {
		f.preparationDiagnostic.Host = 0
	}
	f.diagnosticStage("inventory", "inventory")
	if disposableCount != 1 {
		return nil, fmt.Errorf("expected exactly one authentically designated disposable physical host")
	}
	for address, value := range instances.Data {
		if f.preparationDiagnostic != nil {
			f.preparationDiagnostic.Host = 0
			f.preparationDiagnostic.Field, f.preparationDiagnostic.Transport = "", ""
		}
		f.diagnosticStage("DNS", "dns")
		resolved, err := resolveBYOHAddress(ctx, address, resolver)
		if err != nil {
			return nil, err
		}
		owner := -1
		for index, host := range proof.Hosts {
			all := true
			for _, ip := range resolved {
				all = all && containsBYOHAddress(host.Facts.Addresses, ip)
			}
			if all {
				if owner != -1 {
					return nil, fmt.Errorf("registration alias has ambiguous physical ownership")
				}
				owner = index
			}
		}
		if owner < 0 {
			return nil, fmt.Errorf("unrelated or unproven registration prevents global-key isolation")
		}
		host := &proof.Hosts[owner]
		if f.preparationDiagnostic != nil {
			f.preparationDiagnostic.Host = owner + 1
		}
		if value != "username="+host.Lease.Username || instances.Annotations[registrationOwnershipAnnotation(address)] != "" {
			return nil, fmt.Errorf("initial registration username or foreign ownership mismatch")
		}
		host.RegistrationAliases = append(host.RegistrationAliases, address)
	}
	for index, host := range proof.Hosts {
		if len(host.RegistrationAliases) == 0 {
			if f.preparationDiagnostic != nil {
				f.preparationDiagnostic.Host = index + 1
			}
			return nil, fmt.Errorf("initial host is not registered")
		}
	}
	if f.preparationDiagnostic != nil {
		f.preparationDiagnostic.Host = 0
	}
	f.diagnosticStage("inventory", "inventory")
	if err := f.validateBYOHIsolation(ctx, proof.Hosts); err != nil {
		return nil, err
	}
	proof.Validated = true
	if f.preparationDiagnostic != nil {
		f.preparationDiagnostic.Phase = "validated"
		f.preparationDiagnostic.Completed = 3
		f.preparationDiagnostic.Host = 0
		f.preparationDiagnostic.checkpoint()
	}
	return proof, nil
}

func uniqueBYOHStrings(values []string) []string {
	result := []string{}
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func mustBYOHJSON(value interface{}) string { data, _ := json.Marshal(value); return string(data) }

func (f *byohTestFixture) validateBYOHIsolation(ctx context.Context, hosts []byohPreparedHost) error {
	if err := f.validateBYOHMachineIsolation(ctx); err != nil {
		return err
	}
	nodes, err := f.coreClient.Nodes().List(ctx, metav1.ListOptions{LabelSelector: "kubernetes.io/os=windows"})
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, host := range hosts {
		allowed[host.Lease.NodeName] = true
	}
	for _, node := range nodes.Items {
		if !allowed[node.Name] || node.Annotations["machine.openshift.io/machine"] != "" || len(node.OwnerReferences) != 0 {
			return fmt.Errorf("unrelated Windows Node prevents global-key isolation")
		}
	}
	pods, err := f.coreClient.Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if allowed[pod.Spec.NodeName] || pod.Spec.NodeSelector["kubernetes.io/os"] == "windows" || (pod.Spec.OS != nil && pod.Spec.OS.Name == corev1.Windows) {
			return fmt.Errorf("existing Windows workload prevents dedicated-host adoption")
		}
		if pod.Spec.NodeName != "" || (pod.Spec.OS != nil && pod.Spec.OS.Name == corev1.Linux) {
			continue
		}
		for index := range nodes.Items {
			matches, matchErr := nodeaffinity.GetRequiredNodeAffinity(&pod).Match(&nodes.Items[index])
			if matchErr != nil || matches {
				return fmt.Errorf("potential Windows host workload prevents dedicated-host adoption")
			}
		}
	}
	return nil
}

func (f *byohTestFixture) validateBYOHMachineIsolation(ctx context.Context) error {
	if f.dynamicClient == nil {
		return fmt.Errorf("Machine API isolation client is unavailable")
	}
	machines, err := f.dynamicClient.Resource(schema.GroupVersionResource{Group: "machine.openshift.io", Version: "v1beta1", Resource: "machines"}).Namespace("openshift-machine-api").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("read Windows Machine isolation: %w", err)
	}
	for _, machine := range machines.Items {
		osLabel, _, _ := unstructured.NestedString(machine.Object, "spec", "metadata", "labels", "kubernetes.io/os")
		userData, _, _ := unstructured.NestedString(machine.Object, "spec", "providerSpec", "value", "userDataSecret", "name")
		if strings.EqualFold(osLabel, "windows") || strings.EqualFold(machine.GetLabels()["machine.openshift.io/os-id"], "windows") || strings.Contains(strings.ToLower(userData), "windows") {
			return fmt.Errorf("Windows Machine prevents global-key isolation")
		}
	}
	return nil
}

func revalidateBYOHPreflight(ctx context.Context, f *byohTestFixture, proof *byohPreflightProof) error {
	secret, err := f.coreClient.Secrets(wmcoNamespace).Get(ctx, byohPrivateKeySecret, metav1.GetOptions{})
	if err != nil {
		return err
	}
	hash, err := publicKeyHash(secret.Data[byohPrivateKeyDataKey])
	if err != nil || string(secret.UID) != proof.SecretUID || hash != proof.KeyHash {
		return fmt.Errorf("original Secret changed before preparation")
	}
	pool, err := f.coreClient.ConfigMaps(wmcoNamespace).Get(ctx, byohPoolConfigMap, metav1.GetOptions{})
	if err != nil {
		return err
	}
	instances, err := f.coreClient.ConfigMaps(wmcoNamespace).Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if pool.UID != proof.Pool.UID || pool.ResourceVersion != proof.Pool.ResourceVersion || !reflect.DeepEqual(pool.Data, proof.Pool.Data) || instances.UID != proof.Instances.UID || instances.ResourceVersion != proof.Instances.ResourceVersion || !reflect.DeepEqual(instances.Data, proof.Instances.Data) || !reflect.DeepEqual(instances.Annotations, proof.Instances.Annotations) {
		return fmt.Errorf("preflight inventory or registrations changed before preparation")
	}
	for _, host := range proof.Hosts {
		node, err := f.coreClient.Nodes().Get(ctx, host.Node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if node.UID != host.Node.UID || node.ResourceVersion == "" || !nodeReadyAndConverged(node) || !reflect.DeepEqual(node.Labels, host.Node.Labels) || !reflect.DeepEqual(node.Status.Addresses, host.Node.Status.Addresses) || !reflect.DeepEqual(node.Annotations, host.Node.Annotations) || !reflect.DeepEqual(node.OwnerReferences, host.Node.OwnerReferences) || node.Spec.ProviderID != host.Node.Spec.ProviderID {
			return fmt.Errorf("initial Node changed before preparation")
		}
	}
	return f.validateBYOHIsolation(ctx, proof.Hosts)
}
