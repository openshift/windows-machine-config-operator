package logconfig

import (
	ctrl "sigs.k8s.io/controller-runtime"
)

func init() {
	log := ctrl.Log.WithName("logconfig").WithName("init")

	var err error
	logFileSize, err = getEnvQuantityString(LogFileSizeEnvVar)
	if err != nil {
		log.Error(err, "cannot load environment variable", "name", LogFileSizeEnvVar)
	}

	logFileAge, err = getEnvDurationString(LogFileAgeEnvVar)
	if err != nil {
		log.Error(err, "cannot load environment variable", "name", LogFileAgeEnvVar)
	}

	flushInterval, err = getEnvDurationString(LogFlushIntervalEnvVar)
	if err != nil {
		log.Error(err, "cannot load environment variable", "name", LogFlushIntervalEnvVar)
	}
}
