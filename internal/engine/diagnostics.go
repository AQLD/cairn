package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	goruntime "runtime"
	"runtime/debug"
	"strings"
	"time"
)

// Diagnostics: what an operator sends when something is wrong, so the
// problem can be looked at without access to their network or data. It holds
// facts about the engine, the host and the state of things, and never what
// workspaces hold or do: no names, files, commands, outputs, environment,
// secrets or tokens. Every file is JSON, readable before it is sent.

// Diagnostics writes the bundle, a .tar.gz, to w.
func (e *Engine) Diagnostics(ctx context.Context, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	now := e.cfg.Now().UTC()
	add := func(name string, v any) error {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: "cairn-diagnostics/" + name, Mode: 0o644, Size: int64(len(data)), ModTime: now}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	}
	errText := func(err error) string {
		if err == nil {
			return "ok"
		}
		return err.Error()
	}

	if err := add("README.json", map[string]string{
		"about":    "Cairn diagnostics: engine, host and state facts. No workspace names, files, commands, outputs, environment, secrets or tokens.",
		"made_at":  now.Format(time.RFC3339),
		"contents": "engine.json host.json config.json counts.json workspaces.json saves.json activity.json checks.json",
	}); err != nil {
		return err
	}

	build := map[string]string{"go": goruntime.Version()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		build["module"] = bi.Main.Version
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" || s.Key == "vcs.time" || s.Key == "vcs.modified" {
				build[s.Key] = s.Value
			}
		}
	}
	if err := add("engine.json", map[string]any{"info": e.Info(), "build": build, "started_at": e.startedAt.UTC()}); err != nil {
		return err
	}

	host := map[string]any{"os": goruntime.GOOS, "arch": goruntime.GOARCH, "cpus": goruntime.NumCPU()}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		host["kernel"] = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) >= 2 && (f[0] == "MemTotal:" || f[0] == "MemAvailable:") {
				host[strings.TrimSuffix(f[0], ":")+"_kB"] = f[1]
			}
		}
	}
	if free, err := freeBytes(e.cfg.DataDir); err == nil {
		host["data_volume_free_bytes"] = free
	}
	if err := add("host.json", host); err != nil {
		return err
	}

	warm := map[string]int{}
	for t, n := range e.cfg.Warm {
		if t == "" {
			t = "(default)"
		}
		warm[t] = n
	}
	if err := add("config.json", map[string]any{
		"retention":            e.cfg.Retention.String(),
		"keep_saves":           e.cfg.KeepSaves,
		"keep_saves_for":       e.cfg.KeepSavesFor.String(),
		"min_free_bytes":       e.cfg.MinFreeBytes,
		"disk_reserve_bytes":   e.cfg.DiskReserve,
		"default_disk_mb":      e.cfg.DiskMB,
		"disk_images":          !e.cfg.NoDiskImages,
		"shared_templates":     e.cfg.SharedTemplates,
		"warm":                 warm,
		"max_commands":         e.cfg.MaxCommands,
		"max_ops":              e.cfg.MaxOps,
		"object_storage":       e.cfg.Objects != nil,
		"secrets_key":          e.cfg.SecretsKey != nil,
		"has_default_template": e.cfg.DefaultTemplate != "",
	}); err != nil {
		return err
	}

	counts, err := e.store.Counts(ctx)
	if err != nil {
		return err
	}
	if err := add("counts.json", counts); err != nil {
		return err
	}

	ws, err := e.store.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	usage := map[string]int64{}
	if st, err := e.Storage(ctx); err == nil {
		for _, u := range st.PerWorkspace {
			usage[u.ID] = u.Bytes
		}
		_ = add("storage.json", map[string]int64{"volume_free": st.VolumeFree, "volume_total": st.VolumeTotal,
			"workspaces": st.Workspaces, "saves": st.Saves, "templates": st.Templates})
	}
	type wsFacts struct {
		ID         string    `json:"id"`
		State      string    `json:"state"`
		Limits     any       `json:"limits"`
		Network    bool      `json:"network"`
		Egress     int       `json:"egress_entries"`
		Secrets    int       `json:"secrets"`
		DiskImage  bool      `json:"disk_image"`
		Template   bool      `json:"from_template"`
		Bytes      int64     `json:"bytes"`
		CreatedAt  time.Time `json:"created_at"`
		UpdatedAt  time.Time `json:"updated_at"`
		SourceSave string    `json:"source_save,omitempty"`
	}
	var facts []wsFacts
	for i := range ws {
		spec, _ := specOf(&ws[i])
		facts = append(facts, wsFacts{ID: ws[i].ID, State: string(ws[i].State), Limits: spec.Limits, Network: spec.Network,
			Egress: len(spec.Egress), Secrets: len(spec.Secrets), DiskImage: spec.DiskImage, Template: spec.Template != "",
			Bytes: usage[ws[i].ID], CreatedAt: ws[i].CreatedAt, UpdatedAt: ws[i].UpdatedAt, SourceSave: ws[i].SourceSave})
	}
	if err := add("workspaces.json", facts); err != nil {
		return err
	}

	saves, err := e.store.AllSaves(ctx)
	if err != nil {
		return err
	}
	type saveFacts struct {
		ID, Workspace, Kind, State string
		Size                       int64
		CreatedAt                  time.Time
		Auto, Uploaded, Pinned     bool
	}
	var sf []saveFacts
	for _, s := range saves {
		sf = append(sf, saveFacts{s.ID, s.WorkspaceID, s.Kind, string(s.State), s.Size, s.CreatedAt, s.Auto, s.Uploaded, s.Pinned})
	}
	if err := add("saves.json", sf); err != nil {
		return err
	}

	activity := map[string]any{}
	if e.events != nil {
		if c, err := e.events.CountsSince(ctx, now.Add(-24*time.Hour)); err == nil {
			activity["last_24h_by_kind"] = c
		}
		activity["dropped"] = e.events.Dropped()
	}
	if err := add("activity.json", activity); err != nil {
		return err
	}

	if err := add("checks.json", map[string]any{
		"state_store":     errText(e.store.Check(ctx)),
		"disk_images":     errText(e.diskErr),
		"disk_pressure":   e.pressured.Load(),
		"runtime":         e.cfg.Runtime.Name(),
		"runtime_version": e.Info().RuntimeVersion,
	}); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
