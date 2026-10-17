// Package drydock implements the preview-VM engine and its two frontends.
//
// The API is the Driver interface, declared once here. Engine is the single
// implementation; the local driver calls it directly and the remote driver
// reaches a copy of it through the portal. There is no second definition of the
// operations and no argv protocol between the pieces.
package drydock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// InputOverride overrides an input of the user's config flake, matching the
// name they would pass to nixos-rebuild (e.g. "my-nix").
type InputOverride struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

// Request is the union of options accepted by every operation. It is the single
// wire representation shared by the local and remote drivers.
type Request struct {
	Name           string          `json:"name,omitempty"`
	Flake          string          `json:"flake,omitempty"`
	Host           string          `json:"host,omitempty"`
	Mem            string          `json:"mem,omitempty"`
	CPUs           string          `json:"cpus,omitempty"`
	Disk           string          `json:"disk,omitempty"`
	DisplayAddr    string          `json:"display_addr,omitempty"`
	Headless       bool            `json:"headless,omitempty"`
	Persist        bool            `json:"persist,omitempty"`
	Fresh          bool            `json:"fresh,omitempty"`
	DryRun         bool            `json:"dry_run,omitempty"`
	ClearOverrides bool            `json:"clear_overrides,omitempty"`
	JSON           bool            `json:"json,omitempty"`
	RunningOnly    bool            `json:"running_only,omitempty"`
	Args           []string        `json:"args,omitempty"`
	Overrides      []InputOverride `json:"overrides,omitempty"`
	NixArgs        []string        `json:"nix_args,omitempty"`
}

// Status describes a VM instance.
type Status struct {
	Name          string          `json:"name"`
	State         string          `json:"state"`
	Flake         string          `json:"flake"`
	Host          string          `json:"host"`
	Dir           string          `json:"dir"`
	VNCPort       string          `json:"vnc_port"`
	WebPort       string          `json:"web_port"`
	SSHPort       string          `json:"ssh_port"`
	SSHKey        string          `json:"ssh_key,omitempty"`
	URL           string          `json:"url"`
	Created       string          `json:"created,omitempty"`
	Mem           int             `json:"mem_mib,omitempty"`
	CPUs          int             `json:"cpus,omitempty"`
	Disk          int             `json:"disk_mib,omitempty"`
	Overrides     []InputOverride `json:"overrides,omitempty"`
	NixArgs       []string        `json:"nix_args,omitempty"`
	DiskBytes     int64           `json:"disk_bytes,omitempty"`
	DiskUsedBytes int64           `json:"disk_used_bytes,omitempty"`
	RSSBytes      int64           `json:"rss_bytes,omitempty"`
}

// VMInfo is the compact form used by List.
type VMInfo struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Config  string `json:"config,omitempty"`
	Created string `json:"created,omitempty"`
	URL     string `json:"url,omitempty"`
}

// Event is one streamed message. Streaming operations emit stdout/stderr lines
// and finish with a result or an error.
type Event struct {
	Type string          `json:"type"`
	Data string          `json:"data,omitempty"`
	Code *int            `json:"code,omitempty"`
	Raw  json.RawMessage `json:"raw,omitempty"`
}

// Sink receives streamed events from an operation.
type Sink interface {
	Emit(Event) error
}

// Driver is the entire drydock operation surface. It is implemented once by
// Engine and reached directly (local) or over HTTP (remote).
type Driver interface {
	Up(context.Context, Request, Sink) error
	Apply(context.Context, Request, Sink) error
	Reset(context.Context, Request, Sink) error
	Down(context.Context, Request, Sink) error
	Status(context.Context, Request) (Status, error)
	URL(context.Context, Request) (string, error)
	Screenshot(context.Context, Request) ([]byte, error)
	SSH(context.Context, Request, Sink) error
	Logs(context.Context, Request, Sink) error
	List(context.Context) ([]VMInfo, error)
	Remove(context.Context, Request, Sink) error
	Prune(context.Context, Request, Sink) error
	Doctor(context.Context, Sink) error
}

// meta is the persisted per-VM state.
type meta struct {
	Name        string          `json:"name"`
	Flake       string          `json:"flake"`
	Host        string          `json:"host"`
	VNCPort     int             `json:"vnc_port"`
	WebPort     int             `json:"web_port"`
	SSHPort     int             `json:"ssh_port"`
	DisplayAddr string          `json:"display_addr"`
	Headless    bool            `json:"headless"`
	Created     string          `json:"created"`
	Mem         int             `json:"mem,omitempty"`
	CPUs        int             `json:"cpus,omitempty"`
	Disk        int             `json:"disk,omitempty"`
	Overrides   []InputOverride `json:"overrides,omitempty"`
	NixArgs     []string        `json:"nix_args,omitempty"`
}

func (e *Engine) vmDir(name string) string {
	return filepath.Join(e.StateRoot, "vms", name)
}

func (e *Engine) loadMeta(name string) (*meta, error) {
	data, err := os.ReadFile(filepath.Join(e.vmDir(name), "meta.json"))
	if err != nil {
		return nil, err
	}
	var m meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (e *Engine) saveMeta(m *meta) error {
	dir := e.vmDir(m.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), append(data, '\n'), 0o644)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// NormalizeRef turns a filesystem path into an explicit flake reference. Flake
// refs are returned unchanged.
func NormalizeRef(ref string) string {
	if strings.HasPrefix(ref, ".") || strings.HasPrefix(ref, "/") {
		if abs, err := filepath.Abs(ref); err == nil {
			return "path:" + abs
		}
	}
	return ref
}
