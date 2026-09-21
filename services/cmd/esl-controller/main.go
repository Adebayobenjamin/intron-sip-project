// Command esl-controller carries calls.
//
// FreeSWITCH opens one TCP connection per call to :8022 and hands the call
// over pre-answer; this service resolves the dialled number against Postgres,
// decides what happens, and drives the call over that socket for its whole
// life.
//
// It reads and writes Postgres directly and does NOT depend on the
// control-plane API to handle a call — that API restarts on every deploy, and
// a per-call question asked of it would make each of those restarts a window
// where inbound calls fail.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := runMain(); err != nil {
		fmt.Fprintf(os.Stderr, "esl-controller: %v\n", err)
		os.Exit(1)
	}
}
