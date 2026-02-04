package services

var logFileSize, logFileAge, flushInterval string

func init() {
	logFileSize = getEnvQuantityOrDefault(logFileSizeEnvVar, defaultLogFileSize)
	logFileAge = getEnvDurationOrDefault(logFileAgeEnvVar, defaultLogFileAge)
	flushInterval = getEnvDurationOrDefault(logFlushIntervalEnvVar, defaultFlushInterval)
}
