// Command sapedbd serves databases over the sapedb protocol.
//
// Everything it needs comes from the environment, so the same image runs in a
// container, under systemd, or on a laptop without a different invocation:
//
//	SAPEDB_SECRET     what connection strings are signed with (required)
//	SAPEDB_ADDR       where to listen                       (default :7433)
//	SAPEDB_DIR        where databases live                  (default /var/lib/sapedb)
//	SAPEDB_TLS_CERT   certificate, with SAPEDB_TLS_KEY
//	SAPEDB_TLS_KEY    its key
//	SAPEDB_INSECURE   1 to serve without TLS, said out loud
//	SAPEDB_ENCRYPT    1 to encrypt every database at rest
//	SAPEDB_LABEL      signing label, if not the default
//	SAPEDB_SHUTDOWN   how long to let connections finish    (default 20s)
//
// There is no logic here on purpose. What this command decides is decided in
// internal/service, where it can be tested without starting a process.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sapedb/sapedb/internal/service"
)

func main() {
	// SIGTERM is how a container is asked to stop, and SIGINT how a person
	// does. Both mean the same thing here.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	if err := service.Run(ctx, service.Env, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
