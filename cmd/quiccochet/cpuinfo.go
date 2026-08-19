package main

import (
	"fmt"
	"runtime"

	"github.com/originnova555-hue/esp-tun/internal/crypto"
)

func cmdCPUInfo() error {
	fmt.Printf("arch          : %s\n", runtime.GOARCH)
	fmt.Printf("cores         : %d\n", runtime.NumCPU())
	fmt.Printf("hardware AES  : %t\n", crypto.HardwareAES())
	for _, want := range []string{crypto.Auto, crypto.AESGCM, crypto.ChaCha20} {
		fmt.Printf("cipher=%-18s -> %s\n", want, crypto.Select(want))
	}
	return nil
}
