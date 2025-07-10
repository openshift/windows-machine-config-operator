package controllers

import (
	"fmt"
)

type UpgradeLimitExceededError struct {
	NodeName string
	Limit    int
}

func (e *UpgradeLimitExceededError) Error() string {
	return fmt.Sprintf("Cannot mark node %s as upgrading. Maximum parallel upgrades reached (%d)", e.NodeName, e.Limit)
}
