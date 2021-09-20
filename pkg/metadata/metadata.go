package metadata

import (
	"context"
	"encoding/json"
	"path"
	"strings"

	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/patch"
)

// VersionAnnotation indicates the version of WMCO that configured the node
const VersionAnnotation = "windowsmachineconfig.openshift.io/version"

// generatePatch creates a patch applying the given operation onto each given annotation key and value
func generatePatch(op string, labels, annotations map[string]string) ([]*patch.JSONPatch, error) {
	if len(labels) == 0 && len(annotations) == 0 {
		return nil, errors.New("labels and annotations empty")
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

// ApplyAnnotations applies all the given annotations to the given Node resource
func ApplyAnnotations(c client.Client, ctx context.Context, node core.Node, annotations map[string]string) error {
	patchData, err := GenerateAddPatch(nil, annotations)
	if err != nil {
		return errors.Wrapf(err, "error creating annotations patch request")
	}
	err = c.Patch(ctx, &node, client.RawPatch(kubeTypes.JSONPatchType, patchData))
	if err != nil {
		return errors.Wrapf(err, "unable to apply patch data %s on node %s", patchData, node.GetName())
	}
	return nil
}
