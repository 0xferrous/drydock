package drydock

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
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

// startDisplay serves noVNC for the VM, preferring the user systemd manager and
// falling back to a detached process.
func (e *Engine) startDisplay(m *meta) error {
	novnc, err := exec.LookPath("novnc")
	if err != nil {
		return err
	}
	listen := fmt.Sprintf("%s:%d", m.DisplayAddr, m.WebPort)
	vncAddr := fmt.Sprintf("127.0.0.1:%d", m.VNCPort)
	e.stopDisplay(m.Name)

	unit := "drydock-" + m.Name + "-display"
	if err := exec.Command("systemd-run", "--user", "--quiet", "--collect", "--unit="+unit,
		novnc, "--vnc", vncAddr, "--listen", listen).Run(); err == nil {
		return nil
	}

	logFile, err := os.Create(filepath.Join(e.vmDir(m.Name), "novnc.log"))
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(novnc, "--vnc", vncAddr, "--listen", listen)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(e.vmDir(m.Name), "novnc.pid"),
		[]byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
}

func (e *Engine) stopDisplay(name string) {
	_ = exec.Command("systemctl", "--user", "stop", "drydock-"+name+"-display").Run()
	pidFile := filepath.Join(e.vmDir(name), "novnc.pid")
	if data, err := os.ReadFile(pidFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
		_ = os.Remove(pidFile)
	}
}

// Screenshot captures the guest framebuffer via the QEMU monitor and returns a
// PNG. This lets an agent "look" at the result without a browser.
func (e *Engine) Screenshot(ctx context.Context, req Request) ([]byte, error) {
	r, err := e.resolve(req)
	if err != nil {
		return nil, err
	}
	m, err := e.loadMeta(r.req.Name)
	if err != nil {
		return nil, err
	}
	if m.Headless {
		return nil, fmt.Errorf("VM '%s' is headless", m.Name)
	}
	if _, running := e.pid(m.Name); !running {
		return nil, fmt.Errorf("VM '%s' is not running", m.Name)
	}

	ppm := filepath.Join(r.vmDir, "screen.ppm")
	_ = os.Remove(ppm)
	conn, err := net.Dial("unix", filepath.Join(r.vmDir, "qmp.sock"))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil { // QMP greeting
		return nil, err
	}
	if _, err := fmt.Fprintln(conn, `{"execute":"qmp_capabilities"}`); err != nil {
		return nil, err
	}
	_, _ = reader.ReadString('\n')
	if _, err := fmt.Fprintf(conn, "{\"execute\":\"screendump\",\"arguments\":{\"filename\":%q}}\n", ppm); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ppm); err == nil {
			time.Sleep(200 * time.Millisecond) // let QEMU finish writing
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	data, err := os.ReadFile(ppm)
	if err != nil {
		return nil, fmt.Errorf("could not capture the guest display: %w", err)
	}
	_ = os.Remove(ppm)

	img, err := decodePPM(data)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodePPM decodes a binary P6 pixmap, which is what QEMU's screendump writes.
func decodePPM(data []byte) (image.Image, error) {
	reader := bufio.NewReader(bytes.NewReader(data))
	token := func() (string, error) {
		for {
			b, err := reader.ReadByte()
			if err != nil {
				return "", err
			}
			switch {
			case b == '#':
				for {
					c, err := reader.ReadByte()
					if err != nil {
						return "", err
					}
					if c == '\n' {
						break
					}
				}
			case b == ' ' || b == '\t' || b == '\n' || b == '\r':
				continue
			default:
				var sb []byte
				sb = append(sb, b)
				for {
					c, err := reader.ReadByte()
					if err != nil || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
						break
					}
					sb = append(sb, c)
				}
				return string(sb), nil
			}
		}
	}

	magic, err := token()
	if err != nil {
		return nil, err
	}
	if magic != "P6" {
		return nil, fmt.Errorf("unsupported PPM format %q", magic)
	}
	widthStr, err := token()
	if err != nil {
		return nil, err
	}
	heightStr, err := token()
	if err != nil {
		return nil, err
	}
	maxStr, err := token()
	if err != nil {
		return nil, err
	}
	width, _ := strconv.Atoi(widthStr)
	height, _ := strconv.Atoi(heightStr)
	maxValue, _ := strconv.Atoi(maxStr)
	if width <= 0 || height <= 0 || maxValue <= 0 || maxValue > 255 {
		return nil, fmt.Errorf("invalid PPM header")
	}
	pixels := make([]byte, width*height*3)
	if _, err := io.ReadFull(reader, pixels); err != nil {
		return nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 0; i < width*height; i++ {
		img.Pix[i*4+0] = pixels[i*3+0]
		img.Pix[i*4+1] = pixels[i*3+1]
		img.Pix[i*4+2] = pixels[i*3+2]
		img.Pix[i*4+3] = 255
	}
	return img, nil
}
