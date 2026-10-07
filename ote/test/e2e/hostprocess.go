package winc

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"syscall"
	"time"

	g "github.com/onsi/ginkgo/v2"
	exutil "github.com/openshift/origin/test/extended/util"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

const (
	hostProcessLogMaxAttempts       = 5
	hostProcessLogOverallTimeout    = 90 * time.Second
	hostProcessLogPerRequestTimeout = 10 * time.Second
	hostProcessLogRetryDelay        = time.Second
	hostProcessCompletionTimeout    = 10 * time.Minute
	hostProcessPollInterval         = time.Second
	hostProcessPodGetTimeout        = 10 * time.Second
	hostProcessMaxReadErrors        = 5
	hostProcessDiagnosticTimeout    = 5 * time.Second
	hostProcessDiagnosticMaxEvents  = 8
	hostProcessSnapshotStatusLimit  = 8
	hostProcessSnapshotMaxBytes     = 768
	hostProcessMutationTimeout      = 30 * time.Second
)

type hostProcessLogGetter func(context.Context) ([]byte, error)

type hostProcessLogRetryConfig struct {
	maxAttempts       int
	overallTimeout    time.Duration
	perRequestTimeout time.Duration
	retryDelay        time.Duration
}

func defaultHostProcessLogRetryConfig() hostProcessLogRetryConfig {
	return hostProcessLogRetryConfig{
		maxAttempts:       hostProcessLogMaxAttempts,
		overallTimeout:    hostProcessLogOverallTimeout,
		perRequestTimeout: hostProcessLogPerRequestTimeout,
		retryDelay:        hostProcessLogRetryDelay,
	}
}

func isTransientHostProcessAPIError(err error) bool {
	if err == nil || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsNotFound(err) {
		return false
	}
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) {
		return true
	}
	errorText := strings.ToLower(err.Error())
	for _, marker := range []string{"forbidden", "unauthorized"} {
		if strings.Contains(errorText, marker) {
			return false
		}
	}
	if isMissingHostProcessPodError(errorText) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary()) {
		return true
	}

	for _, marker := range []string{
		"connection reset", "connection refused", "tls handshake timeout",
		"http2: client connection lost", "goaway", "unexpected eof",
	} {
		if strings.Contains(errorText, marker) {
			return true
		}
	}
	return false
}

func isMissingHostProcessPodError(errorText string) bool {
	if strings.Contains(errorText, "pod not found") {
		return true
	}
	for _, podResource := range []string{`pod "`, `pods "`} {
		if strings.Contains(errorText, podResource) && strings.Contains(errorText, `" not found`) {
			return true
		}
	}
	return false
}

func hostProcessLogDeadlineError(attempts int, deadlineErr, lastErr error) error {
	if lastErr == nil {
		return fmt.Errorf("HostProcess log retrieval deadline exceeded after %d attempt(s): %w", attempts, deadlineErr)
	}
	return fmt.Errorf("HostProcess log retrieval deadline exceeded after %d attempt(s): %w; last error: %w",
		attempts, deadlineErr, lastErr)
}

func retrieveHostProcessLogs(ctx context.Context, getLogs hostProcessLogGetter,
	config hostProcessLogRetryConfig) ([]byte, error) {
	retryCtx, cancel := context.WithTimeout(ctx, config.overallTimeout)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= config.maxAttempts; attempt++ {
		if err := retryCtx.Err(); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, hostProcessLogDeadlineError(attempt-1, err, lastErr)
		}

		requestCtx, requestCancel := context.WithTimeout(retryCtx, config.perRequestTimeout)
		output, err := getLogs(requestCtx)
		requestErr := requestCtx.Err()
		requestCancel()
		if contextErr := firstHostProcessContextError(ctx, retryCtx, requestErr); contextErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if retryCtx.Err() != nil {
				return nil, hostProcessLogDeadlineError(attempt, retryCtx.Err(), lastErr)
			}
			err = contextErr
		}
		if err == nil {
			return output, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !isTransientHostProcessAPIError(err) {
			return nil, fmt.Errorf("HostProcess log retrieval failed on attempt %d: %w", attempt, err)
		}
		if retryCtx.Err() != nil {
			return nil, hostProcessLogDeadlineError(attempt, retryCtx.Err(), err)
		}
		if attempt == config.maxAttempts {
			return nil, fmt.Errorf("HostProcess log retrieval exhausted after %d attempt(s); last error: %w",
				attempt, err)
		}

		timer := time.NewTimer(config.retryDelay)
		select {
		case <-retryCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, hostProcessLogDeadlineError(attempt, retryCtx.Err(), err)
		case <-timer.C:
		}
	}

	return nil, fmt.Errorf("HostProcess log retrieval exhausted after %d attempt(s); last error: %w",
		config.maxAttempts, lastErr)
}

func firstHostProcessContextError(callerCtx, ownerCtx context.Context, requestErr error) error {
	if callerCtx.Err() != nil {
		return callerCtx.Err()
	}
	if ownerCtx.Err() != nil {
		return ownerCtx.Err()
	}
	return requestErr
}

func hostProcessCommandResult(nodeName, podName string, terminalPhase corev1.PodPhase, logOutput []byte,
	logErr error) (string, error) {
	if logErr != nil {
		if terminalPhase == corev1.PodFailed {
			return "", fmt.Errorf("HostProcess command in pod %s on %s failed; log retrieval also failed: %w",
				podName, nodeName, logErr)
		}
		return "", fmt.Errorf("failed to get logs for HostProcess pod %s: %w", podName, logErr)
	}

	output := strings.TrimSpace(string(logOutput))
	if terminalPhase == corev1.PodFailed {
		// Command output is deliberately omitted from errors. It is free-form, potentially sensitive,
		// and can be arbitrarily large; the phase and pod identity retain the actionable failure cause.
		return "", fmt.Errorf("HostProcess command in pod %s on %s failed (phase=%s); command output omitted",
			podName, nodeName, terminalPhase)
	}
	return output, nil
}

type hostProcessContainerSnapshot struct {
	name       string
	state      string
	reason     string
	exitCode   int32
	startedAt  time.Time
	finishedAt time.Time
}

type hostProcessPodSnapshot struct {
	uid                       types.UID
	phase                     corev1.PodPhase
	createdAt                 time.Time
	observedAt                time.Time
	deletionTimestamp         *time.Time
	container                 *hostProcessContainerSnapshot
	containerStatusesOmitted  int
	containerStatusScanCapped bool
}

type hostProcessPodState struct {
	last                 *hostProcessPodSnapshot
	lastReadFailure      string
	diagnosticsAttempted bool
}

func newHostProcessPodSnapshot(pod *corev1.Pod, observedAt time.Time) hostProcessPodSnapshot {
	snapshot := hostProcessPodSnapshot{
		uid:        pod.UID,
		phase:      pod.Status.Phase,
		createdAt:  pod.CreationTimestamp.Time,
		observedAt: observedAt,
	}
	if pod.DeletionTimestamp != nil {
		deletionTimestamp := pod.DeletionTimestamp.Time
		snapshot.deletionTimestamp = &deletionTimestamp
	}
	statusLimit := len(pod.Status.ContainerStatuses)
	if statusLimit > hostProcessSnapshotStatusLimit {
		statusLimit = hostProcessSnapshotStatusLimit
	}
	for i := 0; i < statusLimit; i++ {
		status := pod.Status.ContainerStatuses[i]
		if status.Name != pod.Name {
			continue
		}
		container := hostProcessContainerSnapshot{name: safeKubernetesDiagnosticField(status.Name), state: "unknown"}
		switch {
		case status.State.Terminated != nil:
			container.state = "terminated"
			container.reason = safeKubernetesDiagnosticField(status.State.Terminated.Reason)
			container.exitCode = status.State.Terminated.ExitCode
			container.startedAt = status.State.Terminated.StartedAt.Time
			container.finishedAt = status.State.Terminated.FinishedAt.Time
		case status.State.Running != nil:
			container.state = "running"
			container.startedAt = status.State.Running.StartedAt.Time
		case status.State.Waiting != nil:
			container.state = "waiting"
			container.reason = safeKubernetesDiagnosticField(status.State.Waiting.Reason)
		}
		snapshot.container = &container
		break
	}
	snapshot.containerStatusesOmitted = len(pod.Status.ContainerStatuses)
	if snapshot.container != nil {
		snapshot.containerStatusesOmitted--
	}
	snapshot.containerStatusScanCapped = snapshot.container == nil && len(pod.Status.ContainerStatuses) > statusLimit
	return snapshot
}

func (s *hostProcessPodState) observe(pod *corev1.Pod, observedAt time.Time) []string {
	snapshot := newHostProcessPodSnapshot(pod, observedAt)
	event := "phase-change"
	changed := s.last == nil || s.last.phase != snapshot.phase
	if s.last == nil {
		event = "observed"
	}
	s.last = &snapshot
	s.lastReadFailure = ""
	if !changed {
		return nil
	}
	return []string{formatHostProcessPodSnapshot(event, snapshot)}
}

func (s *hostProcessPodState) observeReadFailure(err error) []string {
	failure := safeKubernetesAPIError(err)
	if s.lastReadFailure == failure {
		return nil
	}
	s.lastReadFailure = failure
	return []string{fmt.Sprintf("event=status-read-failed category=%s lifecycle-state-unchanged=true", failure)}
}

func (s *hostProcessPodState) requestDiagnostics() bool {
	if s.diagnosticsAttempted {
		return false
	}
	s.diagnosticsAttempted = true
	return true
}

func (s *hostProcessPodState) summary(observedAt time.Time, expectedUID types.UID) string {
	if s.last == nil {
		return boundHostProcessSnapshotOutput(fmt.Sprintf("state=never-observed observedAt=%s cause=unknown uid=%s",
			formatDiagnosticTime(observedAt), safeKubernetesDiagnosticField(string(expectedUID))))
	}
	return boundHostProcessSnapshotOutput(fmt.Sprintf("state=last-observed cause=unknown last={%s}",
		formatHostProcessPodSnapshotFields(*s.last)))
}

func formatHostProcessPodSnapshot(event string, snapshot hostProcessPodSnapshot) string {
	return boundHostProcessSnapshotOutput(fmt.Sprintf("event=%s %s", safeKubernetesDiagnosticField(event),
		formatHostProcessPodSnapshotFields(snapshot)))
}

func formatHostProcessPodSnapshotFields(snapshot hostProcessPodSnapshot) string {
	deletionTimestamp := "none"
	if snapshot.deletionTimestamp != nil {
		deletionTimestamp = formatDiagnosticTime(*snapshot.deletionTimestamp)
	}
	containerStatus := "missing"
	if snapshot.container != nil {
		container := snapshot.container
		fields := []string{
			fmt.Sprintf("name=%s", safeKubernetesDiagnosticField(container.name)),
			fmt.Sprintf("state=%s", safeKubernetesDiagnosticField(container.state)),
		}
		if container.reason != "" {
			fields = append(fields, fmt.Sprintf("reason=%s", safeKubernetesDiagnosticField(container.reason)))
		}
		if container.state == "terminated" {
			fields = append(fields, fmt.Sprintf("exitCode=%d", container.exitCode))
		}
		if !container.startedAt.IsZero() {
			fields = append(fields, fmt.Sprintf("startedAt=%s", formatDiagnosticTime(container.startedAt)))
		}
		if !container.finishedAt.IsZero() {
			fields = append(fields, fmt.Sprintf("finishedAt=%s", formatDiagnosticTime(container.finishedAt)))
		}
		containerStatus = "{" + strings.Join(fields, ",") + "}"
	}
	return boundHostProcessSnapshotOutput(fmt.Sprintf("uid=%s phase=%s containerStatus=%s containerStatusesOmitted=%d containerStatusScanCapped=%t createdAt=%s observedAt=%s deletionTimestamp=%s",
		safeKubernetesDiagnosticField(string(snapshot.uid)), safeKubernetesDiagnosticField(string(snapshot.phase)),
		containerStatus, snapshot.containerStatusesOmitted, snapshot.containerStatusScanCapped,
		formatDiagnosticTime(snapshot.createdAt), formatDiagnosticTime(snapshot.observedAt), deletionTimestamp))
}

func boundHostProcessSnapshotOutput(output string) string {
	if len(output) <= hostProcessSnapshotMaxBytes {
		return output
	}
	const marker = " outputTruncated=true"
	return output[:hostProcessSnapshotMaxBytes-len(marker)] + marker
}

func formatDiagnosticTime(timestamp time.Time) string {
	if timestamp.IsZero() {
		return "unknown"
	}
	return timestamp.UTC().Format(time.RFC3339Nano)
}

func safeKubernetesDiagnosticField(value string) string {
	if value == "" {
		return "none"
	}
	if len(value) > 63 {
		return "redacted"
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("._-/", char) {
			continue
		}
		return "redacted"
	}
	return value
}

func safeKubernetesAPIError(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err):
		return "timeout"
	case apierrors.IsForbidden(err):
		return "forbidden"
	case apierrors.IsUnauthorized(err):
		return "unauthorized"
	case apierrors.IsNotFound(err):
		return "not-found"
	case apierrors.IsTooManyRequests(err):
		return "rate-limited"
	case apierrors.IsServiceUnavailable(err):
		return "service-unavailable"
	default:
		return "api-error"
	}
}

type hostProcessEventLister func(context.Context, metav1.ListOptions) (*corev1.EventList, error)

func collectHostProcessEventDiagnostics(ctx context.Context, namespace string, uid types.UID,
	listEvents hostProcessEventLister) []string {
	events, err := listEvents(ctx, metav1.ListOptions{
		FieldSelector: fields.AndSelectors(
			fields.OneTermEqualSelector("involvedObject.namespace", namespace),
			fields.OneTermEqualSelector("involvedObject.uid", string(uid)),
		).String(),
		Limit: hostProcessDiagnosticMaxEvents + 1,
	})
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return []string{fmt.Sprintf("event-diagnostics unavailable: category=%s; cause remains unknown",
			safeKubernetesAPIError(err))}
	}
	if events == nil {
		return []string{"event-diagnostics unavailable: category=api-error; cause remains unknown"}
	}

	matching := make([]corev1.Event, 0, len(events.Items))
	for _, event := range events.Items {
		if event.InvolvedObject.UID == uid {
			matching = append(matching, event)
		}
	}
	if len(matching) == 0 {
		return []string{fmt.Sprintf("event-diagnostics namespace=%s uid=%s matched=0; cause remains unknown",
			namespace, uid)}
	}
	sort.SliceStable(matching, func(i, j int) bool {
		return eventDiagnosticTime(matching[i]).Before(eventDiagnosticTime(matching[j]))
	})
	start := 0
	if len(matching) > hostProcessDiagnosticMaxEvents {
		start = len(matching) - hostProcessDiagnosticMaxEvents
	}
	shown := matching[start:]
	lines := []string{fmt.Sprintf("event-diagnostics namespace=%s uid=%s received=%d shown=%d omittedFromPage=%d moreAvailable=%t; bounded page is not guaranteed to contain newest events; events are informational and do not identify a deletion actor",
		namespace, uid, len(matching), len(shown), len(matching)-len(shown), events.Continue != "")}
	for _, event := range shown {
		lines = append(lines, fmt.Sprintf("event-evidence observedAt=%s type=%s reason=%s count=%d source=%s",
			formatDiagnosticTime(eventDiagnosticTime(event)), safeKubernetesDiagnosticField(event.Type),
			safeKubernetesDiagnosticField(event.Reason), event.Count,
			safeKubernetesDiagnosticField(event.Source.Component)))
	}
	return lines
}

func eventDiagnosticTime(event corev1.Event) time.Time {
	switch {
	case !event.EventTime.IsZero():
		return event.EventTime.Time
	case !event.LastTimestamp.IsZero():
		return event.LastTimestamp.Time
	case !event.FirstTimestamp.IsZero():
		return event.FirstTimestamp.Time
	default:
		return event.CreationTimestamp.Time
	}
}

func hostProcessCompletionError(podName string, pollErr error) error {
	return fmt.Errorf("HostProcess pod %s did not complete: %w", podName, pollErr)
}

type hostProcessPodGetter func(context.Context) (*corev1.Pod, error)

type hostProcessCompletionConfig struct {
	overallTimeout           time.Duration
	pollInterval             time.Duration
	perRequestTimeout        time.Duration
	maxConsecutiveReadErrors int
}

func defaultHostProcessCompletionConfig() hostProcessCompletionConfig {
	return hostProcessCompletionConfig{
		overallTimeout:           hostProcessCompletionTimeout,
		pollInterval:             hostProcessPollInterval,
		perRequestTimeout:        hostProcessPodGetTimeout,
		maxConsecutiveReadErrors: hostProcessMaxReadErrors,
	}
}

func waitForHostProcessPodCompletion(ctx context.Context, podName string, expectedUID types.UID, getPod hostProcessPodGetter,
	state *hostProcessPodState, logLines func([]string), collectDiagnostics func(context.Context, types.UID),
	config hostProcessCompletionConfig) (corev1.PodPhase, error) {
	if expectedUID == "" {
		return corev1.PodUnknown, fmt.Errorf("HostProcess pod %s has no creation UID", podName)
	}
	if logLines == nil {
		logLines = func([]string) {}
	}
	if collectDiagnostics == nil {
		collectDiagnostics = func(context.Context, types.UID) {}
	}
	diagnose := func() {
		if !state.requestDiagnostics() {
			return
		}
		diagnosticCtx, diagnosticCancel := context.WithTimeout(ctx, hostProcessDiagnosticTimeout)
		defer diagnosticCancel()
		collectDiagnostics(diagnosticCtx, expectedUID)
	}

	var terminalPhase corev1.PodPhase
	consecutiveReadErrors := 0
	pollErr := wait.PollUntilContextTimeout(ctx, config.pollInterval, config.overallTimeout, true,
		func(pollCtx context.Context) (bool, error) {
			requestCtx, requestCancel := context.WithTimeout(pollCtx, config.perRequestTimeout)
			pod, err := getPod(requestCtx)
			requestErr := requestCtx.Err()
			requestCancel()
			if pollCtx.Err() != nil {
				err = pollCtx.Err()
			} else if requestErr != nil {
				err = requestErr
			}
			if err != nil {
				if apierrors.IsNotFound(err) {
					observedAt := time.Now()
					if state.last == nil {
						logLines([]string{boundHostProcessSnapshotOutput(fmt.Sprintf("event=not-found state=never-observed observedAt=%s cause=unknown uid=%s",
							formatDiagnosticTime(observedAt), safeKubernetesDiagnosticField(string(expectedUID))))})
						diagnose()
						return false, fmt.Errorf("HostProcess pod %s with uid %s disappeared before its first status observation: %w",
							podName, expectedUID, err)
					}
					logLines([]string{boundHostProcessSnapshotOutput(fmt.Sprintf("event=not-found state=disappeared observedAt=%s cause=unknown last={%s}",
						formatDiagnosticTime(observedAt), formatHostProcessPodSnapshotFields(*state.last)))})
					diagnose()
					return false, fmt.Errorf("HostProcess pod %s with uid %s disappeared: %w",
						podName, expectedUID, err)
				}
				if pollCtx.Err() != nil || ctx.Err() != nil {
					return false, err
				}
				if !isTransientHostProcessAPIError(err) {
					logLines(state.observeReadFailure(err))
					diagnose()
					return false, fmt.Errorf("HostProcess pod %s status read failed: %w", podName, err)
				}
				consecutiveReadErrors++
				logLines(state.observeReadFailure(err))
				if consecutiveReadErrors >= config.maxConsecutiveReadErrors {
					diagnose()
					return false, fmt.Errorf("HostProcess pod %s status read failed %d consecutive times: %w",
						podName, consecutiveReadErrors, err)
				}
				return false, nil
			}

			consecutiveReadErrors = 0
			if pod == nil || pod.UID == "" {
				return false, fmt.Errorf("HostProcess pod %s status response had no UID", podName)
			}
			if pod.UID != expectedUID {
				current := newHostProcessPodSnapshot(pod, time.Now())
				logLines([]string{boundHostProcessSnapshotOutput(fmt.Sprintf("event=replaced expectedUID=%s observed={%s}",
					safeKubernetesDiagnosticField(string(expectedUID)), formatHostProcessPodSnapshotFields(current)))})
				diagnose()
				return false, fmt.Errorf("HostProcess pod %s uid changed from %s to %s", podName, expectedUID, pod.UID)
			}

			logLines(state.observe(pod, time.Now()))
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				terminalPhase = pod.Status.Phase
				return true, nil
			}
			return false, nil
		})
	if pollErr != nil {
		logLines([]string{boundHostProcessSnapshotOutput(fmt.Sprintf("event=completion-wait-failed %s",
			state.summary(time.Now(), expectedUID)))})
		diagnose()
		return corev1.PodUnknown, pollErr
	}
	return terminalPhase, nil
}

type hostProcessPodCreator func(context.Context, *corev1.Pod) (*corev1.Pod, error)
type hostProcessPodDeleter func(context.Context, string, metav1.DeleteOptions) error

func newHostProcessPod(podName, nodeName, image, psCommand string) *corev1.Pod {
	hostProcess := true
	runAsUser := "NT AUTHORITY\\SYSTEM"
	windowsOptions := &corev1.WindowsSecurityContextOptions{
		HostProcess:   &hostProcess,
		RunAsUserName: &runAsUser,
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: wmcoNamespace,
			Labels:    map[string]string{"run": podName},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:      podName,
				Image:     image,
				Command:   []string{"powershell.exe", "-Command", psCommand},
				Resources: corev1.ResourceRequirements{},
				SecurityContext: &corev1.SecurityContext{
					WindowsOptions: windowsOptions.DeepCopy(),
				},
			}},
			RestartPolicy: corev1.RestartPolicyNever,
			DNSPolicy:     corev1.DNSClusterFirst,
			NodeSelector:  map[string]string{"kubernetes.io/hostname": nodeName},
			HostNetwork:   true,
			SecurityContext: &corev1.PodSecurityContext{
				WindowsOptions: windowsOptions,
			},
			Tolerations: []corev1.Toleration{{
				Key: "os", Operator: corev1.TolerationOpEqual, Value: "Windows", Effect: corev1.TaintEffectNoSchedule,
			}},
			OS: &corev1.PodOS{Name: corev1.Windows},
		},
	}
}

func createHostProcessPod(ctx context.Context, pod *corev1.Pod, create hostProcessPodCreator) (*corev1.Pod, error) {
	requestCtx, cancel := context.WithTimeout(ctx, hostProcessMutationTimeout)
	createdPod, err := create(requestCtx, pod)
	requestErr := requestCtx.Err()
	cancel()
	if ctx.Err() != nil {
		return createdPod, ctx.Err()
	}
	if requestErr != nil {
		return createdPod, requestErr
	}
	if err != nil {
		// Do not retry an ambiguous Create response: the server may have accepted this exact pod.
		return createdPod, fmt.Errorf("HostProcess pod creation failed: category=%s", safeKubernetesAPIError(err))
	}
	if createdPod == nil || createdPod.UID == "" {
		return nil, errors.New("HostProcess pod creation response had no UID")
	}
	return createdPod, nil
}

// cleanupHostProcessPod makes exactly one synchronous, independently bounded delete request. It intentionally
// uses a fresh context so a pod created just before caller cancellation can still be removed. The UID precondition
// prevents this limited post-cancellation allowance from deleting a same-name replacement.
func cleanupHostProcessPod(podName string, uid types.UID, deletePod hostProcessPodDeleter) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), hostProcessMutationTimeout)
	defer cancel()
	propagation := metav1.DeletePropagationBackground
	err := deletePod(cleanupCtx, podName, metav1.DeleteOptions{
		Preconditions:     &metav1.Preconditions{UID: &uid},
		PropagationPolicy: &propagation,
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func getHostProcessLogsForUID(ctx context.Context, podName string, expectedUID types.UID,
	getPod hostProcessPodGetter, getLogs hostProcessLogGetter) ([]byte, error) {
	pod, err := getPod(ctx)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if pod == nil || pod.UID != expectedUID {
		observedUID := types.UID("unavailable")
		if pod != nil && pod.UID != "" {
			observedUID = pod.UID
		}
		return nil, fmt.Errorf("HostProcess pod %s uid changed from %s to %s before log retrieval",
			podName, expectedUID, observedUID)
	}
	// Pod logs are exposed by a name-only endpoint. This immediately preceding UID check rejects known
	// replacements, but the Kubernetes API cannot make the GET and log request UID-atomic.
	output, err := getLogs(ctx)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return output, err
}

// runHostProcessPS runs a PowerShell command on a Windows node using one explicitly created HostProcess pod.
// The same pod is polled and its logs are retried; the command is never rerun and the pod is never recreated.
func runHostProcessPS(oc *exutil.CLI, nodeName, image, psCommand string, waitForCompletion ...bool) (string, error) {
	return runHostProcessPSWithContext(g.GinkgoT().Context(), oc, nodeName, image, psCommand, waitForCompletion...)
}

// runHostProcessPSWithContext is the context-aware form used by bounded callers. The supplied context owns pod
// creation, completion polling, diagnostics, and log retrieval; cleanup remains independently bounded.
func runHostProcessPSWithContext(ctx context.Context, oc *exutil.CLI, nodeName, image, psCommand string,
	waitForCompletion ...bool) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random suffix: %w", err)
	}
	nodeSafe := strings.ReplaceAll(nodeName, ".", "-")
	if len(nodeSafe) > 20 {
		nodeSafe = nodeSafe[:20]
	}
	podName := fmt.Sprintf("hpc-%s-%x", nodeSafe, b)
	pods := oc.AdminKubeClient().CoreV1().Pods(wmcoNamespace)

	e2e.Logf("[HostProcess] Creating pod %s on node %s", podName, nodeName)
	createdPod, err := createHostProcessPod(ctx, newHostProcessPod(podName, nodeName, image, psCommand),
		func(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
			return pods.Create(ctx, pod, metav1.CreateOptions{})
		})
	if createdPod != nil && createdPod.UID != "" {
		defer func() {
			e2e.Logf("[HostProcess] Pod %s cleanup attempt started", podName)
			if err := cleanupHostProcessPod(podName, createdPod.UID, pods.Delete); err != nil {
				e2e.Logf("[HostProcess] Pod %s cleanup request failed: category=%s", podName, safeKubernetesAPIError(err))
				return
			}
			e2e.Logf("[HostProcess] Pod %s cleanup request completed (pod may already have been absent)", podName)
		}()
	}
	if err != nil {
		e2e.Logf("[HostProcess] Pod %s creation failed: category=%s", podName, safeKubernetesAPIError(err))
		return "", fmt.Errorf("failed to create HostProcess pod %s on %s: %w", podName, nodeName, err)
	}
	e2e.Logf("[HostProcess] Pod %s creation request accepted: uid=%s", podName, createdPod.UID)

	if len(waitForCompletion) > 0 && !waitForCompletion[0] {
		e2e.Logf("[HostProcess] Pod %s created, returning without waiting", podName)
		return "", nil
	}

	events := oc.AdminKubeClient().CoreV1().Events(wmcoNamespace)
	state := &hostProcessPodState{}
	logLines := func(lines []string) {
		for _, line := range lines {
			e2e.Logf("[HostProcess] Pod %s lifecycle: %s", podName, line)
		}
	}
	collectDiagnostics := func(ctx context.Context, uid types.UID) {
		logLines(collectHostProcessEventDiagnostics(ctx, wmcoNamespace, uid, events.List))
	}
	getPod := func(ctx context.Context) (*corev1.Pod, error) {
		return pods.Get(ctx, podName, metav1.GetOptions{})
	}

	terminalPhase, pollErr := waitForHostProcessPodCompletion(ctx, podName, createdPod.UID, getPod,
		state, logLines, collectDiagnostics, defaultHostProcessCompletionConfig())
	if pollErr != nil {
		return "", hostProcessCompletionError(podName, pollErr)
	}
	e2e.Logf("[HostProcess] Pod %s final phase: %s", podName, terminalPhase)
	if terminalPhase == corev1.PodFailed && state.requestDiagnostics() {
		diagnosticCtx, cancel := context.WithTimeout(ctx, hostProcessDiagnosticTimeout)
		collectDiagnostics(diagnosticCtx, createdPod.UID)
		cancel()
	}

	logOutput, logErr := retrieveHostProcessLogs(ctx, func(ctx context.Context) ([]byte, error) {
		return getHostProcessLogsForUID(ctx, podName, createdPod.UID, getPod,
			func(ctx context.Context) ([]byte, error) {
				return pods.GetLogs(podName, &corev1.PodLogOptions{}).DoRaw(ctx)
			})
	}, defaultHostProcessLogRetryConfig())
	return hostProcessCommandResult(nodeName, podName, terminalPhase, logOutput, logErr)
}
