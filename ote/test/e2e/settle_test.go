package winc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMachineSetSnapshotSettledAfter(t *testing.T) {
	const settle = 2 * time.Minute
	converged := machineSetSnapshot{
		Replicas: 2,
		Machines: []string{"e2e-wm-aaaaa", "e2e-wm-bbbbb"},
		Ready:    2,
	}

	tests := []struct {
		name         string
		previous     machineSetSnapshot
		current      machineSetSnapshot
		unchangedFor time.Duration
		want         bool
	}{
		{
			name:         "converged and unchanged for the settle window",
			previous:     converged,
			current:      converged,
			unchangedFor: settle,
			want:         true,
		},
		{
			name:         "converged but the settle window has not elapsed",
			previous:     converged,
			current:      converged,
			unchangedFor: settle - time.Second,
			want:         false,
		},
		{
			name:         "a machine is being deleted",
			previous:     converged,
			current:      machineSetSnapshot{Replicas: 2, Machines: []string{"e2e-wm-aaaaa", "e2e-wm-bbbbb"}, Deleting: 1, Ready: 2},
			unchangedFor: settle,
			want:         false,
		},
		{
			name:     "the MachineSet is still over replicated",
			previous: converged,
			current: machineSetSnapshot{
				Replicas: 2,
				Machines: []string{"e2e-wm-aaaaa", "e2e-wm-bbbbb", "e2e-wm-ccccc"},
				Ready:    3,
			},
			unchangedFor: settle,
			want:         false,
		},
		{
			name:     "the MachineSet is under replicated",
			previous: converged,
			current: machineSetSnapshot{
				Replicas: 2,
				Machines: []string{"e2e-wm-aaaaa"},
				Ready:    1,
			},
			unchangedFor: settle,
			want:         false,
		},
		{
			name:     "a machine has no ready node yet",
			previous: converged,
			current: machineSetSnapshot{
				Replicas: 2,
				Machines: []string{"e2e-wm-aaaaa", "e2e-wm-bbbbb"},
				Ready:    1,
			},
			unchangedFor: settle,
			want:         false,
		},
		{
			name:         "the machine list changed since the previous observation",
			previous:     converged,
			current:      machineSetSnapshot{Replicas: 2, Machines: []string{"e2e-wm-bbbbb", "e2e-wm-ccccc"}, Ready: 2},
			unchangedFor: settle,
			want:         false,
		},
		{
			name:         "zero replicas with no machines is settled",
			previous:     machineSetSnapshot{Replicas: 0},
			current:      machineSetSnapshot{Replicas: 0},
			unchangedFor: settle,
			want:         true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.current.settledAfter(tc.previous, tc.unchangedFor, settle))
		})
	}
}

func TestFirstExistingNode(t *testing.T) {
	existing := map[string]bool{
		"e2e-wm-aaaaa": false, // stale reference: the Machine still points at a removed Node
		"e2e-wm-bbbbb": true,
		"e2e-wm-ccccc": true,
	}
	exists := func(name string) bool { return existing[name] }

	tests := []struct {
		name       string
		candidates []string
		wantNode   string
		wantOK     bool
	}{
		{
			name:       "first candidate exists",
			candidates: []string{"e2e-wm-bbbbb", "e2e-wm-ccccc"},
			wantNode:   "e2e-wm-bbbbb",
			wantOK:     true,
		},
		{
			name:       "stale reference is skipped for a registered replacement",
			candidates: []string{"e2e-wm-aaaaa", "e2e-wm-bbbbb"},
			wantNode:   "e2e-wm-bbbbb",
			wantOK:     true,
		},
		{
			name:       "only a stale reference",
			candidates: []string{"e2e-wm-aaaaa"},
			wantNode:   "",
			wantOK:     false,
		},
		{
			name:       "empty names are ignored",
			candidates: []string{"", "e2e-wm-ccccc"},
			wantNode:   "e2e-wm-ccccc",
			wantOK:     true,
		},
		{
			name:       "no candidates",
			candidates: nil,
			wantNode:   "",
			wantOK:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node, ok := firstExistingNode(tc.candidates, exists)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantNode, node)
		})
	}
}

func TestAutoscalerWorkloadReplicas(t *testing.T) {
	const podCPU = int64(2000) // 2 CPU per pod

	tests := []struct {
		name    string
		nodeCPU []int64
		podCPU  int64
		want    int
	}{
		{
			name:    "vsphere three 3.5 CPU nodes fit one pod each plus one pending",
			nodeCPU: []int64{3500, 3500, 3500},
			podCPU:  podCPU,
			want:    4,
		},
		{
			name:    "azure two 8 vCPU nodes fit three pods each plus one pending",
			nodeCPU: []int64{7500, 7500},
			podCPU:  podCPU,
			want:    7,
		},
		{
			name:    "one small node keeps the node-count floor",
			nodeCPU: []int64{3500},
			podCPU:  podCPU,
			want:    2,
		},
		{
			name:    "capacity below one pod falls back to the node count",
			nodeCPU: []int64{1000, 1000},
			podCPU:  podCPU,
			want:    3,
		},
		{
			name:    "unknown pod request keeps the node-count floor",
			nodeCPU: []int64{7500, 7500},
			podCPU:  0,
			want:    3,
		},
		{
			name:    "no nodes still yields one replica",
			nodeCPU: nil,
			podCPU:  podCPU,
			want:    1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, autoscalerWorkloadReplicas(tc.nodeCPU, tc.podCPU))
		})
	}
}

func TestAWSVersionForOtherBuild(t *testing.T) {
	tests := []struct {
		build       string
		wantVersion string
		wantOK      bool
	}{
		{build: "10.0.20348", wantVersion: "2025", wantOK: true},
		{build: "10.0.26100", wantVersion: "2022", wantOK: true},
		{build: " 10.0.20348 ", wantVersion: "2025", wantOK: true},
		{build: "10.0.17763", wantOK: false},
		{build: "", wantOK: false},
	}
	for _, tc := range tests {
		version, ok := awsVersionForOtherBuild(tc.build)
		assert.Equal(t, tc.wantOK, ok, tc.build)
		assert.Equal(t, tc.wantVersion, version, tc.build)
	}
}

func TestAWSDeriveSecondaryImageFilter(t *testing.T) {
	tests := []struct {
		name       string
		primary    string
		target     string
		wantFilter string
		wantOK     bool
	}{
		{
			name:       "core base 2022 primary to 2025",
			primary:    "Windows_Server-2022-English-Core-Base-2025.01.15",
			target:     "2025",
			wantFilter: "Windows_Server-2025-English-Core-Base-*",
			wantOK:     true,
		},
		{
			name:       "full base 2025 primary to 2022 keeps the variant",
			primary:    "Windows_Server-2025-English-Full-Base-2025.02.10",
			target:     "2022",
			wantFilter: "Windows_Server-2022-English-Full-Base-*",
			wantOK:     true,
		},
		{
			name:    "same version is not a secondary",
			primary: "Windows_Server-2025-English-Core-Base-2025.02.10",
			target:  "2025",
			wantOK:  false,
		},
		{
			name:    "non windows name",
			primary: "amzn2-ami-hvm-2.0",
			target:  "2025",
			wantOK:  false,
		},
		{
			name:    "empty target",
			primary: "Windows_Server-2022-English-Core-Base-2025.01.15",
			target:  "",
			wantOK:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			filter, ok := awsDeriveSecondaryImageFilter(tc.primary, tc.target)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantFilter, filter)
		})
	}
}
