package drydock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Names and addresses come from an untrusted client, so they are strictly
// validated before being used in paths, systemd unit names, or arguments.
var (
	nameRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	inputNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)
	hostRE      = regexp.MustCompile(`^[A-Za-z0-9\[\]][A-Za-z0-9.:_\[\]-]*$`)
)

// Engine holds the paths an instance needs. Zero value is not usable; use
// NewEngine.
type Engine struct {
	StateRoot string
	NixDir    string

	// Defaults supplied by a host portal. Resolution order for every field is
	// explicit request > saved VM metadata > these defaults > environment.
	DefaultFlake     string
	DefaultHost      string
	DefaultOverrides []InputOverride
}

// NewEngine builds an Engine from the environment.
func NewEngine() *Engine {
	root := os.Getenv("DRYDOCK_STATE")
	if root == "" {
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			root = filepath.Join(xdg, "drydock")
		} else if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, ".local", "state", "drydock")
		} else {
			root = "/tmp/drydock"
		}
	}
	return &Engine{StateRoot: root, NixDir: os.Getenv("DRYDOCK_NIX_DIR")}
}

type resolved struct {
	req   Request
	vmDir string
}

// resolve merges flags, saved metadata, and environment into a usable request.
func (e *Engine) resolve(req Request) (resolved, error) {
	if req.Name == "" {
		req.Name = envOr("DRYDOCK_NAME", "default")
	}
	if !nameRE.MatchString(req.Name) {
		return resolved{}, fmt.Errorf("invalid VM name %q", req.Name)
	}
	saved, _ := e.loadMeta(req.Name)
	if saved == nil {
		saved = &meta{Name: req.Name}
	}
	if req.Flake == "" {
		req.Flake = saved.Flake
	}
	if req.Flake == "" {
		req.Flake = e.DefaultFlake
	}
	if req.Host == "" {
		req.Host = saved.Host
	}
	if req.Host == "" {
		req.Host = e.DefaultHost
	}
	if req.DisplayAddr == "" {
		req.DisplayAddr = saved.DisplayAddr
	}
	if req.ClearOverrides {
		req.Overrides = nil
	} else if len(req.Overrides) == 0 {
		if len(saved.Overrides) > 0 {
			req.Overrides = saved.Overrides
		} else {
			req.Overrides = e.DefaultOverrides
		}
	}
	if len(req.NixArgs) == 0 {
		req.NixArgs = saved.NixArgs
	}
	if req.Flake == "" {
		req.Flake = os.Getenv("DRYDOCK_FLAKE")
	}
	if req.Flake == "" {
		req.Flake = "."
	}
	req.Flake = NormalizeRef(req.Flake)
	if req.Host == "" {
		req.Host = os.Getenv("DRYDOCK_HOST")
	}
	if req.Host == "" {
		return resolved{}, fmt.Errorf("no --host given and none saved for %q", req.Name)
	}
	if req.DisplayAddr == "" {
		req.DisplayAddr = envOr("DRYDOCK_DISPLAY_ADDRESS", "127.0.0.1")
	}
	if !hostRE.MatchString(req.DisplayAddr) {
		return resolved{}, fmt.Errorf("invalid display address %q", req.DisplayAddr)
	}
	for _, override := range req.Overrides {
		if !inputNameRE.MatchString(override.Name) {
			return resolved{}, fmt.Errorf("invalid override input name %q", override.Name)
		}
	}
	return resolved{req: req, vmDir: e.vmDir(req.Name)}, nil
}

// pid returns the QEMU pid and whether it is alive.
func (e *Engine) pid(name string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(e.vmDir(name), "qemu.pid"))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return pid, false
	}
	return pid, true
}

func (e *Engine) keyPath(name string) string {
	return filepath.Join(e.vmDir(name), "id_ed25519")
}

// ensureKey creates the guest SSH keypair if it does not exist.
func (e *Engine) ensureKey(name string) (string, error) {
	key := e.keyPath(name)
	if _, err := os.Stat(key); err == nil {
		return key, nil
	}
	if err := os.MkdirAll(e.vmDir(name), 0o755); err != nil {
		return "", err
	}
	cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "drydock", "-f", key)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ssh-keygen: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return key, nil
}

// pickPorts derives stable-ish ports from the VM name and nudges on collision.
func (e *Engine) pickPorts(name string) (vnc, web, ssh int, err error) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	seed := int(h.Sum32())
	if vnc, err = freePort(5900 + seed%200); err != nil {
		return
	}
	if web, err = freePort(6080 + seed%200); err != nil {
		return
	}
	if ssh, err = freePort(2200 + seed%600); err != nil {
		return
	}
	return
}

func freePort(start int) (int, error) {
	for port := start; port < start+1000; port++ {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = listener.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free port near %d", start)
}

// buildAttr builds one of the wrapper flake's outputs. The wrapper loads the
// user's flake as an input and extends their nixosConfiguration with the guest
// module, so `nix build` (with input overrides) does the work. Evaluation is
// pure and the Nix sandbox is forced on, so a client-controlled config cannot
// read host files or weaken the build sandbox.
func (e *Engine) buildAttr(ctx context.Context, req Request, attr, link string, sink Sink) (string, error) {
	evalDir := filepath.Join(e.vmDir(req.Name), "eval")
	if err := e.writeWrapper(evalDir, req); err != nil {
		return "", err
	}
	args := []string{
		"build", "--no-write-lock-file",
		"--option", "sandbox", "true",
		"-o", link,
		"path:" + evalDir + "#drydock." + attr,
		"--override-input", "config", req.Flake,
	}
	for _, override := range req.Overrides {
		args = append(args, "--override-input", "config/"+override.Name, override.Ref)
	}
	args = append(args, req.NixArgs...)

	_ = os.MkdirAll(filepath.Dir(link), 0o755)
	cmd := exec.CommandContext(ctx, "nix", args...)
	if err := streamExec(ctx, sink, cmd); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(link)
}

// writeWrapper generates the per-VM flake that pins the user's config as an
// input and exposes the VM and system derivations with the guest module
// applied. Guest parameters are passed as options, never via the environment.
func (e *Engine) writeWrapper(dir string, req Request) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(dir, "flake.lock"))
	pubkey, err := os.ReadFile(e.keyPath(req.Name) + ".pub")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "id_ed25519.pub"), pubkey, 0o644); err != nil {
		return err
	}
	// The guest module must live inside the flake: pure evaluation forbids
	// referencing absolute store paths from the wrapper.
	if err := copyTree(filepath.Join(e.NixDir, "guest"), filepath.Join(dir, "guest")); err != nil {
		return err
	}
	contents := fmt.Sprintf(`{
  inputs.config.url = %s;

  outputs = { self, config, ... }:
    let
      base = builtins.getAttr %s config.nixosConfigurations;
      preview = base.extendModules {
        modules = [
          ./guest
          {
            drydock = {
              sshPubkey = builtins.readFile ./id_ed25519.pub;
              mem = %s;
              cpus = %s;
              disk = %s;
            };
          }
        ];
      };
    in {
      drydock = {
        vm = preview.config.system.build.vm;
        system = preview.config.virtualisation.vmVariant.system.build.toplevel;
      };
    };
}
`, nixQuote(req.Flake), nixQuote(req.Host), nixInt(req.Mem), nixInt(req.CPUs), nixInt(req.Disk))
	return os.WriteFile(filepath.Join(dir, "flake.nix"), []byte(contents), 0o644)
}

// copyTree copies a small directory tree (the guest module) into the wrapper
// flake. It is not meant for large or symlinked trees.
func copyTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyTree(from, to); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if err := os.WriteFile(to, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func nixInt(value string) string {
	if value == "" {
		return "null"
	}
	if n, err := strconv.Atoi(value); err == nil {
		return strconv.Itoa(n)
	}
	return "null"
}

func nixQuote(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "${", "\\${")
	return "\"" + replacer.Replace(value) + "\""
}

// Up builds and boots the VM (idempotent).
func (e *Engine) Up(ctx context.Context, req Request, sink Sink) error {
	r, err := e.resolve(req)
	if err != nil {
		return err
	}
	key, err := e.ensureKey(r.req.Name)
	if err != nil {
		return err
	}
	if !req.DryRun {
		if _, running := e.pid(r.req.Name); running {
			existing, loadErr := e.loadMeta(r.req.Name)
			if loadErr == nil && e.probeSSH(ctx, existing, key, 15*time.Second) {
				_ = sink.Emit(Event{Type: "stdout", Data: fmt.Sprintf("VM '%s' is already running", r.req.Name)})
				if !existing.Headless {
					if err := e.startDisplay(existing); err != nil {
						_ = sink.Emit(Event{Type: "stderr", Data: "warning: " + err.Error()})
					}
				}
				return e.emitStatus(sink, r)
			}
			_ = sink.Emit(Event{Type: "stdout", Data: fmt.Sprintf("VM '%s' is running but unreachable; restarting", r.req.Name)})
			if err := e.Down(ctx, r.req, sink); err != nil {
				return err
			}
		}
	}
	if req.Fresh {
		_ = os.Remove(filepath.Join(r.vmDir, "disk.qcow2"))
	}
	vnc, web, sshPort, err := e.pickPorts(r.req.Name)
	if err != nil {
		return err
	}
	m := &meta{
		Name: r.req.Name, Flake: r.req.Flake, Host: r.req.Host,
		VNCPort: vnc, WebPort: web, SSHPort: sshPort,
		DisplayAddr: r.req.DisplayAddr, Headless: req.Headless,
		Created:   time.Now().Format(time.RFC3339),
		Mem:       atoiOr0(r.req.Mem),
		CPUs:      atoiOr0(r.req.CPUs),
		Disk:      atoiOr0(r.req.Disk),
		Overrides: r.req.Overrides, NixArgs: r.req.NixArgs,
	}
	if err := e.saveMeta(m); err != nil {
		return err
	}

	_ = sink.Emit(Event{Type: "stdout", Data: "building the VM (this can take a while the first time)"})
	runner, err := e.buildAttr(ctx, r.req, "vm", filepath.Join(r.vmDir, "vm"), sink)
	if err != nil {
		return err
	}
	if req.DryRun {
		_ = sink.Emit(Event{Type: "stdout", Data: "built " + runner})
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(runner, "bin", "run-*-vm"))
	if len(matches) == 0 {
		return fmt.Errorf("could not find run-*-vm under %s/bin", runner)
	}

	console := filepath.Join(r.vmDir, "console.log")
	pidFile := filepath.Join(r.vmDir, "qemu.pid")
	_ = os.Remove(pidFile)
	qemuOpts := fmt.Sprintf("-display none -qmp unix:%s,server,nowait -daemonize -pidfile %s",
		filepath.Join(r.vmDir, "qmp.sock"), pidFile)
	if !m.Headless {
		// QEMU's -vnc takes a display number; the TCP port is 5900 + display.
		qemuOpts = fmt.Sprintf("-vnc 127.0.0.1:%d %s", m.VNCPort-5900, qemuOpts)
	}
	_ = sink.Emit(Event{Type: "stdout", Data: fmt.Sprintf(
		"booting VM '%s' (ssh :%d, vnc :%d)", m.Name, m.SSHPort, m.VNCPort)})

	diskImage := filepath.Join(r.vmDir, "disk.qcow2")
	if _, err := os.Stat(diskImage); os.IsNotExist(err) {
		_ = sink.Emit(Event{Type: "stdout", Data: "creating the guest disk (first boot; this can take a moment)"})
	}
	env := append(os.Environ(),
		"NIX_DISK_IMAGE="+diskImage,
		"QEMU_OPTS="+qemuOpts,
		"QEMU_NET_OPTS="+fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:22", m.SSHPort),
	)
	if err := runToFile(ctx, console, env, matches[0]); err != nil {
		_ = sink.Emit(Event{Type: "stderr", Data: tailLines(console, 40)})
		return fmt.Errorf("the VM failed to start; full log: %s", console)
	}
	if _, running := e.pid(m.Name); !running {
		return fmt.Errorf("the VM did not start; see %s", console)
	}
	_ = sink.Emit(Event{Type: "stdout", Data: "waiting for the guest to accept SSH"})
	if err := e.waitSSH(ctx, r, m, key); err != nil {
		return err
	}
	_ = sink.Emit(Event{Type: "stdout", Data: "guest is ready"})
	if !m.Headless {
		if err := e.startDisplay(m); err != nil {
			return err
		}
	}
	return e.emitStatus(sink, r)
}

// Apply rebuilds the current config and live-switches the running guest.
func (e *Engine) Apply(ctx context.Context, req Request, sink Sink) error {
	r, err := e.resolve(req)
	if err != nil {
		return err
	}
	key, err := e.ensureKey(r.req.Name)
	if err != nil {
		return err
	}
	m, err := e.loadMeta(r.req.Name)
	if err != nil {
		return err
	}
	if _, running := e.pid(r.req.Name); !running {
		return fmt.Errorf("VM '%s' is not running; run 'drydock up' first", r.req.Name)
	}
	_ = sink.Emit(Event{Type: "stdout", Data: "building the current configuration"})
	top, err := e.buildAttr(ctx, r.req, "system", filepath.Join(r.vmDir, "system"), sink)
	if err != nil {
		return err
	}
	if req.DryRun {
		_ = sink.Emit(Event{Type: "stdout", Data: "built " + top})
		return nil
	}
	action := "test"
	if req.Persist {
		action = "switch"
	}
	_ = sink.Emit(Event{Type: "stdout", Data: "live-switching the guest to " + top})
	remote := fmt.Sprintf("export PATH=/run/current-system/sw/bin:$PATH; %s/bin/switch-to-configuration %s", top, action)
	if err := streamExec(ctx, sink, e.sshCommand(ctx, m, key, true, remote)); err != nil {
		return fmt.Errorf("switch-to-configuration failed: %w", err)
	}
	_ = sink.Emit(Event{Type: "stdout", Data: "applied"})
	if !m.Headless {
		return e.emitStatus(sink, r)
	}
	return nil
}

// Reset destroys the disk and boots fresh.
func (e *Engine) Reset(ctx context.Context, req Request, sink Sink) error {
	if err := e.Down(ctx, req, sink); err != nil {
		return err
	}
	r, err := e.resolve(req)
	if err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(r.vmDir, "disk.qcow2"))
	_ = sink.Emit(Event{Type: "stdout", Data: "disk removed; booting fresh"})
	return e.Up(ctx, req, sink)
}

// Down stops the VM and its display.
func (e *Engine) Down(ctx context.Context, req Request, sink Sink) error {
	r, err := e.resolve(req)
	if err != nil {
		return err
	}
	if pid, running := e.pid(r.req.Name); running {
		_ = sink.Emit(Event{Type: "stdout", Data: fmt.Sprintf("stopping VM '%s' (pid %d)", r.req.Name, pid)})
		_ = syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; i < 20; i++ {
			if _, alive := e.pid(r.req.Name); !alive {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	e.stopDisplay(r.req.Name)
	_ = os.Remove(filepath.Join(r.vmDir, "qemu.pid"))
	_ = os.Remove(filepath.Join(r.vmDir, "qmp.sock"))
	return nil
}

// Status reports a single VM.
func (e *Engine) Status(ctx context.Context, req Request) (Status, error) {
	r, err := e.resolve(req)
	if err != nil {
		return Status{}, err
	}
	return e.statusFor(r)
}

// URL returns the browser display URL.
func (e *Engine) URL(ctx context.Context, req Request) (string, error) {
	r, err := e.resolve(req)
	if err != nil {
		return "", err
	}
	st, err := e.statusFor(r)
	if err != nil {
		return "", err
	}
	if st.URL == "" {
		return "", fmt.Errorf("VM '%s' is headless", st.Name)
	}
	return st.URL, nil
}

// SSH opens a shell or runs a command in the guest. An interactive shell
// (no Args) attaches to the current process stdio and is only sensible when
// drydock runs locally.
func (e *Engine) SSH(ctx context.Context, req Request, sink Sink) error {
	r, err := e.resolve(req)
	if err != nil {
		return err
	}
	key, err := e.ensureKey(r.req.Name)
	if err != nil {
		return err
	}
	m, err := e.loadMeta(r.req.Name)
	if err != nil {
		return err
	}
	if _, running := e.pid(r.req.Name); !running {
		return fmt.Errorf("VM '%s' is not running", r.req.Name)
	}
	if len(req.Args) == 0 {
		cmd := e.sshCommand(ctx, m, key, false)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return streamExec(ctx, sink, e.sshCommand(ctx, m, key, true, req.Args...))
}

// Logs follows the guest console output.
func (e *Engine) Logs(ctx context.Context, req Request, sink Sink) error {
	r, err := e.resolve(req)
	if err != nil {
		return err
	}
	console := filepath.Join(r.vmDir, "console.log")
	if _, err := os.Stat(console); err != nil {
		return fmt.Errorf("no console log yet for '%s'", r.req.Name)
	}
	return streamExec(ctx, sink, exec.CommandContext(ctx, "tail", "-n", "200", "-f", console))
}

// List enumerates known VMs.
func (e *Engine) List(ctx context.Context) ([]VMInfo, error) {
	entries, err := os.ReadDir(filepath.Join(e.StateRoot, "vms"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []VMInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		info := VMInfo{Name: name, State: "stopped"}
		if _, running := e.pid(name); running {
			info.State = "running"
		}
		if m, err := e.loadMeta(name); err == nil {
			info.Config = m.Flake + "#" + m.Host
			info.Created = m.Created
			if !m.Headless {
				info.URL = fmt.Sprintf("http://%s:%d/vnc.html?autoconnect=1&resize=scale", m.DisplayAddr, m.WebPort)
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// Remove stops (if needed) and deletes a VM's state.
func (e *Engine) Remove(ctx context.Context, req Request, sink Sink) error {
	name := req.Name
	if name == "" {
		name = envOr("DRYDOCK_NAME", "default")
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid VM name %q", name)
	}
	dir := e.vmDir(name)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("no VM named %q", name)
	}
	if _, running := e.pid(name); running {
		req.Name = name
		_ = sink.Emit(Event{Type: "stdout", Data: fmt.Sprintf("stopping VM '%s' before removing", name)})
		if err := e.Down(ctx, req, sink); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return sink.Emit(Event{Type: "stdout", Data: fmt.Sprintf("removed VM '%s'", name)})
}

// Prune removes every stopped VM's state and leaves running ones alone.
func (e *Engine) Prune(ctx context.Context, req Request, sink Sink) error {
	infos, err := e.List(ctx)
	if err != nil {
		return err
	}
	removed := 0
	for _, info := range infos {
		if info.State == "running" {
			_ = sink.Emit(Event{Type: "stdout", Data: fmt.Sprintf("skipping running VM '%s'", info.Name)})
			continue
		}
		if err := os.RemoveAll(e.vmDir(info.Name)); err != nil {
			return err
		}
		removed++
		_ = sink.Emit(Event{Type: "stdout", Data: fmt.Sprintf("removed VM '%s'", info.Name)})
	}
	return sink.Emit(Event{Type: "stdout", Data: fmt.Sprintf("pruned %d VM(s)", removed)})
}

// Doctor checks host prerequisites.
func (e *Engine) Doctor(ctx context.Context, sink Sink) error {
	checks := []struct {
		name  string
		check func() error
	}{
		{"/dev/kvm is readable and writable", func() error { return access("/dev/kvm") }},
		{"nix is on PATH", func() error { return lookPath("nix") }},
		{"novnc is on PATH", func() error { return lookPath("novnc") }},
		{"ssh-keygen is on PATH", func() error { return lookPath("ssh-keygen") }},
		{"ssh is on PATH", func() error { return lookPath("ssh") }},
		{"user systemd is running", func() error {
			return exec.Command("systemctl", "--user", "is-system-running").Run()
		}},
	}
	failed := false
	for _, c := range checks {
		if err := c.check(); err != nil {
			failed = true
			_ = sink.Emit(Event{Type: "stdout", Data: "  [fail] " + c.name})
		} else {
			_ = sink.Emit(Event{Type: "stdout", Data: "  [ok]   " + c.name})
		}
	}
	if failed {
		return fmt.Errorf("one or more prerequisites failed")
	}
	return nil
}

// --- shared helpers -------------------------------------------------------

func (e *Engine) statusFor(r resolved) (Status, error) {
	m, err := e.loadMeta(r.req.Name)
	if err != nil {
		return Status{}, fmt.Errorf("no VM named %q", r.req.Name)
	}
	st := Status{
		Name: m.Name, State: "stopped", Flake: m.Flake, Host: m.Host,
		Dir: e.vmDir(m.Name), VNCPort: strconv.Itoa(m.VNCPort),
		WebPort: strconv.Itoa(m.WebPort), SSHPort: strconv.Itoa(m.SSHPort),
		SSHKey: e.keyPath(m.Name), Created: m.Created,
		Mem: m.Mem, CPUs: m.CPUs, Disk: m.Disk,
		Overrides: m.Overrides, NixArgs: m.NixArgs,
	}
	if !m.Headless {
		st.URL = fmt.Sprintf("http://%s:%d/vnc.html?autoconnect=1&resize=scale", m.DisplayAddr, m.WebPort)
	}
	st.DiskBytes, st.DiskUsedBytes = diskUsage(filepath.Join(e.vmDir(m.Name), "disk.qcow2"))
	if pid, running := e.pid(m.Name); running {
		st.State = "running"
		st.RSSBytes = rssBytes(pid)
	}
	return st, nil
}

func (e *Engine) emitStatus(sink Sink, r resolved) error {
	st, err := e.statusFor(r)
	if err != nil {
		return err
	}
	if r.req.JSON {
		data, _ := json.MarshalIndent(st, "", "  ")
		return sink.Emit(Event{Type: "stdout", Data: string(data)})
	}
	return sink.Emit(Event{Type: "stdout", Data: FormatStatus(st)})
}

func (e *Engine) sshCommand(ctx context.Context, m *meta, key string, batch bool, args ...string) *exec.Cmd {
	base := []string{"-q",
		"-F", "/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		"-i", key,
		"-p", strconv.Itoa(m.SSHPort),
		"root@127.0.0.1",
	}
	if batch {
		base = append([]string{"-o", "BatchMode=yes"}, base...)
	}
	return exec.CommandContext(ctx, "ssh", append(base, args...)...)
}

func (e *Engine) waitSSH(ctx context.Context, r resolved, m *meta, key string) error {
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if _, running := e.pid(m.Name); !running {
			return fmt.Errorf("the VM exited while booting; see %s", filepath.Join(r.vmDir, "console.log"))
		}
		if streamExec(ctx, nopSink{}, e.sshCommand(ctx, m, key, true, "true")) == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timed out waiting for SSH; see %s", filepath.Join(r.vmDir, "console.log"))
}

// probeSSH reports whether the running guest accepts our key within timeout.
// It is used to decide if an "already running" VM is actually usable.
func (e *Engine) probeSSH(ctx context.Context, m *meta, key string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if streamExec(ctx, nopSink{}, e.sshCommand(ctx, m, key, true, "true")) == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}

type nopSink struct{}

func (nopSink) Emit(Event) error { return nil }

// streamExec runs a command and forwards combined output to the sink.
func streamExec(ctx context.Context, sink Sink, cmd *exec.Cmd) error {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		_ = pw.Close()
	}()
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		_ = sink.Emit(Event{Type: "stdout", Data: scanner.Text()})
	}
	return <-done
}

// runToFile runs a command with its combined output captured to a file.
func runToFile(ctx context.Context, path string, env []string, name string, args ...string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	cmd.Stdout = file
	cmd.Stderr = file
	return cmd.Run()
}

func atoiOr0(value string) int {
	n, _ := strconv.Atoi(value)
	return n
}

// diskUsage returns the apparent size of a disk image and the blocks it
// actually occupies on disk (qcow2 files are sparse).
func diskUsage(path string) (apparent, used int64) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0
	}
	apparent = info.Size()
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		used = st.Blocks * 512
	}
	return apparent, used
}

// rssBytes reports a process's resident set size.
func rssBytes(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * int64(os.Getpagesize())
}

func tailLines(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func access(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return file.Close()
}

func lookPath(name string) error {
	_, err := exec.LookPath(name)
	return err
}
