package metrics

const (
	// PortName specifies the portname used for Prometheus monitoring
	PortName = "metrics"
	// Port is the port number on which windows-exporter is exposed.
	Port int32 = 9182
	// WindowsMetricsResource is the name for objects created for Prometheus monitoring
	// by current operator version. Its name is defined through the bundle manifests
	WindowsMetricsResource = "windows-exporter"
)
