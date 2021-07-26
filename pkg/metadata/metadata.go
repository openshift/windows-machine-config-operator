package metadata

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openshift/windows-machine-config-operator/pkg/patch"
)

// VersionAnnotation indicates the version of WMCO that configured the node
const VersionAnnotation = "windowsmachineconfig.openshift.io/version"

// generateAnnotationPatch creates a patch applying the given operation onto each given annotation key and value
func generateAnnotationPatch(op string, annotations map[string]string) ([]*patch.JSONPatch, error) {
	if len(annotations) == 0 {
		return nil, errors.New("annotations to format cannot be empty or nil")
	}
	var patches []*patch.JSONPatch
	for key, value := range annotations {
		patches = append(patches, patch.NewJSONPatch(op, formatPathValue(key), value))
	}
	return patches, nil
}

// GenerateAddPatch creates a comma-separated list of operations to add all given annotations from an object
// An "add" patch overwrites existing value if an annotation already exists
func GenerateAddPatch(annotations map[string]string) ([]byte, error) {
	patch, err := generateAnnotationPatch("add", annotations)
	if err != nil {
		return []byte{}, err
	}
	return json.Marshal(patch)
}

// GenerateRemovePatch creates a comma-separated list of operations to remove all given annotations from an object
// A "remove" patch fails transactionally if any of the annotations do not exist
func GenerateRemovePatch(annotations []string) ([]byte, error) {
	annotationMap := make(map[string]string)
	for _, annotation := range annotations {
		annotationMap[annotation] = ""
	}
	patch, err := generateAnnotationPatch("remove", annotationMap)
	if err != nil {
		return []byte{}, err
	}
	return json.Marshal(patch)
}

// formatPathValue formats the path value specifying which attribute a JSON Patch operation should be applied to
func formatPathValue(key string) string {
	// The `/` in the annotation key needs to be escaped in order to not be considered a "directory" in the path
	escapedKey := strings.Replace(key, "/", "~1", -1)
	return fmt.Sprintf("/metadata/annotations/%s", escapedKey)
}
