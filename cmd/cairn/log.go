package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// cairn log WORKSPACE [-f] [-n N] [--kind K] [-v] [--json]
func runLog(ctx context.Context, c *client.Client, args []string) (int, error) {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	follow := fs.Bool("f", false, "keep printing new events as they happen (Ctrl-C to stop)")
	n := fs.Int("n", 50, "show the last N events")
	kind := fs.String("kind", "", `only this kind (exec, save, file_write, …; "session_*" for a prefix)`)
	verbose := fs.Bool("v", false, "also show each command's output tail and errors in full")
	asJSON := fs.Bool("json", false, "print raw events, one JSON object per line")
	var ws string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ws, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if ws == "" && fs.NArg() > 0 {
		ws = fs.Arg(0)
	}
	if ws == "" {
		return 2, errors.New("usage: cairn log WORKSPACE [-f] [-n N] [--kind K] [-v] [--json]")
	}
	print := func(ev client.Event) error {
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(ev)
		}
		printEvent(os.Stdout, ev, *verbose)
		return nil
	}
	q := client.EventQuery{Last: *n, Kind: *kind}
	if *follow {
		return 0, c.FollowEvents(ctx, ws, q, print)
	}
	evs, err := c.Events(ctx, ws, q)
	if err != nil {
		return 1, err
	}
	for _, ev := range evs {
		if err := print(ev); err != nil {
			return 1, err
		}
	}
	return 0, nil
}

func printEvent(w io.Writer, ev client.Event, verbose bool) {
	mark := " "
	if ev.Error != "" {
		mark = "!"
	} else if code, ok := ev.Data["exit_code"].(float64); ok && code != 0 {
		mark = "!"
	}
	fmt.Fprintf(w, "%s %s %-13s %s\n", ev.Time.Local().Format(time.TimeOnly), mark, ev.Kind, ev.Summary)
	if !verbose {
		return
	}
	if ev.Error != "" {
		fmt.Fprintf(w, "      error: %s\n", ev.Error)
	}
	for _, k := range []string{"stdout_tail", "stderr_tail"} {
		if s, _ := ev.Data[k].(string); strings.TrimSpace(s) != "" {
			fmt.Fprintf(w, "      %s:\n", strings.TrimSuffix(k, "_tail"))
			for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
				fmt.Fprintf(w, "        %s\n", line)
			}
		}
	}
}
