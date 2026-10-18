// cairn is the command-line tool for a Cairn engine. It talks to the engine only
// through the REST API.
//
//	cairn create [--name NAME] [--template T]
//	cairn template import NAME ROOTFS.tar | template pull NAME IMAGE | template ls | template rm NAME
//	cairn ls
//	cairn exec WORKSPACE -- COMMAND [ARGS...]
//	cairn put WORKSPACE LOCAL_FILE REMOTE_PATH
//	cairn get WORKSPACE REMOTE_PATH [LOCAL_FILE]
//	cairn save WORKSPACE [--full]
//	cairn saves WORKSPACE
//	cairn resume SAVE
//	cairn copy SAVE [--count N]
//	cairn rm-save SAVE
//	cairn undelete SAVE
//	cairn rm WORKSPACE
//	cairn info
//
// CAIRN_ENDPOINT (default http://127.0.0.1:7420) and CAIRN_TOKEN select the engine.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/AQLD/cairn/client"
)

const usage = `usage: cairn <command> [arguments]

commands:
  create [--name NAME] [--template T]     create a workspace (from a template)
  template import NAME ROOTFS.tar         import a root filesystem tar as a template
                                          (e.g. docker export $(docker create python:3.12-slim) > python.tar)
  template pull NAME IMAGE [--insecure]   build a template from a container image in a registry
                                          (login from CAIRN_REGISTRY_USERNAME / CAIRN_REGISTRY_PASSWORD)
  template ls | template rm NAME          list or delete templates
  ls                                      list workspaces
  exec WORKSPACE -- COMMAND [ARGS...]     run a command
  put WORKSPACE LOCAL_FILE REMOTE_PATH    upload a file
  get WORKSPACE REMOTE_PATH [LOCAL_FILE]  download a file (stdout if no LOCAL_FILE)
  save WORKSPACE [--full]                 save (quick by default)
  saves WORKSPACE                         list a workspace's saves
  resume SAVE                             restore a workspace to a save
  copy SAVE [--count N]                   make workspaces from a save
  verify SAVE                             re-check a save's checksums now
  rm-save SAVE                            delete a save (recoverable for the retention window)
  undelete SAVE                           bring back a deleted save
  pause WORKSPACE                         free its memory now; the next command wakes it
  rm WORKSPACE                            delete a workspace (saves are kept)
  info                                    show the engine's runtime and capabilities

environment:
  CAIRN_ENDPOINT  engine URL (default http://127.0.0.1:7420)
  CAIRN_TOKEN     API token
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(usage)
		return
	}
	endpoint := os.Getenv("CAIRN_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:7420"
	}
	c := client.New(endpoint, os.Getenv("CAIRN_TOKEN"))
	code, err := run(context.Background(), c, os.Args[1], os.Args[2:])
	if err != nil {
		if _, isAPI := err.(*client.Error); isAPI {
			fmt.Fprintln(os.Stderr, err) // already prefixed "cairn:"
		} else {
			fmt.Fprintln(os.Stderr, "cairn:", err)
		}
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(ctx context.Context, c *client.Client, cmd string, args []string) (int, error) {
	switch cmd {
	case "create":
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		name := fs.String("name", "", "workspace name")
		template := fs.String("template", "", "template to start from (default: the engine's)")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		ws, err := c.CreateWorkspace(ctx, client.CreateWorkspace{Name: *name, Template: *template})
		if err != nil {
			return 1, err
		}
		fmt.Println(ws.ID)

	case "ls":
		ws, err := c.ListWorkspaces(ctx)
		if err != nil {
			return 1, err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tSTATE\tCREATED")
		for _, w := range ws {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", w.ID, w.Name, w.State, w.CreatedAt.Local().Format(time.DateTime))
		}
		return 0, tw.Flush()

	case "exec":
		if len(args) < 3 || args[1] != "--" {
			return 2, errors.New("usage: cairn exec WORKSPACE -- COMMAND [ARGS...]")
		}
		res, err := c.ExecStream(ctx, args[0], client.Exec{Command: args[2:]}, func(ev client.ExecEvent) {
			if ev.Stream == "stderr" {
				os.Stderr.Write(ev.Data)
			} else {
				os.Stdout.Write(ev.Data)
			}
		})
		if err != nil {
			return 1, err
		}
		if res.TimedOut {
			return 124, errors.New("command timed out")
		}
		return res.ExitCode, nil

	case "put":
		if len(args) != 3 {
			return 2, errors.New("usage: cairn put WORKSPACE LOCAL_FILE REMOTE_PATH")
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return 1, err
		}
		return 0, c.WriteFile(ctx, args[0], args[2], data)

	case "get":
		if len(args) < 2 || len(args) > 3 {
			return 2, errors.New("usage: cairn get WORKSPACE REMOTE_PATH [LOCAL_FILE]")
		}
		data, err := c.ReadFile(ctx, args[0], args[1])
		if err != nil {
			return 1, err
		}
		if len(args) == 3 {
			return 0, os.WriteFile(args[2], data, 0o644)
		}
		_, err = os.Stdout.Write(data)
		return 0, err

	case "save":
		fs := flag.NewFlagSet("save", flag.ContinueOnError)
		full := fs.Bool("full", false, "also save memory and processes, then stop the workspace")
		ws, rest := splitID(args)
		if err := fs.Parse(rest); err != nil || ws == "" {
			return 2, errors.New("usage: cairn save WORKSPACE [--full]")
		}
		kind := client.QuickSave
		if *full {
			kind = client.FullSave
		}
		sv, err := c.Save(ctx, ws, kind)
		if err != nil {
			return 1, err
		}
		fmt.Println(sv.ID)

	case "saves":
		if len(args) != 1 {
			return 2, errors.New("usage: cairn saves WORKSPACE")
		}
		svs, err := c.ListSaves(ctx, args[0])
		if err != nil {
			return 1, err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tKIND\tSTATE\tSIZE\tCREATED")
		for _, s := range svs {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", s.ID, s.Kind, s.State, s.Size, s.CreatedAt.Local().Format(time.DateTime))
		}
		return 0, tw.Flush()

	case "resume":
		if len(args) != 1 {
			return 2, errors.New("usage: cairn resume SAVE")
		}
		ws, err := c.Resume(ctx, args[0])
		if err != nil {
			return 1, err
		}
		fmt.Println(ws.ID)

	case "copy":
		fs := flag.NewFlagSet("copy", flag.ContinueOnError)
		count := fs.Int("count", 1, "number of copies")
		sv, rest := splitID(args)
		if err := fs.Parse(rest); err != nil || sv == "" {
			return 2, errors.New("usage: cairn copy SAVE [--count N]")
		}
		ws, err := c.Copy(ctx, sv, *count)
		if err != nil {
			return 1, err
		}
		for _, w := range ws {
			fmt.Println(w.ID)
		}

	case "template":
		switch {
		case len(args) == 3 && args[0] == "import":
			f, err := os.Open(args[2])
			if err != nil {
				return 1, err
			}
			defer f.Close()
			t, err := c.ImportTemplate(ctx, args[1], f)
			if err != nil {
				return 1, err
			}
			fmt.Printf("%s  %d bytes  sha256:%s\n", t.Name, t.Size, t.SHA256[:12])
		case (len(args) == 3 || len(args) == 4 && args[3] == "--insecure") && args[0] == "pull":
			// the login comes from the environment, never the command line (shell history)
			t, err := c.PullTemplate(ctx, args[1], client.PullTemplate{
				Image: args[2], Insecure: len(args) == 4,
				Username: os.Getenv("CAIRN_REGISTRY_USERNAME"), Password: os.Getenv("CAIRN_REGISTRY_PASSWORD")})
			if err != nil {
				return 1, err
			}
			fmt.Printf("%s  %d bytes  from %s@%s\n", t.Name, t.Size, t.Image, t.ImageDigest)
		case len(args) == 1 && args[0] == "ls":
			ts, err := c.ListTemplates(ctx)
			if err != nil {
				return 1, err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSIZE\tCREATED")
			for _, t := range ts {
				fmt.Fprintf(tw, "%s\t%d\t%s\n", t.Name, t.Size, t.CreatedAt.Local().Format(time.DateTime))
			}
			return 0, tw.Flush()
		case len(args) == 2 && args[0] == "rm":
			return 0, c.DeleteTemplate(ctx, args[1])
		default:
			return 2, errors.New("usage: cairn template import NAME ROOTFS.tar | template pull NAME IMAGE [--insecure] | template ls | template rm NAME")
		}

	case "rm-save":
		if len(args) != 1 {
			return 2, errors.New("usage: cairn rm-save SAVE")
		}
		if err := c.DeleteSave(ctx, args[0]); err != nil {
			return 1, err
		}
		sv, err := c.GetSave(ctx, args[0])
		if err == nil && sv.PurgeAfter != nil {
			fmt.Printf("deleted; recoverable with `cairn undelete %s` until %s\n", sv.ID, sv.PurgeAfter.Local().Format(time.DateTime))
		}

	case "verify":
		if len(args) != 1 {
			return 2, errors.New("usage: cairn verify SAVE")
		}
		sv, err := c.VerifySave(ctx, args[0])
		if err != nil {
			return 1, err
		}
		fmt.Println(sv.ID, sv.State)

	case "undelete":
		if len(args) != 1 {
			return 2, errors.New("usage: cairn undelete SAVE")
		}
		sv, err := c.UndeleteSave(ctx, args[0])
		if err != nil {
			return 1, err
		}
		fmt.Println(sv.ID, sv.State)

	case "pause":
		if len(args) != 1 {
			return 2, errors.New("usage: cairn pause WORKSPACE")
		}
		ws, err := c.PauseWorkspace(ctx, args[0])
		if err != nil {
			return 1, err
		}
		fmt.Println(ws.ID, ws.State)

	case "rm":
		if len(args) != 1 {
			return 2, errors.New("usage: cairn rm WORKSPACE")
		}
		return 0, c.DeleteWorkspace(ctx, args[0])

	case "info":
		info, err := c.Info(ctx)
		if err != nil {
			return 1, err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return 0, enc.Encode(info)

	default:
		fmt.Fprint(os.Stderr, usage)
		return 2, fmt.Errorf("unknown command %q", cmd)
	}
	return 0, nil
}

// splitID takes the first non-flag argument as the ID, so flags may come
// before or after it.
func splitID(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, append(append([]string{}, args[:i]...), args[i+1:]...)
		}
	}
	return "", args
}
