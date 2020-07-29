package version

import (
	"fmt"
	"runtime"

	sdkVersion "github.com/operator-framework/operator-sdk/version"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var log = logf.Log.WithName("version")

var (
	Version   = "" // version will be replaced while building the binary using ldflags
	GoVersion = fmt.Sprintf("%s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
)

// Print() logs the operator version and related information
func Print() {
	log.Info("operator", "version", Version)
	log.Info("go", "version", GoVersion)
	log.Info("operator-sdk", "version", sdkVersion.Version)
}

// Get() returns the operator version
func Get() string {
	return Version
}
