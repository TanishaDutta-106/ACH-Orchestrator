// cmd/server/main.go
//
// Entry point for the ACH Payment Retry Orchestrator server.
//
// Phase 1: This file is intentionally minimal. The HTTP server, Temporal
// worker, and Redis client will be wired up in Phase 2. For now, the binary
// compiles and confirms that all imported packages resolve correctly.
package main

import "fmt"

func main() {
	// Phase 2 will initialize:
	//   - Config loading from environment variables
	//   - PostgreSQL connection pool (db.NewRepository)
	//   - Temporal client and worker registration
	//   - HTTP/gRPC API server
	fmt.Println("ACH Payment Retry Orchestrator — Phase 1 (domain + DB layer only)")
}
