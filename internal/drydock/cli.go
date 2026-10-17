package drydock

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
)

// Version is the drydock release string.
const Version = "0.2.0"

const usageText = `drydock — preview OS configuration changes in a NixOS VM

The guest is built from your own flake's nixosConfiguration, so what you see
is what you would get. The host system is never modified.

Usage:
  drydock <command> [flags]

Commands:
  up            Build and boot the preview VM (idempotent)
  apply         Rebuild the current config and live-switch the running VM
  reset         Destroy the VM disk and boot it fresh
  down          Stop the VM and its display
  rm            Stop and delete a VM's state
  prune         Delete every stopped VM's state
  status        Show VM state, ports and URLs
  url           Print the browser (noVNC) URL
  screenshot    Capture the guest display to a PNG
  ssh           Open a shell in the guest (or run: drydock ssh -- CMD)
  logs          Show the VM console log
  list          List known VMs
  doctor        Check host prerequisites

Flags:
  -n, --name NAME          VM instance name (default: default)
  -f, --flake REF          Config flake (default: $DRYDOCK_FLAKE or the current dir)
  -H, --host HOST          nixosConfigurations attribute to preview
  -P, --portal URL         Drive a drydock portal on another machine
                           (also $DRYDOCK_PORTAL; defaults to local execution)
      --mem MiB            Guest memory
      --cpus N             Guest vCPUs
      --disk MiB           Guest disk size
      --display-address A  Address to bind the noVNC display to (default: 127.0.0.1)
      --override-input NAME REF
                           Override an input of the config flake (repeatable);
                           REF may be a path, URL or flake ref
      --nix-arg ARG        Extra argument passed to nix-build (repeatable)
      --headless           Do not start VNC/noVNC (SSH-only)
      --fresh              Recreate the disk before booting
      --dry-run            Build only; do not boot or switch
      --clear-overrides    Ignore overrides saved for this VM
      --persist            Apply with switch-to-configuration "switch"
  -j, --json               Machine-readable status output
      --running            Only list running VMs
  -h, --help               Show this help
`

// RunCLI parses arguments and executes one drydock command against either a
// local engine or a remote portal. It returns a process exit code.
func RunCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stderr, usageText)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	command := args[0]

	var req Request
	var portal, token string

	overrides, nixArgs, rest, err := splitPassthrough(args[1:])
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 2
	}

	fs := flag.NewFlagSet("drydock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usageText) }
	fs.StringVar(&req.Name, "name", envOr("DRYDOCK_NAME", "default"), "VM instance name")
	fs.StringVar(&req.Name, "n", envOr("DRYDOCK_NAME", "default"), "VM instance name")
	fs.StringVar(&req.Flake, "flake", os.Getenv("DRYDOCK_FLAKE"), "config flake")
	fs.StringVar(&req.Flake, "f", os.Getenv("DRYDOCK_FLAKE"), "config flake")
	fs.StringVar(&req.Host, "host", os.Getenv("DRYDOCK_HOST"), "nixosConfiguration")
	fs.StringVar(&req.Host, "H", os.Getenv("DRYDOCK_HOST"), "nixosConfiguration")
	fs.StringVar(&portal, "portal", os.Getenv("DRYDOCK_PORTAL"), "portal URL")
	fs.StringVar(&portal, "P", os.Getenv("DRYDOCK_PORTAL"), "portal URL")
	fs.StringVar(&token, "token", os.Getenv("DRYDOCK_PORTAL_TOKEN"), "portal bearer token")
	fs.StringVar(&req.Mem, "mem", "", "guest memory MiB")
	fs.StringVar(&req.CPUs, "cpus", "", "guest vCPUs")
	fs.StringVar(&req.Disk, "disk", "", "guest disk MiB")
	fs.StringVar(&req.DisplayAddr, "display-address", envOr("DRYDOCK_DISPLAY_ADDRESS", "127.0.0.1"), "noVNC bind address")
	fs.BoolVar(&req.Headless, "headless", false, "SSH-only, no display")
	fs.BoolVar(&req.Fresh, "fresh", false, "recreate the disk")
	fs.BoolVar(&req.DryRun, "dry-run", false, "build without booting or switching")
	fs.BoolVar(&req.ClearOverrides, "clear-overrides", false, "ignore overrides saved for this VM")
	fs.BoolVar(&req.Persist, "persist", false, "persist the switch")
	fs.BoolVar(&req.JSON, "json", false, "machine-readable status")
	fs.BoolVar(&req.JSON, "j", false, "machine-readable status")
	fs.BoolVar(&req.RunningOnly, "running", false, "only list running VMs")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	req.Args = fs.Args()
	req.Flake = NormalizeRef(req.Flake)
	req.Overrides = overrides
	req.NixArgs = nixArgs

	var driver Driver = NewEngine()
	if portal != "" {
		driver = NewRemote(portal, token)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sink := writerSink{out: stdout, err: stderr}

	switch command {
	case "up":
		err = driver.Up(ctx, req, sink)
	case "apply":
		err = driver.Apply(ctx, req, sink)
	case "reset":
		err = driver.Reset(ctx, req, sink)
	case "down":
		err = driver.Down(ctx, req, sink)
	case "rm", "remove":
		err = driver.Remove(ctx, req, sink)
	case "prune":
		err = driver.Prune(ctx, req, sink)
	case "status":
		err = runStatus(ctx, driver, req, stdout)
	case "url":
		err = runURL(ctx, driver, req, stdout)
	case "screenshot":
		err = runScreenshot(ctx, driver, req, stdout)
	case "ssh":
		err = driver.SSH(ctx, req, sink)
	case "logs":
		err = driver.Logs(ctx, req, sink)
	case "list":
		err = runList(ctx, driver, req, stdout)
	case "doctor":
		err = driver.Doctor(ctx, sink)
	default:
		fmt.Fprintf(stderr, "error: unknown command %q\n\n", command)
		fmt.Fprint(stderr, usageText)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 1
	}
	return 0
}

// splitPassthrough pulls --override-input and --nix-arg out of the argument
// list before flag parsing, because they take two values (or use NAME=REF).
func splitPassthrough(args []string) (overrides []InputOverride, nixArgs, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			rest = append(rest, args[i:]...)
			return
		case arg == "--override-input":
			if i+2 >= len(args) {
				return nil, nil, nil, fmt.Errorf("--override-input needs NAME and REF")
			}
			overrides = append(overrides, InputOverride{Name: args[i+1], Ref: NormalizeRef(args[i+2])})
			i += 2
		case strings.HasPrefix(arg, "--override-input="):
			spec := strings.TrimPrefix(arg, "--override-input=")
			name, ref, ok := strings.Cut(spec, "=")
			if !ok {
				return nil, nil, nil, fmt.Errorf("--override-input expects NAME=REF")
			}
			overrides = append(overrides, InputOverride{Name: name, Ref: NormalizeRef(ref)})
		case arg == "--nix-arg":
			if i+1 >= len(args) {
				return nil, nil, nil, fmt.Errorf("--nix-arg needs a value")
			}
			nixArgs = append(nixArgs, args[i+1])
			i++
		case strings.HasPrefix(arg, "--nix-arg="):
			nixArgs = append(nixArgs, strings.TrimPrefix(arg, "--nix-arg="))
		default:
			rest = append(rest, arg)
		}
	}
	return
}

func runStatus(ctx context.Context, driver Driver, req Request, stdout io.Writer) error {
	status, err := driver.Status(ctx, req)
	if err != nil {
		return err
	}
	if req.JSON {
		data, _ := json.MarshalIndent(status, "", "  ")
		fmt.Fprintln(stdout, string(data))
		return nil
	}
	fmt.Fprintln(stdout, FormatStatus(status))
	return nil
}

func runURL(ctx context.Context, driver Driver, req Request, stdout io.Writer) error {
	url, err := driver.URL(ctx, req)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, url)
	return nil
}

func runScreenshot(ctx context.Context, driver Driver, req Request, stdout io.Writer) error {
	out := "drydock-screenshot.png"
	if len(req.Args) > 0 {
		out = req.Args[0]
	}
	data, err := driver.Screenshot(ctx, req)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(stdout, out)
	return nil
}

func runList(ctx context.Context, driver Driver, req Request, stdout io.Writer) error {
	vms, err := driver.List(ctx)
	if err != nil {
		return err
	}
	if req.RunningOnly {
		filtered := vms[:0]
		for _, vm := range vms {
			if vm.State == "running" {
				filtered = append(filtered, vm)
			}
		}
		vms = filtered
	}
	if req.JSON {
		data, _ := json.MarshalIndent(vms, "", "  ")
		fmt.Fprintln(stdout, string(data))
		return nil
	}
	if len(vms) == 0 {
		fmt.Fprintln(stdout, "(no VMs)")
		return nil
	}
	w := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tCONFIG\tDISPLAY")
	for _, vm := range vms {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", vm.Name, vm.State, orDash(vm.Config), orDash(vm.URL))
	}
	return w.Flush()
}

// FormatStatus renders a Status for humans.
func FormatStatus(status Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "VM:        %s (%s)\n", status.Name, status.State)
	fmt.Fprintf(&b, "config:    %s#%s\n", status.Flake, status.Host)
	if len(status.Overrides) > 0 {
		parts := make([]string, 0, len(status.Overrides))
		for _, override := range status.Overrides {
			parts = append(parts, override.Name+"="+override.Ref)
		}
		fmt.Fprintf(&b, "overrides: %s\n", strings.Join(parts, ", "))
	}
	if status.Mem > 0 || status.CPUs > 0 || status.Disk > 0 {
		fmt.Fprintf(&b, "resources: %s RAM, %d vCPU, %s disk\n",
			humanMiB(status.Mem), status.CPUs, humanMiB(status.Disk))
	}
	if status.SSHKey != "" {
		fmt.Fprintf(&b, "ssh:       ssh -i %s -p %s root@127.0.0.1\n", status.SSHKey, status.SSHPort)
	}
	if status.URL != "" {
		fmt.Fprintf(&b, "display:   %s\n", status.URL)
	} else {
		fmt.Fprintln(&b, "display:   (headless)")
	}
	if status.DiskUsedBytes > 0 {
		fmt.Fprintf(&b, "disk:      %s used (%s virtual)\n", humanBytes(status.DiskUsedBytes), humanMiB(status.Disk))
	}
	if status.RSSBytes > 0 {
		fmt.Fprintf(&b, "memory:    %s RSS\n", humanBytes(status.RSSBytes))
	}
	if status.Created != "" {
		fmt.Fprintf(&b, "created:   %s\n", status.Created)
	}
	return strings.TrimRight(b.String(), "\n")
}

func humanMiB(mib int) string {
	if mib >= 1024 {
		return fmt.Sprintf("%.1f GiB", float64(mib)/1024)
	}
	return fmt.Sprintf("%d MiB", mib)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// writerSink prints engine events to the CLI's streams.
type writerSink struct {
	out io.Writer
	err io.Writer
}

func (w writerSink) Emit(event Event) error {
	if event.Type == "stderr" {
		_, err := fmt.Fprintln(w.err, event.Data)
		return err
	}
	_, err := fmt.Fprintln(w.out, event.Data)
	return err
}
