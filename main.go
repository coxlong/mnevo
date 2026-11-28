package main

import (
	"fmt"
	"os"

	"github.com/coxlong/mnevo/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mnevo:", err)
		os.Exit(1)
	}
}
