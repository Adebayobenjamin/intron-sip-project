// Command control-plane serves the configuration and action API: operator
// CRUD, read models, and the endpoints that queue work for the ESL controller.
//
// Nothing it serves is in the path of a call. It is also the ONLY migrator —
// esl-controller shares the database but never applies migrations, because two
// processes racing golang-migrate at startup is a deploy-ordering problem
// nobody wants to debug at 3am.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := runMain(); err != nil {
		// Straight to stderr, not the structured logger: this path includes
		// the case where configuration failed, which is before there is a
		// logger to use.
		fmt.Fprintf(os.Stderr, "control-plane: %v\n", err)
		os.Exit(1)
	}
}
