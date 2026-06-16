package logconfig

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// LogFileSizeEnvVar is the environment variable name for log file size limit
	LogFileSizeEnvVar = "SERVICES_LOG_FILE_SIZE"
	// LogFileAgeEnvVar is the environment variable name for log file age retention
	LogFileAgeEnvVar = "SERVICES_LOG_FILE_AGE"
	// LogFlushIntervalEnvVar is the environment variable name for log flush interval
	LogFlushIntervalEnvVar = "SERVICES_LOG_FLUSH_INTERVAL"
)

// logFileSize, logFileAge, and flushInterval hold the configuration values
// for log file size, log file age, and flush interval respectively.
var logFileSize, logFileAge, flushInterval string

// GetLogFileSize returns the configured log file size value
func GetLogFileSize() string {
	return logFileSize
}

// GenerateKubeLogRunnerCmd returns the command string to run the given commandPath with kube-log-runner
// logging to the given logfilePath. Log rotation parameters logFileSize, logFileAge, and flushInterval
// are read via environment variables.
func GenerateKubeLogRunnerCmd(kubeLogRunnerPath, commandPath, logfilePath string) string {
	cmdBuilder := strings.Builder{}
	cmdBuilder.WriteString(kubeLogRunnerPath)

	cmdBuilder.WriteString(" -log-file=")
	cmdBuilder.WriteString(logfilePath)

	if logFileSize != "" {
		cmdBuilder.WriteString(" -log-file-size=")
		cmdBuilder.WriteString(logFileSize)
	}

	if logFileAge != "" {
		cmdBuilder.WriteString(" -log-file-age=")
		cmdBuilder.WriteString(logFileAge)
	}

	if flushInterval != "" {
		cmdBuilder.WriteString(" -flush-interval=")
		cmdBuilder.WriteString(flushInterval)
	}

	cmdBuilder.WriteString(" " + commandPath)

	return cmdBuilder.String()
}

// LogConfig logs the effective log rotation configuration when at least one rotation
// parameter is set. Call this after ValidateLogConfig so the globals reflect the
// validated env vars. Silent when nothing is configured (the default state).
func LogConfig(log logr.Logger) {
	if logFileSize == "" && logFileAge == "" && flushInterval == "" {
		return
	}
	log.Info("log rotation configured",
		LogFileSizeEnvVar, logFileSize,
		LogFileAgeEnvVar, logFileAge,
		LogFlushIntervalEnvVar, flushInterval)
}

// ValidateLogConfig parses the log rotation environment variables and sets the package-level
// globals. It returns an error for each variable whose value is present but invalid.
// The valid (or absent) values are applied and globals for invalid ones remain unchanged at "".
// This ensures misconfigured rotation flags is not silently omitted.
func ValidateLogConfig() error {
	var errs []string

	if val, err := getEnvQuantityString(LogFileSizeEnvVar); err != nil {
		errs = append(errs, err.Error())
	} else {
		logFileSize = val
	}

	if val, err := getEnvDurationString(LogFileAgeEnvVar); err != nil {
		errs = append(errs, err.Error())
	} else {
		logFileAge = val
	}

	if val, err := getEnvDurationString(LogFlushIntervalEnvVar); err != nil {
		errs = append(errs, err.Error())
	} else {
		flushInterval = val
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid log configuration: %s", strings.Join(errs, "; "))
	}
	return nil
}

// getEnvQuantityString returns the string value of the environment variable for the given key
// if it represents a valid and non-negative quantity, otherwise returns error
func getEnvQuantityString(key string) (string, error) {
	value := os.Getenv(key)
	value = strings.TrimSpace(value)
	if value == "" {
		// not present
		return "", nil
	}
	// validate value as quantity
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return "", fmt.Errorf("invalid quantity value for %s: %w", key, err)
	}
	if q.Sign() < 0 {
		return "", fmt.Errorf("quantity cannot be negative for %s", key)
	}
	return value, nil
}

// getEnvDurationString returns the string value of the environment variable for the given key
// if it represents a valid and non-negative duration, otherwise returns error
func getEnvDurationString(key string) (string, error) {
	value := os.Getenv(key)
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "-") {
		return "", fmt.Errorf("duration cannot be negative for %s", key)
	}

	// validate value as duration
	if _, err := time.ParseDuration(value); err != nil {
		return "", fmt.Errorf("invalid duration value for %s: %w", key, err)
	}
	return value, nil
}
