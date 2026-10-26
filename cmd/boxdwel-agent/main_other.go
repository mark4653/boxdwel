//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "boxdwel-agent: the guest agent runs on Linux only")
	os.Exit(1)
}
