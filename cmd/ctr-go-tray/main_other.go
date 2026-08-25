//go:build !darwin

package main

import (
	"fmt"
	"runtime"
)

func main() {
	fmt.Printf("ctr-go-tray is macOS-only in this release; current OS is %s\n", runtime.GOOS)
}
