//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/AQLD/cairn/client"
)

// cmdShell: cairn shell WORKSPACE [-- COMMAND...] opens an interactive
// terminal in a running workspace (a login shell unless a command is given).
// Exit the shell, or close the window, to leave.
func cmdShell(ctx context.Context, c *client.Client, args []string) (int, error) {
	if len(args) == 0 {
		return 2, errors.New("usage: cairn shell WORKSPACE [-- COMMAND...]")
	}
	id, rest := args[0], args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	rows, cols := termSize(os.Stdout)
	term, err := c.OpenTerminal(ctx, id, rows, cols, os.Stdout, rest...)
	if err != nil {
		return 1, err
	}
	defer term.Close()
	if isTerminal(os.Stdin) {
		restore, err := makeRaw(os.Stdin)
		if err != nil {
			return 1, fmt.Errorf("raw mode: %w", err)
		}
		defer restore()
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				r, c := termSize(os.Stdout)
				_ = term.Resize(r, c)
			}
		}()
	}
	go func() {
		_, _ = io.Copy(term, os.Stdin)
		// stdin ended (a pipe): leave the shell to finish what it was given
	}()
	code, err := term.Wait()
	if err != nil {
		return 1, err
	}
	return code, nil
}
