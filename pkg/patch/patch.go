package patch

// JSONPatch describes a patch operation
type JSONPatch struct {
	// op defines patch operation to be performed on the Endpoints object
	Op string `json:"op"`
	// path defines the location of the patch
	Path string `json:"path"`
	// value defines the data to be patched
	Value interface{} `json:"value,omitempty"`
}

// NewJSONPatch returns a pointer to a JSONPatch
func NewJSONPatch(op, path string, value interface{}) *JSONPatch {
	return &JSONPatch{
		Op:    op,
		Path:  path,
		Value: value,
	}
}
