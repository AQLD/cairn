// cairn is the command-line tool for a Cairn engine. It talks to the engine only
// through the REST API.
//
//	cairn up [--build] | down [--purge] | status
//	cairn create [--name NAME] [--template T] [--network | --allow NAME|RANGE ...]
//	cairn template import NAME ROOTFS.tar | template pull NAME IMAGE | template ls | template rm NAME
//	cairn ls
//	cairn exec WORKSPACE -- COMMAND [ARGS...]
//	cairn put WORKSPACE LOCAL_FILE REMOTE_PATH
//	cairn get WORKSPACE REMOTE_PATH [LOCAL_FILE]
//	cairn save WORKSPACE [--full [--stop]]
//	cairn saves WORKSPACE
//	cairn diff SAVE [--from SAVE] [--limit N] [--json]
//	cairn resume SAVE
//	cairn copy SAVE [--count N]
//	cairn rm-save SAVE
//	cairn undelete SAVE
//	cairn pin SAVE | unpin SAVE
//	cairn log WORKSPACE [-f] [-n N] [--kind K] [-v] [--json]
//	cairn rm WORKSPACE
//	cairn info
//
// CAIRN_ENDPOINT and CAIRN_TOKEN select the engine; without them, the config
// file `cairn up` writes (client.ConfigPath), else http://127.0.0.1:7420.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/AQLD/cairn/client"
)

const usage = `usage: cairn <command> [arguments]

commands:
  up [--build] [--port P] [--template NAME=IMAGE]
                                          start a local engine in Docker (gVisor workspaces,
                                          full saves) and write the client config
  down [--purge]                          stop it (workspaces kept); --purge deletes everything
  status                                  show the engine and its workspaces
  setup [--project] [--agents LIST]       connect coding agents (Claude Code, Codex, pi, Cursor):
                                          engine, skill and MCP; --project gives this repository
                                          a workspace and tells its agents to use it
  doctor                                  check the engine, agents and project; say how to fix
  run [--no-push] -- COMMAND [ARGS...]    push this project's edits into its workspace, run there
  version                                 print this CLI's version
  create [--name NAME] [--template T] [--network | --allow NAME...] [--memory MB] [--disk MB] [--cpus N]
                                          create a workspace (from a template)
  secret set NAME --host HOST...          store a secret (value read from stdin; sent only to those hosts)
  secret ls | secret rm NAME              list (never values) or delete secrets
  diagnostics [-o FILE]                   a bundle for bug reports: facts, never workspace contents (admin)
  audit [--key ID] [--kind K] [--last N]  who did what, across workspaces (admin)
  key create NAME [--scope read|write|admin] [--own]  an API key for one caller (token shown once)
  key ls | key rm ID                      list or revoke API keys
  shell WORKSPACE [-- COMMAND...]         an interactive terminal in a running workspace
  template warmup NAME [-- COMMAND...]   warm workspaces of NAME arrive with COMMAND already run (none: clear)
  template import NAME ROOTFS.tar         import a root filesystem tar as a template
                                          (e.g. docker export $(docker create python:3.12-slim) > python.tar)
  template pull NAME IMAGE [--insecure]   build a template from a container image in a registry
                                          (login from CAIRN_REGISTRY_USERNAME / CAIRN_REGISTRY_PASSWORD)
  template ls | template rm NAME          list or delete templates
  ls                                      list workspaces
  exec WORKSPACE -- COMMAND [ARGS...]     run a command
  session new WORKSPACE [python|shell]    start a session (an interpreter that keeps its state)
  session run WORKSPACE SESSION [CODE]    run code in it (CODE, or stdin if none)
  session ls WORKSPACE | session rm WORKSPACE SESSION
                                          list or stop sessions
  put WORKSPACE LOCAL_FILE REMOTE_PATH    upload a file
  get WORKSPACE REMOTE_PATH [LOCAL_FILE]  download a file (stdout if no LOCAL_FILE)
  push WORKSPACE LOCAL_DIR [REMOTE_DIR] [--exclude PATTERN]...
                                          copy a folder in (default /root/<its name>), merging
  pull WORKSPACE REMOTE_DIR [LOCAL_DIR]   copy a folder out (default ./<its name>), merging
  save WORKSPACE [--full [--stop]]        save files (quick); --full adds memory and running processes
                                          and keeps the workspace running (--stop: leave it stopped)
  saves WORKSPACE                         list a workspace's saves
  diff SAVE [--from SAVE] [--limit N] [--json]
                                          files added (A), modified (M) and removed (D) since
                                          the save it came from, or since --from
  resume SAVE                             restore a workspace to a save
  copy SAVE [--count N]                   make workspaces from a save
  verify SAVE                             re-check a save's checksums now
  rm-save SAVE                            delete a save (recoverable for the retention window)
  undelete SAVE                           bring back a deleted save
  pin SAVE | unpin SAVE                   keep a save from pruning and deletion, or stop keeping it
  pause WORKSPACE                         free its memory now; the next command wakes it
  log WORKSPACE [-f] [-n N] [--kind K] [-v] [--json]
                                          what was done in it: commands, files, saves, sessions
                                          (-f: watch live; -v: with output tails)
  rm WORKSPACE                            delete a workspace (saves are kept)
  info                                    show the engine's runtime and capabilities
  mcp [--workspace ID]                    serve the Model Context Protocol on stdio, for coding agents
  mcp install CLIENT [--project] [--write]
                                          register Cairn with claude, cursor, codex, vscode, windsurf or goose

In a project set up with "cairn setup --project", "." names its workspace: cairn exec . -- make

environment:
  CAIRN_ENDPOINT  engine URL (default: from the config cairn up writes, else http://127.0.0.1:7420)
  CAIRN_TOKEN     API token
  CAIRN_CA_CERT   PEM file of CAs to trust for an https endpoint (a private CA)
  CAIRN_CONFIG    config file (default ~/.config/cairn/config.json)
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(usage)
		return
	}
	ctx := context.Background()
	var code int
	var err error
	switch os.Args[1] {
	case "version", "--version":
		fmt.Println(version())
	case "up":
		code, err = cmdUp(ctx, os.Args[2:])
	case "down":
		code, err = cmdDown(ctx, os.Args[2:])
	default:
		var c *client.Client
		if c, err = client.FromConfig(); err == nil {
			code, err = run(ctx, c, os.Args[1], os.Args[2:])
		}
	}
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

// execStream runs a command, passing its output through as it comes, and
// returns its exit code.
func execStream(ctx context.Context, c *client.Client, ws string, req client.Exec) (int, error) {
	res, err := c.ExecStream(ctx, ws, req, func(ev client.ExecEvent) {
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
}

func run(ctx context.Context, c *client.Client, cmd string, args []string) (int, error) {
	args, err := resolveDot(cmd, args)
	if err != nil {
		return 2, err
	}
	switch cmd {
	case "run":
		return cmdRun(ctx, c, args)
	case "setup":
		return cmdSetup(ctx, c, args)
	case "doctor":
		return cmdDoctor(ctx, c, args)
	case "status":
		return cmdStatus(ctx, c, args)
	case "mcp":
		return runMCP(ctx, c, args)

	case "create":
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		name := fs.String("name", "", "workspace name")
		template := fs.String("template", "", "template to start from (default: the engine's)")
		network := fs.Bool("network", false, "allow outbound network access")
		memory := fs.Int64("memory", 0, "memory limit in MB (default: the engine's)")
		disk := fs.Int64("disk", 0, "disk size in MB (default: the engine's)")
		cpus := fs.Float64("cpus", 0, "CPU limit, in CPUs (default: the engine's)")
		var allow stringList
		fs.Var(&allow, "allow", "allow outbound traffic only to this name (pypi.org), name suffix (*.github.com) or IPv4 range; repeat it; implies --network")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		ws, err := c.CreateWorkspace(ctx, client.CreateWorkspace{Name: *name, Template: *template, Network: *network, Egress: allow,
			Limits: client.Limits{MemoryMB: *memory, DiskMB: *disk, CPUs: *cpus}})
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
		return execStream(ctx, c, args[0], client.Exec{Command: args[2:]})

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

	case "push":
		fs := flag.NewFlagSet("push", flag.ContinueOnError)
		var exclude stringList
		fs.Var(&exclude, "exclude", "leave out names or paths matching this pattern (repeatable), e.g. node_modules")
		pos, err := interleaved(fs, args)
		if err != nil || len(pos) < 2 || len(pos) > 3 {
			return 2, errors.New("usage: cairn push WORKSPACE LOCAL_DIR [REMOTE_DIR] [--exclude PATTERN]...")
		}
		remote := ""
		if len(pos) == 3 {
			remote = pos[2]
		} else if remote, err = defaultRemote(pos[1]); err != nil {
			return 1, err
		}
		st, err := c.PushDir(ctx, pos[0], pos[1], remote, client.PushOptions{Exclude: exclude})
		if err != nil {
			return 1, err
		}
		fmt.Printf("%s: %d files, %d folders, %d links, %d bytes\n", remote, st.Files, st.Dirs, st.Links, st.Bytes)

	case "pull":
		if len(args) < 2 || len(args) > 3 {
			return 2, errors.New("usage: cairn pull WORKSPACE REMOTE_DIR [LOCAL_DIR]")
		}
		local := path.Base(path.Clean("/" + args[1]))
		if len(args) == 3 {
			local = args[2]
		}
		st, err := c.PullDir(ctx, args[0], args[1], local)
		if err != nil {
			return 1, err
		}
		fmt.Printf("%s: %d files, %d folders, %d links, %d bytes\n", local, st.Files, st.Dirs, st.Links, st.Bytes)

	case "save":
		fs := flag.NewFlagSet("save", flag.ContinueOnError)
		full := fs.Bool("full", false, "also save memory and running processes; the workspace keeps running")
		stop := fs.Bool("stop", false, "with --full: leave the workspace stopped (its memory freed) until the save is resumed")
		ws, rest := splitID(args)
		if err := fs.Parse(rest); err != nil || ws == "" {
			return 2, errors.New("usage: cairn save WORKSPACE [--full [--stop]]")
		}
		if *stop && !*full {
			return 2, errors.New("--stop needs --full (a quick save never stops the workspace)")
		}
		opts := client.SaveOptions{Kind: client.QuickSave}
		if *full {
			opts = client.SaveOptions{Kind: client.FullSave, KeepRunning: !*stop}
		}
		sv, err := c.SaveWith(ctx, ws, opts)
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
		fmt.Fprintln(tw, "ID\tKIND\tSTATE\tSIZE\tCREATED\tPINNED")
		for _, s := range svs {
			pinned := ""
			if s.Pinned {
				pinned = "yes"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", s.ID, s.Kind, s.State, s.Size, s.CreatedAt.Local().Format(time.DateTime), pinned)
		}
		return 0, tw.Flush()

	case "diff":
		fs := flag.NewFlagSet("diff", flag.ContinueOnError)
		from := fs.String("from", "", "compare with this save instead of the one it came from")
		limit := fs.Int("limit", 0, "list at most N changes (default 1000)")
		asJSON := fs.Bool("json", false, "print the engine's JSON")
		to, rest := splitID(args)
		if err := fs.Parse(rest); err != nil || to == "" || fs.NArg() != 0 {
			return 2, errors.New("usage: cairn diff SAVE [--from SAVE] [--limit N] [--json]")
		}
		d, err := c.DiffSaves(ctx, to, *from, *limit)
		if err != nil {
			return 1, err
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return 0, enc.Encode(d)
		}
		mark := map[string]string{"added": "A", "modified": "M", "removed": "D"}
		for _, ch := range d.Changes {
			suffix := ""
			if ch.Type == "dir" {
				suffix = "/"
			}
			fmt.Printf("%s %s%s\n", mark[ch.Change], ch.Path, suffix)
		}
		more := ""
		if d.Truncated {
			more = fmt.Sprintf("; %d listed, --limit for more", len(d.Changes))
		}
		fmt.Fprintf(os.Stderr, "%d added, %d modified, %d removed since %s%s\n", d.Added, d.Modified, d.Removed, d.From, more)

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
		fmt.Fprintln(os.Stderr, "cairn: each copy is a whole workspace; `cairn rm ID` the ones you don't keep")

	case "secret":
		switch {
		case len(args) >= 2 && args[0] == "set":
			fs := flag.NewFlagSet("secret set", flag.ContinueOnError)
			var hosts stringList
			fs.Var(&hosts, "host", "a host the secret may be sent to (github.com, *.githubusercontent.com, an IPv4 range); repeat it")
			if err := fs.Parse(args[2:]); err != nil {
				return 2, err
			}
			// the value comes from stdin, never the command line (shell history, ps)
			value, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
			if err != nil {
				return 1, err
			}
			v := strings.TrimRight(string(value), "\r\n")
			if v == "" {
				return 2, errors.New("secret set reads the value from stdin: echo \"$TOKEN\" | cairn secret set NAME --host HOST")
			}
			sec, err := c.SetSecret(ctx, args[1], v, hosts)
			if err != nil {
				return 1, err
			}
			fmt.Printf("%s  for %s\n", sec.Name, strings.Join(sec.Hosts, ", "))
		case len(args) == 1 && args[0] == "ls":
			list, err := c.ListSecrets(ctx)
			if err != nil {
				return 1, err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tHOSTS\tUPDATED")
			for _, s := range list {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Name, strings.Join(s.Hosts, ","), s.UpdatedAt.Local().Format(time.DateTime))
			}
			tw.Flush()
		case len(args) == 2 && args[0] == "rm":
			if err := c.DeleteSecret(ctx, args[1]); err != nil {
				return 1, err
			}
		default:
			return 2, errors.New("usage: cairn secret set NAME --host HOST... (value on stdin) | secret ls | secret rm NAME")
		}

	case "diagnostics":
		fs := flag.NewFlagSet("diagnostics", flag.ContinueOnError)
		out := fs.String("o", "cairn-diagnostics-"+time.Now().Format("20060102-150405")+".tar.gz", "file to write the bundle to")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		f, err := os.Create(*out)
		if err != nil {
			return 1, err
		}
		if err := c.Diagnostics(ctx, f); err != nil {
			f.Close()
			os.Remove(*out)
			return 1, err
		}
		if err := f.Close(); err != nil {
			return 1, err
		}
		fmt.Printf("%s: engine, host and state facts; no workspace names, files, commands, outputs or secrets (open it to check before sending)\n", *out)

	case "audit":
		fs := flag.NewFlagSet("audit", flag.ContinueOnError)
		key := fs.String("key", "", "only what this key did (an ID from cairn key ls, or operator)")
		kind := fs.String("kind", "", "only this kind of action (exec, save, key_create, secret_set, …)")
		last := fs.Int("last", 50, "the newest N actions")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		evs, err := c.Audit(ctx, *key, client.EventQuery{Kind: *kind, Last: *last})
		if err != nil {
			return 1, err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "TIME\tKEY\tWORKSPACE\tWHAT")
		for _, ev := range evs {
			who, ws := ev.Key, ev.WorkspaceID
			if who == "" {
				who = "-"
			}
			if ws == "" {
				ws = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", ev.Time.Local().Format(time.DateTime), who, ws, ev.Summary)
		}
		tw.Flush()

	case "key":
		switch {
		case len(args) >= 2 && args[0] == "create":
			fs := flag.NewFlagSet("key create", flag.ContinueOnError)
			scope := fs.String("scope", "write", "what the key may do: read, write or admin")
			own := fs.Bool("own", false, "the key sees and uses only the workspaces it makes")
			if err := fs.Parse(args[2:]); err != nil {
				return 2, err
			}
			create := c.CreateKey
			if *own {
				create = c.CreateOwnKey
			}
			k, err := create(ctx, args[1], *scope)
			if err != nil {
				return 1, err
			}
			fmt.Fprintf(os.Stderr, "%s (%s, %s): the token below is shown only now\n", k.ID, k.Name, k.Scope)
			fmt.Println(k.Token)
		case len(args) == 1 && args[0] == "ls":
			list, err := c.ListKeys(ctx)
			if err != nil {
				return 1, err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tSCOPE\tCREATED\tREVOKED")
			for _, k := range list {
				revoked := ""
				if k.RevokedAt != nil {
					revoked = k.RevokedAt.Local().Format(time.DateTime)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", k.ID, k.Name, k.Scope, k.CreatedAt.Local().Format(time.DateTime), revoked)
			}
			tw.Flush()
		case len(args) == 2 && args[0] == "rm":
			if err := c.RevokeKey(ctx, args[1]); err != nil {
				return 1, err
			}
		default:
			return 2, errors.New("usage: cairn key create NAME [--scope read|write|admin] | key ls | key rm ID")
		}

	case "shell":
		return cmdShell(ctx, c, args)

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
		case len(args) >= 2 && args[0] == "warmup":
			cmd := args[2:]
			if len(cmd) > 0 && cmd[0] == "--" {
				cmd = cmd[1:]
			}
			t, err := c.SetTemplateWarmup(ctx, args[1], cmd)
			if err != nil {
				return 1, err
			}
			if len(t.Warmup) == 0 {
				fmt.Printf("%s: no warm-up\n", t.Name)
			} else {
				fmt.Printf("%s: warm workspaces will have run: %s\n", t.Name, strings.Join(t.Warmup, " "))
			}
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

	case "session":
		switch {
		case (len(args) == 2 || len(args) == 3) && args[0] == "new":
			language := "python"
			if len(args) == 3 {
				language = args[2]
			}
			s, err := c.CreateSession(ctx, args[1], language)
			if err != nil {
				return 1, err
			}
			fmt.Println(s.ID)
		case (len(args) == 3 || len(args) == 4) && args[0] == "run":
			var code string
			if len(args) == 4 {
				code = args[3]
			} else {
				b, err := io.ReadAll(os.Stdin)
				if err != nil {
					return 1, err
				}
				code = string(b)
			}
			res, err := c.RunInSession(ctx, args[1], args[2], code, 0)
			if err != nil {
				return 1, err
			}
			os.Stdout.WriteString(res.Stdout)
			os.Stderr.WriteString(res.Stderr)
			switch {
			case res.Ended:
				return 1, errors.New("the session ended during this cell; start a new one")
			case res.TimedOut:
				return 124, errors.New("cell interrupted at its timeout; the session carries on")
			}
			return res.ExitCode, nil
		case len(args) == 2 && args[0] == "ls":
			list, err := c.ListSessions(ctx, args[1])
			if err != nil {
				return 1, err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tLANGUAGE\tPID\tCELLS")
			for _, s := range list {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\n", s.ID, s.Language, s.PID, s.Cells)
			}
			return 0, tw.Flush()
		case len(args) == 3 && args[0] == "rm":
			return 0, c.DeleteSession(ctx, args[1], args[2])
		default:
			return 2, errors.New("usage: cairn session new WORKSPACE [python|shell] | session run WORKSPACE SESSION [CODE] | session ls WORKSPACE | session rm WORKSPACE SESSION")
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

	case "pin", "unpin":
		if len(args) != 1 {
			return 2, fmt.Errorf("usage: cairn %s SAVE", cmd)
		}
		pin := c.PinSave
		if cmd == "unpin" {
			pin = c.UnpinSave
		}
		sv, err := pin(ctx, args[0])
		if err != nil {
			return 1, err
		}
		state := "unpinned"
		if sv.Pinned {
			state = "pinned"
		}
		fmt.Println(sv.ID, state)

	case "log":
		return runLog(ctx, c, args)

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

// interleaved parses flags that may come before, between or after the
// positional arguments, which it returns in order.
func interleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// stringList collects a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// defaultRemote is where `cairn push WS DIR` puts DIR: /root/<its name>, in
// the home folder of the workspace's user.
func defaultRemote(local string) (string, error) {
	abs, err := filepath.Abs(local)
	if err != nil {
		return "", err
	}
	return "/root/" + filepath.Base(abs), nil
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
