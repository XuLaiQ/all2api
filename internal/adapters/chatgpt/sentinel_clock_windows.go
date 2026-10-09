//go:build windows

package chatgpt

import "golang.org/x/sys/windows"

var (
	kernel32       = windows.NewLazySystemDLL("kernel32.dll")
	getTickCount64 = kernel32.NewProc("GetTickCount64")
)

func sentinelPerformanceMilliseconds() float64 {
	value, _, _ := getTickCount64.Call()
	return float64(value)
}
