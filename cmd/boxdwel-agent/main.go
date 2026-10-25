package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "boxdwel-agent: stub only; guest initialization, READY, PTY and protocol are not implemented")
	os.Exit(1)
}
