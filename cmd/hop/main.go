package main

import (
	"os"

	"github.com/hop-vcs/hop/internal/hop"
)

func main() {
	os.Exit(hop.RunCLI(os.Args[1:], os.Stdout, os.Stderr))
}
