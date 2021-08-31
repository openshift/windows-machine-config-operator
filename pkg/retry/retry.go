package retry

import "time"

const (
	// Count is the number of times we will retry an API call
	Count = 20
	// Interval is the wait time between API calls on a failure
	Interval = 15 * time.Second
	// Timeout is the total time we will wait for an event to occur.
	Timeout = time.Minute * 10
	// ResourceChangeTimeout is the total time waited for a change (create/update/delete) to take place
	ResourceChangeTimeout = time.Minute * 2
)
