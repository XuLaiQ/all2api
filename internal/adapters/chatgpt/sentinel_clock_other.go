//go:build !windows && !linux

package chatgpt

import "time"

var sentinelProcessStart = time.Now()

func sentinelPerformanceMilliseconds() float64 {
	return float64(time.Since(sentinelProcessStart)) / float64(time.Millisecond)
}
