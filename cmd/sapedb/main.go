// Command sapedb sets a database up and looks inside it.
//
// It exists because declaring is not an operation: a new database can do
// nothing over the wire until somebody puts the first declarations in it, and
// that somebody is on the same host as the file.
//
// There is no logic here. What this command decides is decided in internal/cli,
// where it can be tested without starting a process.
package main

import (
	"os"

	"github.com/sapedb/sapedb/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.LookupEnv, os.Stdin, os.Stdout, os.Stderr))
}
