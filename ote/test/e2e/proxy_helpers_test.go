package winc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestProxyPropagationPollBounds(t *testing.T) {
	t.Parallel()

	config := defaultProxyPropagationPollConfig()
	assert.Equal(t, 20*time.Second, config.interval)
	assert.Equal(t, 5*time.Minute, config.timeout)
}

func TestWaitForProxyOnNodesPropagatesOwningContextThroughHostProcessLifecycle(t *testing.T) {
	t.Parallel()

	config := proxyPropagationPollConfig{interval: time.Millisecond, timeout: 200 * time.Millisecond}
	var ownerDeadline time.Time
	createCalls := 0
	phaseCalls := 0
	readCalls := 0

	err := waitForProxyOnNodesWithContext(context.Background(), nil, []string{"node-1"},
		map[string]interface{}{"HTTP_PROXY": "http://proxy.example"},
		func(ctx context.Context, _ *exutil.CLI, nodeName, image, command string, _ ...bool) (string, error) {
			var ok bool
			ownerDeadline, ok = ctx.Deadline()
			require.True(t, ok)
			assert.Less(t, time.Until(ownerDeadline), defaultHostProcessCompletionConfig().overallTimeout,
				"the proxy poll deadline must be shorter than the HostProcess ten-minute completion bound")
			assert.Equal(t, "node-1", nodeName)
			assert.Equal(t, windowsDebugImage, image)
			assert.Contains(t, command, "HTTP_PROXY")

			createdPod, createErr := createHostProcessPod(ctx, &corev1.Pod{},
				func(requestCtx context.Context, _ *corev1.Pod) (*corev1.Pod, error) {
					createCalls++
					deadline, hasDeadline := requestCtx.Deadline()
					assert.True(t, hasDeadline)
					assert.True(t, deadline.Equal(ownerDeadline))
					return testHostProcessPod("created-uid", corev1.PodPending, time.Now()), nil
				})
			require.NoError(t, createErr)

			phase, phaseErr := waitForHostProcessPodCompletion(ctx, "hostprocess-pod", createdPod.UID,
				func(requestCtx context.Context) (*corev1.Pod, error) {
					phaseCalls++
					deadline, hasDeadline := requestCtx.Deadline()
					assert.True(t, hasDeadline)
					assert.True(t, deadline.Equal(ownerDeadline))
					return testHostProcessPod(createdPod.UID, corev1.PodSucceeded, time.Now()), nil
				}, &hostProcessPodState{}, nil, nil, defaultHostProcessCompletionConfig())
			require.NoError(t, phaseErr)
			assert.Equal(t, corev1.PodSucceeded, phase)

			output, readErr := retrieveHostProcessLogs(ctx, func(requestCtx context.Context) ([]byte, error) {
				readCalls++
				deadline, hasDeadline := requestCtx.Deadline()
				assert.True(t, hasDeadline)
				assert.True(t, deadline.Equal(ownerDeadline))
				return []byte("http://proxy.example"), nil
			}, defaultHostProcessLogRetryConfig())
			require.NoError(t, readErr)
			return string(output), nil
		}, config)

	require.NoError(t, err)
	assert.False(t, ownerDeadline.IsZero())
	assert.Equal(t, 1, createCalls)
	assert.Equal(t, 1, phaseCalls)
	assert.Equal(t, 1, readCalls)
}

func TestWaitForProxyOnNodesRepeatsReadOnlyChecksUntilPropagationSucceeds(t *testing.T) {
	t.Parallel()

	calls := 0
	err := waitForProxyOnNodesWithContext(context.Background(), nil, []string{"node-1"},
		map[string]interface{}{"HTTP_PROXY": "http://proxy.example"},
		func(context.Context, *exutil.CLI, string, string, string, ...bool) (string, error) {
			calls++
			if calls == 1 {
				return "http://old-proxy.example", nil
			}
			return "http://proxy.example", nil
		}, proxyPropagationPollConfig{interval: time.Millisecond, timeout: time.Second})

	require.NoError(t, err)
	assert.Equal(t, 2, calls)
}

func TestWaitForProxyOnNodesPreCanceledContextStartsNoHostProcessWork(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := waitForProxyOnNodesWithContext(ctx, nil, []string{"node-1"},
		map[string]interface{}{"HTTP_PROXY": "http://proxy.example"},
		func(context.Context, *exutil.CLI, string, string, string, ...bool) (string, error) {
			calls++
			return "", nil
		}, proxyPropagationPollConfig{interval: time.Millisecond, timeout: time.Second})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, calls)
}

func TestWaitForProxyOnNodesCancellationStopsCurrentPollAndPreservesCause(t *testing.T) {
	t.Parallel()

	cancellationCause := errors.New("proxy verification canceled")
	ctx, cancel := context.WithCancelCause(context.Background())
	var nodes []string
	err := waitForProxyOnNodesWithContext(ctx, nil, []string{"node-1", "node-2"},
		map[string]interface{}{"HTTP_PROXY": "http://proxy.example"},
		func(callCtx context.Context, _ *exutil.CLI, nodeName, _, _ string, _ ...bool) (string, error) {
			nodes = append(nodes, nodeName)
			cancel(cancellationCause)
			<-callCtx.Done()
			return "http://proxy.example", nil
		}, proxyPropagationPollConfig{interval: time.Millisecond, timeout: time.Second})

	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, cancellationCause)
	assert.Equal(t, []string{"node-1"}, nodes)
}

func TestWaitForProxyOnNodesDeadlineStopsNestedWorkAndNextCondition(t *testing.T) {
	t.Parallel()

	var nodes []string
	err := waitForProxyOnNodesWithContext(context.Background(), nil, []string{"node-1", "node-2"},
		map[string]interface{}{"HTTP_PROXY": "http://proxy.example"},
		func(callCtx context.Context, _ *exutil.CLI, nodeName, _, _ string, _ ...bool) (string, error) {
			nodes = append(nodes, nodeName)
			<-callCtx.Done()
			return "", callCtx.Err()
		}, proxyPropagationPollConfig{interval: time.Millisecond, timeout: 20 * time.Millisecond})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, []string{"node-1"}, nodes)
}

func TestWaitForProxyOnNodesDoesNotSwallowHostProcessContextError(t *testing.T) {
	t.Parallel()

	calls := 0
	err := waitForProxyOnNodesWithContext(context.Background(), nil, []string{"node-1", "node-2"},
		map[string]interface{}{"HTTP_PROXY": "http://proxy.example"},
		func(context.Context, *exutil.CLI, string, string, string, ...bool) (string, error) {
			calls++
			return "", fmt.Errorf("HostProcess stopped: %w", context.Canceled)
		}, proxyPropagationPollConfig{interval: time.Millisecond, timeout: time.Second})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls)
}
