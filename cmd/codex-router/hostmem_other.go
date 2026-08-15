//go:build !darwin

package main

// hostMemGiB 非 darwin 平台无 sysctl；快照省略该字段。
func hostMemGiB() float64 { return 0 }
