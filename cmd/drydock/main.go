// Command drydock previews OS configuration changes in a NixOS VM.
package main

import (
	"os"

	"github.com/0xferrous/drydock/internal/drydock"
)

func main() {
	os.Exit(drydock.RunCLI(os.Args[1:], os.Stdout, os.Stderr))
}
