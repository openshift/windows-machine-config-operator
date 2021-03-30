package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	nc "github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
)

// testNodeMetadata tests if all nodes have a worker label and kubelet version and are annotated with the version of
// the currently deployed WMCO
func testNodeMetadata(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)
	operatorVersion, err := getWMCOVersion()
	require.NoError(t, err, "could not get WMCO version")

	clusterKubeletVersion, err := tc.getClusterKubeVersion()
	require.NoError(t, err, "could not get cluster kube version")

	pubKey, err := tc.getExpectedPublicKey()
	require.NoError(t, err, "error getting the expected public key")
	pubKeyAnnotation := nc.CreatePubKeyHashAnnotation(pubKey)

	for _, node := range gc.nodes {
		t.Run(node.GetName()+" Validation Tests", func(t *testing.T) {
			t.Run("Kubelet Version", func(t *testing.T) {
				t.Skip("SKIP UNTIL CLUSTER UPGRADES TO 1.21")
				isValidVersion := strings.HasPrefix(node.Status.NodeInfo.KubeletVersion, clusterKubeletVersion)
				assert.True(t, isValidVersion,
					"expected kubelet version %s was not present on %s. Found %s", node.GetName(),
					clusterKubeletVersion, node.Status.NodeInfo.KubeletVersion)
			})
			// The worker label is not actually added by WMCO however we would like to validate if the Machine Api is
			// properly adding the worker label, if it was specified in the MachineSet. The MachineSet created in the
			// test suite has the worker label
			t.Run("Worker Label", func(t *testing.T) {
				assert.Contains(t, node.Labels, nc.WorkerLabel, "expected node label %s was not present on %s",
					nc.WorkerLabel, node.GetName())
			})
			t.Run("Version Annotation", func(t *testing.T) {
				require.Containsf(t, node.Annotations, nc.VersionAnnotation, "node %s missing version annotation",
					node.GetName())
				assert.Equal(t, operatorVersion, node.Annotations[nc.VersionAnnotation],
					"WMCO version annotation mismatch")
			})
			t.Run("Public Key Annotation", func(t *testing.T) {
				require.Containsf(t, node.Annotations, nc.PubKeyHashAnnotation, "node %s missing public key annotation",
					node.GetName())
				assert.Equal(t, pubKeyAnnotation, node.Annotations[nc.PubKeyHashAnnotation],
					"public key annotation mismatch")
			})
		})
	}
	t.Run("Windows node metadata not applied to Linux nodes", func(t *testing.T) {
		nodes, err := tc.client.K8s.CoreV1().Nodes().List(context.TODO(), meta.ListOptions{
			LabelSelector: core.LabelOSStable + "=linux"})
		require.NoError(t, err, "error listing Linux nodes")
		for _, node := range nodes.Items {
			assert.NotContainsf(t, node.Annotations, nc.VersionAnnotation,
				"version annotation applied to Linux node %s", node.GetName())
			assert.NotContainsf(t, node.Annotations, nc.PubKeyHashAnnotation,
				"public key annotation applied to Linux node %s", node.GetName())
		}
	})
}

// getInstanceID gets the instanceID of VM for a given cloud provider ID
// Ex: aws:///us-east-1e/i-078285fdadccb2eaa. We always want the last entry which is the instanceID
func getInstanceID(providerID string) string {
	providerTokens := strings.Split(providerID, "/")
	return providerTokens[len(providerTokens)-1]
}

// getInstanceIDsOfNodes returns the instanceIDs of all the Windows nodes created
func (tc *testContext) getInstanceIDsOfNodes() ([]string, error) {
	instanceIDs := make([]string, 0, len(gc.nodes))
	for _, node := range gc.nodes {
		if len(node.Spec.ProviderID) > 0 {
			instanceID := getInstanceID(node.Spec.ProviderID)
			instanceIDs = append(instanceIDs, instanceID)
		}
	}
	return instanceIDs, nil
}

// getClusterKubeVersion returns the major and minor Kubernetes version of the cluster
func (tc *testContext) getClusterKubeVersion() (string, error) {
	serverVersion, err := tc.client.K8s.Discovery().ServerVersion()
	if err != nil {
		return "", errors.Wrapf(err, "error getting cluster kube version")
	}
	versionSplit := strings.Split(serverVersion.GitVersion, ".")
	if versionSplit == nil {
		return "", fmt.Errorf("unexpected cluster kube version output")
	}
	return strings.Join(versionSplit[0:2], "."), nil
}

// getWMCOVersion returns the version of the operator. This is sourced from the WMCO binary used to create the operator image.
// This function will return an error if the binary is missing.
func getWMCOVersion() (string, error) {
	cmd := exec.Command(wmcoPath, "version")
	out, err := cmd.Output()
	if err != nil {
		return "", errors.Wrapf(err, "error running %s", cmd.String())
	}
	// out is formatted like:
	// ./build/_output/bin/windows-machine-config-operator version: "0.0.1+4165dda-dirty", go version: "go1.13.7 linux/amd64"
	versionSplit := strings.Split(string(out), "\"")
	if len(versionSplit) < 3 {
		return "", fmt.Errorf("unexpected version output")
	}
	return versionSplit[1], nil
}

// testNodeTaint tests if the Windows node has the Windows taint
func testNodeTaint(t *testing.T) {
	// windowsTaint is the taint that needs to be applied to the Windows node
	windowsTaint := core.Taint{
		Key:    "os",
		Value:  "Windows",
		Effect: core.TaintEffectNoSchedule,
	}

	for _, node := range gc.nodes {
		hasTaint := func() bool {
			for _, taint := range node.Spec.Taints {
				if taint.Key == windowsTaint.Key && taint.Value == windowsTaint.Value && taint.Effect == windowsTaint.Effect {
					return true
				}
			}
			return false
		}()
		assert.Equal(t, hasTaint, true, "expected Windows Taint to be present on the Node: %s", node.GetName())
	}
}
