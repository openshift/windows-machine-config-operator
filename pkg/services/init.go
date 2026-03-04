package services

import ctrl "sigs.k8s.io/controller-runtime"

var logFileSize, logFileAge, flushInterval string

func init() {
	log := ctrl.Log.WithName("services").WithName("init")

	var err error
	logFileSize, err = getEnvQuantityString(logFileSizeEnvVar)
	if err != nil {
		log.Error(err, "cannot load environment variable", "name", logFileSizeEnvVar)
	}

	logFileAge, err = getEnvDurationString(logFileAgeEnvVar)
	if err != nil {
		log.Error(err, "cannot load environment variable", "name", logFileAgeEnvVar)
	}

	flushInterval, err = getEnvDurationString(logFlushIntervalEnvVar)
	if err != nil {
		log.Error(err, "cannot load environment variable", "name", logFlushIntervalEnvVar)
	}
}
