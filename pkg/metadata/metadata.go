package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	core "k8s.io/api/core/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/patch"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
)

const (
	// VersionAnnotation indicates the version of WMCO that configured the node
	VersionAnnotation = "windowsmachineconfig.openshift.io/version"
	// DesiredVersionAnnotation is a Node annotation, indicating the Service ConfigMap that should be used to configure it
	DesiredVersionAnnotation = "windowsmachineconfig.openshift.io/desired-version"
	// RebootAnnotation indicates the node's underlying instance needs to be restarted
	RebootAnnotation = "windowsmachineconfig.openshift.io/reboot-required"
)

// generatePatch creates a patch applying the given operation onto each given annotation key and value
func generatePatch(op string, labels, annotations map[string]string) ([]*patch.JSONPatch, error) {
	if len(labels) == 0 && len(annotations) == 0 {
		return nil, fmt.Errorf("labels and annotations empty")
	}
	var patches []*patch.JSONPatch
	if labels != nil {
		for key, value := range labels {
			patches = append(patches, patch.NewJSONPatch(op, path.Join("/metadata/labels/", escape(key)), value))
		}
	}
	if annotations != nil {
		for key, value := range annotations {
			patches = append(patches, patch.NewJSONPatch(op, path.Join("/metadata/annotations/", escape(key)), value))
		}
	}
	return patches, nil
}

// GenerateAddPatch creates a comma-separated list of operations to add all given labels and annotations from an object
// An "add" patch overwrites existing value if a label or annotation already exists
func GenerateAddPatch(labels, annotations map[string]string) ([]byte, error) {
	patch, err := generatePatch("add", labels, annotations)
	if err != nil {
		return []byte{}, err
	}
	return json.Marshal(patch)
}

// GenerateRemovePatch creates a comma-separated list of operations to remove all given labels and annotations from an
// object. A "remove" patch fails transactionally if any of the annotations do not exist.
func GenerateRemovePatch(labels, annotations []string) ([]byte, error) {
	labelMap := make(map[string]string)
	for _, label := range labels {
		labelMap[label] = ""
	}
	annotationMap := make(map[string]string)
	for _, annotation := range annotations {
		annotationMap[annotation] = ""
	}
	patch, err := generatePatch("remove", labelMap, annotationMap)
	if err != nil {
		return []byte{}, err
	}
	return json.Marshal(patch)
}

// escape replaces characters which would cause parsing issues with their escaped equivalent
func escape(key string) string {
	// The `/` in the metadata key needs to be escaped in order to not be considered a "directory" in the path
	return strings.Replace(key, "/", "~1", -1)
}

// ApplyLabelsAndAnnotations applies all the given annotations to the given Node resource
func ApplyLabelsAndAnnotations(ctx context.Context, c client.Client, node core.Node, labels, annotations map[string]string) error {
	patchData, err := GenerateAddPatch(labels, annotations)
	if err != nil {
		return fmt.Errorf("error creating annotations patch request: %w", err)
	}
	err = c.Patch(ctx, &node, client.RawPatch(kubeTypes.JSONPatchType, patchData))
	if err != nil {
		return fmt.Errorf("unable to apply patch data %s on node %s: %w", patchData, node.GetName(), err)
	}
	return nil
}

// ApplyVersionAnnotation applies this operator's version as the version annotation to the given Node
func ApplyVersionAnnotation(ctx context.Context, c client.Client, node core.Node, value string) error {
	return ApplyLabelsAndAnnotations(ctx, c, node, nil, map[string]string{VersionAnnotation: value})
}

// ApplyDesiredVersionAnnotation applies this operator's version as the desired version annotation to the given Node
func ApplyDesiredVersionAnnotation(ctx context.Context, c client.Client, node core.Node, value string) error {
	return ApplyLabelsAndAnnotations(ctx, c, node, nil, map[string]string{DesiredVersionAnnotation: value})
}

// ApplyRebootAnnotation applies an annotation to the given Node communicating that the instance needs to be restarted
func ApplyRebootAnnotation(ctx context.Context, c client.Client, node core.Node) error {
	return ApplyLabelsAndAnnotations(ctx, c, node, nil, map[string]string{RebootAnnotation: ""})
}

// RemoveVersionAnnotation clears the version annotation from the node object, indicating the node is not configured
func RemoveVersionAnnotation(ctx context.Context, c client.Client, node core.Node) error {
	if _, present := node.GetAnnotations()[VersionAnnotation]; present {
		patchData, err := GenerateRemovePatch([]string{}, []string{VersionAnnotation})
		if err != nil {
			return fmt.Errorf("error creating version annotation remove request: %w", err)
		}
		err = c.Patch(ctx, &node, client.RawPatch(kubeTypes.JSONPatchType, patchData))
		if err != nil {
			return fmt.Errorf("error removing version annotation from node %s: %w", node.GetName(), err)
		}
	}
	return nil
}

// RemoveVersionAnnotation clears the reboot annotation from the node, indicating the instance no longer needs a restart
func RemoveRebootAnnotation(ctx context.Context, c client.Client, node core.Node) error {
	if _, present := node.GetAnnotations()[RebootAnnotation]; present {
		patchData, err := GenerateRemovePatch([]string{}, []string{RebootAnnotation})
		if err != nil {
			return fmt.Errorf("error creating reboot annotation remove request: %w", err)
		}
		err = c.Patch(ctx, &node, client.RawPatch(kubeTypes.JSONPatchType, patchData))
		if err != nil {
			return fmt.Errorf("error removing reboot annotation from node %s: %w", node.GetName(), err)
		}
	}
	return nil
}

// WaitForVersionAnnotation checks if the node object has equivalent version and desiredVersion annotations.
// Waits for retry.Interval seconds and returns an error if the version annotation does not appear in that time frame.
func WaitForVersionAnnotation(ctx context.Context, c client.Client, nodeName string) error {
	node := &core.Node{}
	err := wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
		err := c.Get(ctx, kubeTypes.NamespacedName{Name: nodeName}, node)
		if err != nil {
			return false, err
		}
		desiredVer, ok := node.Annotations[DesiredVersionAnnotation]
		if !ok {
			return true, fmt.Errorf("node %s does not have %s annotation", nodeName, DesiredVersionAnnotation)
		}
		return node.Annotations[VersionAnnotation] == desiredVer, nil
	})
	if err != nil {
		return fmt.Errorf("timeout waiting for %s and %s annotations to match on node %s: %w", VersionAnnotation,
			DesiredVersionAnnotation, nodeName, err)
	}
	return nil
}

// WaitForRebootAnnotationRemoval waits for the reboot annotation to be cleared from the node
func WaitForRebootAnnotationRemoval(ctx context.Context, c client.Client, nodeName string) error {
	node := &core.Node{}
	err := wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
		err := c.Get(ctx, kubeTypes.NamespacedName{Name: nodeName}, node)
		if err != nil {
			return false, nil
		}
		_, present := node.Annotations[RebootAnnotation]
		return !present, nil
	})
	if err != nil {
		return fmt.Errorf("timeout waiting for %s to be cleared: %w", RebootAnnotation, err)
	}
	return nil
}
