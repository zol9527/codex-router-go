//go:build darwin

package main

import "syscall"

// hostMemGiB 返回物理内存 GiB（tray 用它对照本地视觉模型的 minRamGib
// 推荐）。读不到返回 0 —— 快照省略该字段，tray 不做内存门槛提示。
// syscall.Sysctl 会裁掉值尾部的零字节（字符串语义），hw.memsize 的
// 高字节常为 0，因此按实际长度小端拼装而不是硬要 8 字节。
func hostMemGiB() float64 {
	raw, err := syscall.Sysctl("hw.memsize")
	if err != nil || len(raw) == 0 || len(raw) > 8 {
		return 0
	}
	var bytes uint64
	for i := len(raw) - 1; i >= 0; i-- {
		bytes = bytes<<8 | uint64(raw[i])
	}
	return float64(bytes) / (1 << 30)
}
