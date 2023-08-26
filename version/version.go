package version

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
	ctrl "sigs.k8s.io/controller-runtime"
)

var log = ctrl.Log.WithName("version")

var (
	Version   = "" // version will be replaced while building the binary using ldflags
	GoVersion = fmt.Sprintf("%s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
)

// Print() logs the operator version and related information
func Print() {
	log.Info("operator", "version", Version)
	log.Info("go", "version", GoVersion)
}

// Get() returns the operator version
func Get() string {
	return Version
}

// Major returns only the Major portion of the operator version semver
func Major() (int, error) {
	semverParsableVersion := Get()
	if version := Get(); !strings.HasPrefix(version, "v") {
		semverParsableVersion = "v" + version
	}
	majorVersion := strings.TrimPrefix(semver.Major(semverParsableVersion), "v")
	return strconv.Atoi(majorVersion)
}
