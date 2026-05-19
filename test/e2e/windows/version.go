package windows

type ServerVersion string

const (
	// Server2022 represent Windows Server 2022
	Server2022 ServerVersion = "2022"
	// Server2025 represent Windows Server 2025
	Server2025 ServerVersion = "2025"
)

// SupportedVersions are the Windows Server versions supported by the e2e test.
// "" implies the default which is Server2022
var SupportedVersions = []ServerVersion{Server2022, Server2025, ""}

// BuildNumber returns the build for a given server version as defined by Microsoft
var BuildNumber = map[ServerVersion]string{
	Server2022: "10.0.20348",
	Server2025: "10.0.26100",
}

// IsSupported checks if the given version is supported
func IsSupported(version ServerVersion) bool {
	for _, v := range SupportedVersions {
		if v == version {
			return true
		}
	}
	return false
}
