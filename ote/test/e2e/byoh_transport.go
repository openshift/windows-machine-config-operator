package winc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf16"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

const lifecycleConfiguredCommand = `$ProgressPreference='SilentlyContinue';
$names=@('kubelet','containerd','windows-instance-config-daemon');
foreach($n in $names){$svc=Get-Service -Name $n -ErrorAction SilentlyContinue;if($null -eq $svc -or $svc.Status -ne 'Running'){Write-Output 'BYOH_SERVICES_NOT_RUNNING';exit 1}};
foreach($p in @('C:\k\cni','C:\k\containerd')){if(!(Test-Path -LiteralPath $p -PathType Container)){Write-Output 'BYOH_PATH_MISSING';exit 1}};
Write-Output 'BYOH_OK'`

type hostProcessOutputWait struct {
	output    []byte
	attempts  int
	lastError error
}

func waitForHostProcessOutput(ctx context.Context, getLogs hostProcessLogGetter, markers ...string) (hostProcessOutputWait, error) {
	result := hostProcessOutputWait{}
	err := wait.PollUntilContextTimeout(ctx, time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		result.attempts++
		out, err := getLogs(ctx)
		if err != nil {
			result.lastError = err
			return false, err
		}
		result.output = out
		for _, marker := range markers {
			if !bytes.Contains(result.output, []byte(marker)) {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		statuses := make([]string, len(markers))
		for i, marker := range markers {
			statuses[i] = fmt.Sprintf("%s=%t", marker, bytes.Contains(result.output, []byte(marker)))
		}
		lastError := "none"
		if result.lastError != nil {
			lastError = safeKubernetesAPIError(result.lastError)
		}
		return result, fmt.Errorf(
			"HostProcess output incomplete after timeout: markers=[%s] attempts=%d bytes=%d lastError=%s: %w",
			strings.Join(statuses, ","), result.attempts, len(result.output), lastError, err)
	}
	return result, nil
}

func encodePowerShellScript(script string) string {
	encoded := utf16.Encode([]rune(script))
	data := make([]byte, len(encoded)*2)
	for i, value := range encoded {
		data[2*i], data[2*i+1] = byte(value), byte(value>>8)
	}
	return base64.StdEncoding.EncodeToString(data)
}

func hostProcessCommandFingerprint(command []string) string {
	digest := sha256.Sum256([]byte(strings.Join(command, "\x00")))
	return fmt.Sprintf("%x", digest)
}

func runHostProcessControl(ctx context.Context, pods corev1client.PodInterface, nodeName string, command []string, markers ...string) (retErr error) {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	pod := newHostProcessCommandPod("byoh-control-"+string(uuid.NewUUID()), nodeName, windowsDebugImage, command)
	pod.Spec.NodeName, pod.Spec.NodeSelector = nodeName, nil
	noToken := false
	pod.Spec.AutomountServiceAccountToken = &noToken
	created, err := createHostProcessPod(probeCtx, pod, func(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
		return pods.Create(ctx, pod, metav1.CreateOptions{})
	})
	if created != nil && created.UID != "" {
		defer func() {
			if err := cleanupHostProcessPod(pod.Name, created.UID, pods.Delete); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("HostProcess control cleanup failed: %w", err))
			}
		}()
	}
	if err != nil {
		return fmt.Errorf("HostProcess control pod creation failed: %w", err)
	}
	get := func(ctx context.Context) (*corev1.Pod, error) {
		current, err := pods.Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if current.UID != created.UID || current.Spec.NodeName != nodeName || !current.Spec.HostNetwork || len(current.Spec.Containers) != 1 || current.Spec.Containers[0].Image != windowsDebugImage || !reflect.DeepEqual(current.Spec.Containers[0].Command, command) {
			return nil, fmt.Errorf("HostProcess control admitted specification changed")
		}
		return current, nil
	}
	state := &hostProcessPodState{}
	phase, err := waitForHostProcessPodCompletion(probeCtx, pod.Name, created.UID, get, state, nil, nil, defaultHostProcessCompletionConfig())
	if err != nil {
		return fmt.Errorf("HostProcess control completion failed: pod=%s: %w", state.summary(time.Now(), created.UID), err)
	}
	if phase != corev1.PodSucceeded {
		current, getErr := get(probeCtx)
		if getErr != nil {
			return fmt.Errorf("HostProcess control failed with phase=%s; snapshot unavailable: %w", phase, getErr)
		}
		snapshot := newHostProcessPodSnapshot(current, time.Now())
		return fmt.Errorf("HostProcess control failed with phase=%s: pod={%s}", phase, formatHostProcessPodSnapshotFields(snapshot))
	}
	getLogs := func(ctx context.Context) ([]byte, error) {
		return getHostProcessLogsForUID(ctx, pod.Name, created.UID, get, func(ctx context.Context) ([]byte, error) {
			return pods.GetLogs(pod.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
		})
	}
	output, err := retrieveHostProcessLogs(probeCtx, getLogs, defaultHostProcessLogRetryConfig())
	if err != nil {
		return fmt.Errorf("HostProcess control log retrieval failed: %w", err)
	}
	missing := false
	for _, marker := range markers {
		if !bytes.Contains(output, []byte(marker)) {
			missing = true
			break
		}
	}
	if missing {
		waitResult, waitErr := waitForHostProcessOutput(probeCtx, getLogs, markers...)
		if waitErr != nil {
			current, getErr := get(probeCtx)
			if getErr != nil {
				return fmt.Errorf("HostProcess control failed; snapshot unavailable: %w", errors.Join(waitErr, getErr))
			}
			snapshot := newHostProcessPodSnapshot(current, time.Now())
			return fmt.Errorf(
				"HostProcess control failed: commandSHA256=%s commandBytes=%d pod={%s}: %w",
				hostProcessCommandFingerprint(command),
				len(strings.Join(command, "\x00")),
				formatHostProcessPodSnapshotFields(snapshot), waitErr)
		}
		output = waitResult.output
	}
	if len(output) > 4096 {
		return fmt.Errorf("HostProcess control output exceeds 4096 bytes")
	}
	return nil
}

func (f *suppliedBYOHLifecycle) checkHostProcess(ctx context.Context, command string) (string, error) {
	if f.node == nil {
		return "", fmt.Errorf("cannot run HostProcess command without a Node")
	}
	encoded := []string{"powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodePowerShellScript(command)}
	if err := runHostProcessControl(ctx, f.core.Pods(wmcoNamespace), f.node.Name, encoded, "BYOH_OK"); err != nil {
		return "", err
	}
	return "BYOH_OK", nil
}
