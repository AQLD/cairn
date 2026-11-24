// Package runtime is the boundary between the engine and whatever actually runs
// a workspace's processes.
//
// The engine owns each workspace's filesystem folder, its state and its saves.
// A Runtime only runs processes against that folder and, if it can, saves and
// restores memory. Two runtimes exist:
//
//   - runsc: gVisor, the production runtime (Linux only).
//   - host: runs commands directly on the host in the workspace folder. It has
//     no isolation and no memory saves; it exists so the API, state store, save
//     format and recovery can be built and tested on any machine.
//
// Tests and callers check Capabilities instead of the runtime's name, so a
// feature lights up everywhere as soon as a runtime supports it.
package runtime

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrUnsupported is returned for an operation the runtime cannot perform.
var ErrUnsupported = errors.New("not supported by this runtime")

// Capabilities says what a runtime can actually guarantee.
type Capabilities struct {
	// Isolation: workspaces cannot see or touch the host or each other.
	Isolation bool `json:"isolation"`
	// FullSave: memory and running processes can be saved and restored.
	FullSave bool `json:"full_save"`
	// ResourceLimits: CPU, memory and process-count limits are enforced.
	ResourceLimits bool `json:"resource_limits"`
	// DiskLimits: a workspace's disk usage is capped.
	DiskLimits bool `json:"disk_limits"`
	// NetworkControl: network access can be switched off and allow-listed.
	NetworkControl bool `json:"network_control"`
	// PersistentProcesses: processes keep running between commands.
	PersistentProcesses bool `json:"persistent_processes"`
	// SharedTemplates: workspaces made from one template share its files
	// read-only and store only their own changes (set by the engine, whatever
	// the runtime: it needs Linux, root and overlayfs).
	SharedTemplates bool `json:"shared_templates"`
}

// Limits are the resources a workspace may use. Zero means the engine default.
type Limits struct {
	CPUs     float64 `json:"cpus,omitempty"`
	MemoryMB int64   `json:"memory_mb,omitempty"`
	DiskMB   int64   `json:"disk_mb,omitempty"`
}

// Spec is everything a runtime needs to run a workspace.
type Spec struct {
	ID       string
	Hostname string
	RootDir  string // the workspace filesystem; owned by the engine
	Limits   Limits
	Network  bool
	// Egress, when set, limits a workspace with network on to these names
	// ("pypi.org", "*.github.com") and IPv4 ranges (see ParseEgress).
	Egress []string
}

// ExecRequest runs one command inside a workspace.
type ExecRequest struct {
	Argv    []string
	Env     map[string]string
	Dir     string // relative to the workspace root; "" is the root
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Timeout time.Duration
}

// ExecResult is how a command ended.
type ExecResult struct {
	ExitCode int  `json:"exit_code"`
	TimedOut bool `json:"timed_out"`
}

// Runtime runs workspaces. All methods must be safe for concurrent use on
// different workspaces.
type Runtime interface {
	Name() string
	Version() string
	Capabilities() Capabilities

	// Start makes a workspace ready to run commands.
	Start(ctx context.Context, spec Spec) error
	// Exec runs a command and returns when it exits or times out.
	Exec(ctx context.Context, spec Spec, req ExecRequest) (ExecResult, error)
	// Stop ends every process in the workspace. The filesystem stays.
	Stop(ctx context.Context, spec Spec) error

	// Checkpoint writes memory and process state into dir, then stops the
	// workspace. Only for runtimes with FullSave.
	Checkpoint(ctx context.Context, spec Spec, dir string) error
	// Restore starts a workspace from state written by Checkpoint.
	Restore(ctx context.Context, spec Spec, dir string) error
}

// Freezer is a FullSave runtime that can save a workspace's memory without
// stopping it, for full saves that keep the workspace running.
type Freezer interface {
	// CheckpointFrozen freezes every process where it is and writes memory
	// and process state into dir, as Checkpoint does, but leaves the
	// workspace frozen in place: nothing runs or touches its files until
	// Thaw. If it fails, the workspace is left running.
	CheckpointFrozen(ctx context.Context, spec Spec, dir string) error
	// Thaw lets a frozen workspace's processes carry on.
	Thaw(ctx context.Context, spec Spec) error
}
