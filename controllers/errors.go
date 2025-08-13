package controllers

import (
	"fmt"
)

type UpgradeLimitExceededError struct {
	NodeName string
	Count    int
	Max      int
}

func (e *UpgradeLimitExceededError) Error() string {
	return fmt.Sprintf("Cannot mark node %s as upgrading. Current number of upgrading nodes is (%d). Max number of upgrading nodes is (%d)", e.NodeName, e.Count, e.Max)
}
