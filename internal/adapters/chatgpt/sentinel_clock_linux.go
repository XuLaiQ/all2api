//go:build linux

package chatgpt

import (
	"os"
	"strconv"
	"strings"
)

func sentinelPerformanceMilliseconds() float64 {
	data, err := os.ReadFile("/proc/uptime")
	if err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 0 {
			if seconds, parseErr := strconv.ParseFloat(fields[0], 64); parseErr == nil {
				return seconds * 1000
			}
		}
	}
	return 0
}
