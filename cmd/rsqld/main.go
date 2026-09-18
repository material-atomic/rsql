// Command rsqld serves databases over the rsql protocol.
//
// Everything it needs comes from the environment, so the same image runs in a
// container, under systemd, or on a laptop without a different invocation:
//
//	RSQL_SECRET     what connection strings are signed with (required)
//	RSQL_ADDR       where to listen                       (default :7433)
//	RSQL_DIR        where databases live                  (default /var/lib/rsql)
//	RSQL_TLS_CERT   certificate, with RSQL_TLS_KEY
//	RSQL_TLS_KEY    its key
//	RSQL_INSECURE   1 to serve without TLS, said out loud
//	RSQL_ENCRYPT    1 to encrypt every database at rest
//	RSQL_LABEL      signing label, if not the default
//	RSQL_SHUTDOWN   how long to let connections finish    (default 20s)
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

	"github.com/material-atomic/rsql/internal/service"
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
