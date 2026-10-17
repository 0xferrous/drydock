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
	"strconv"
	"strings"
	"syscall"
	"time"
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
// module, so `nix build --flake` (and therefore --override-input and arbitrary
// nix flags) works and the user's config can live in any layout.
func (e *Engine) buildAttr(ctx context.Context, req Request, attr, link string, sink Sink) (string, error) {
	evalDir := filepath.Join(e.vmDir(req.Name), "eval")
	if err := e.writeWrapper(evalDir, req); err != nil {
		return "", err
	}
	args := []string{
		"build", "--impure", "--no-write-lock-file",
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
	cmd.Env = append(os.Environ(),
		"DRYDOCK_SSH_PUBKEY_FILE="+e.keyPath(req.Name)+".pub",
		"DRYDOCK_MEM="+req.Mem,
		"DRYDOCK_CPUS="+req.CPUs,
		"DRYDOCK_DISK="+req.Disk,
	)
	if err := streamExec(ctx, sink, cmd); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(link)
}

// writeWrapper generates the tiny flake that pins the user's config as an input
// and exposes the VM and system derivations with the guest module applied.
func (e *Engine) writeWrapper(dir string, req Request) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(dir, "flake.lock"))
	guest := filepath.Join(e.NixDir, "guest")
	contents := fmt.Sprintf(`{
  inputs.config.url = %s;

  outputs = { self, config, ... }:
    let
      preview = (builtins.getAttr %s config.nixosConfigurations).extendModules {
        modules = [ %s ];
      };
    in {
      drydock = {
        vm = preview.config.system.build.vm;
        system = preview.config.virtualisation.vmVariant.system.build.toplevel;
      };
    };
}
`, nixQuote(req.Flake), nixQuote(req.Host), guest)
	return os.WriteFile(filepath.Join(dir, "flake.nix"), []byte(contents), 0o644)
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
		state := "stopped"
		if _, running := e.pid(entry.Name()); running {
			state = "running"
		}
		out = append(out, VMInfo{Name: entry.Name(), State: state})
	}
	return out, nil
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
	state := "stopped"
	if _, running := e.pid(m.Name); running {
		state = "running"
	}
	st := Status{
		Name: m.Name, State: state, Flake: m.Flake, Host: m.Host,
		Dir: e.vmDir(m.Name), VNCPort: strconv.Itoa(m.VNCPort),
		WebPort: strconv.Itoa(m.WebPort), SSHPort: strconv.Itoa(m.SSHPort),
		SSHKey: e.keyPath(m.Name),
	}
	if !m.Headless {
		st.URL = fmt.Sprintf("http://%s:%d/vnc.html?autoconnect=1&resize=scale", m.DisplayAddr, m.WebPort)
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
