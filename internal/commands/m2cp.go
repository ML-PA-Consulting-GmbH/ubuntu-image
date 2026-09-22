package commands

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// M2cpCLI is the name of the appstore CLI binary that ubuntu-image
// shells out to for trust bootstrap, model push, snap-info lookups,
// download-URL minting, and assertion fetches.
//
// The binary is scheduled to be renamed in the future; every
// exec.Command / exec.LookPath / human-facing message that
// references it goes through this constant so the rename is a
// one-line change.
const M2cpCLI = "m2cp"

// M2cpOutput is what one m2cp invocation wrote. Stdout carries the
// payload (an assertion bundle, a JSON envelope, a download URL);
// Stderr carries m2cp's own narration, including the Task-Id that
// identifies the request server-side.
type M2cpOutput struct {
	Stdout []byte
	Stderr []byte
}

// M2cpRun runs `m2cp <args...>` and returns what it wrote.
//
// Every invocation announces itself on stderr as a copy-pasteable
// command line, and reports its exit status, duration and payload
// size when it returns. m2cp's own stderr is passed through as it
// arrives, so its Task-Id lines stay attached to the call that
// produced them. A build log therefore records every question the
// builder asked the appstore, in order, with the answer sizes --
// which is what a support case needs to tell "m2cp said no" apart
// from "m2cp answered, and we rejected the answer".
//
// Stdout is returned on failure too: m2cp reports some server-side
// errors in a JSON envelope on stdout while still exiting non-zero,
// and callers parse it for the real message.
func M2cpRun(args ...string) (M2cpOutput, error) {
	return runM2cp(true, args...)
}

// M2cpCapture runs `m2cp <args...>` keeping both streams to itself,
// for the one call whose output is a secret: `m2cp user jwt` writes
// the session token, and writes it to stderr (its `println`), so
// passing stderr through would put a bearer token in every build log.
func M2cpCapture(args ...string) (M2cpOutput, error) {
	return runM2cp(false, args...)
}

func runM2cp(passThroughStderr bool, args ...string) (M2cpOutput, error) {
	line := M2cpCLI + " " + shellJoin(args)
	fmt.Fprintf(os.Stderr, "   $ %s\n", line)

	cmd := exec.Command(M2cpCLI, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Writer(&stderr)
	if passThroughStderr {
		cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	}
	start := time.Now()
	err := cmd.Run()
	took := time.Since(start).Round(10 * time.Millisecond)
	out := M2cpOutput{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}

	if err != nil {
		fmt.Fprintf(os.Stderr, "     FAILED after %s: %v\n", took, err)
		if passThroughStderr {
			return out, fmt.Errorf("%s: %w%s", line, err, stderrSuffix(out.Stderr))
		}
		return out, fmt.Errorf("%s: %w", line, err)
	}
	fmt.Fprintf(os.Stderr, "     ok %s, %d byte(s)\n", took, len(out.Stdout))
	return out, nil
}

// M2cpStream runs `m2cp <args...>` with both its streams wired
// straight to our stderr, for calls whose output is meant for the
// reader rather than for parsing.
func M2cpStream(args ...string) error {
	line := M2cpCLI + " " + shellJoin(args)
	fmt.Fprintf(os.Stderr, "   $ %s\n", line)

	cmd := exec.Command(M2cpCLI, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	start := time.Now()
	err := cmd.Run()
	took := time.Since(start).Round(10 * time.Millisecond)
	if err != nil {
		fmt.Fprintf(os.Stderr, "     FAILED after %s: %v\n", took, err)
		return fmt.Errorf("%s: %w", line, err)
	}
	fmt.Fprintf(os.Stderr, "     ok %s\n", took)
	return nil
}

// stderrSuffix appends m2cp's stderr to an error message, trimmed
// and on one line, when it carries anything beyond whitespace.
func stderrSuffix(stderr []byte) string {
	s := strings.TrimSpace(string(stderr))
	if s == "" {
		return ""
	}
	return fmt.Sprintf(" (stderr: %s)", strings.ReplaceAll(s, "\n", " | "))
}

// shellJoin renders args as a command line that can be pasted back
// into a shell: anything holding whitespace or a quote is single-quoted.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t\n'\"$`\\") {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			quoted[i] = a
		}
	}
	return strings.Join(quoted, " ")
}
