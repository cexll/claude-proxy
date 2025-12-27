package handlers

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func parseDurationParam(durationStr string) (time.Duration, error) {
	if strings.HasSuffix(durationStr, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(durationStr, "d"))
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid duration: %s", durationStr)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(durationStr)
}
