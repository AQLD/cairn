//go:build !darwin && !linux

package main

import (
	"context"
	"errors"

	"github.com/AQLD/cairn/client"
)

func cmdShell(context.Context, *client.Client, []string) (int, error) {
	return 1, errors.New("cairn shell needs macOS or Linux")
}
