package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	CtrlPort = 5000 // guest agent: JSON-lines run/control
	PtyPort  = 5001 // guest agent: terminal
)

// VMSpec is what every playground microVM looks like.
type VMSpec struct {
	Kernel string // absolute path on the host
	MemMiB int
	VCPUs  int
}

// firecrackerConfig renders the --config-file JSON. Paths are as seen by the
// firecracker process (relative to its chroot when jailed).
func firecrackerConfig(spec VMSpec, kernel, rootfs, vsock string) []byte {
	cfg := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": kernel,
			"boot_args": "console=ttyS0 quiet loglevel=3 reboot=k panic=1 pci=off " +
				"root=/dev/vda ro rootfstype=squashfs init=/sbin/pg-init",
		},
		"drives": []any{map[string]any{
			"drive_id":       "rootfs",
			"path_on_host":   rootfs,
			"is_root_device": true,
			"is_read_only":   true,
		}},
		"machine-config": map[string]any{
			"vcpu_count":   spec.VCPUs,
			"mem_size_mib": spec.MemMiB,
		},
		// No "network-interfaces": the guest has loopback only.
		"vsock": map[string]any{"guest_cid": 3, "uds_path": vsock},
		// Hand pages the guest frees back to the host (the host has ~1 GB RAM).
		"balloon": map[string]any{
			"amount_mib":          0,
			"deflate_on_oom":      true,
			"free_page_reporting": true,
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return b
}

// Launcher starts and stops the microVM for a slot. Start boots the given
// playground version (image id) and returns the host path of the VM's vsock
// unix socket once Firecracker is running.
type Launcher interface {
	Start(slot int, image string) (vsockPath string, err error)
	Stop(slot int) error
}

// dialVsock opens a connection to a guest vsock port through Firecracker's
// hybrid vsock unix socket ("CONNECT <port>\n" -> "OK <hostport>\n").
func dialVsock(udsPath string, port int, timeout time.Duration) (net.Conn, error) {
	c, err := net.DialTimeout("unix", udsPath, timeout)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(timeout))
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		c.Close()
		return nil, err
	}
	// Read the reply byte by byte so no guest bytes get buffered away.
	var line []byte
	b := make([]byte, 1)
	for len(line) < 64 {
		if _, err := c.Read(b); err != nil {
			c.Close()
			return nil, fmt.Errorf("vsock handshake: %w", err)
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	if !strings.HasPrefix(string(line), "OK ") {
		c.Close()
		return nil, fmt.Errorf("vsock handshake: %q", line)
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

// Hello is the first message the guest agent sends on the control port.
type Hello struct {
	Kernel    string `json:"kernel"`
	PythonBPF string `json:"pythonbpf"`
	PyLibBPF  string `json:"pylibbpf"`
	Python    string `json:"python"`
}

// waitForAgent retries the control port until the guest agent answers.
func waitForAgent(udsPath string, timeout time.Duration, cancel <-chan struct{}) (net.Conn, *bufio.Reader, Hello, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-cancel:
			return nil, nil, Hello{}, fmt.Errorf("cancelled")
		default:
		}
		c, err := dialVsock(udsPath, CtrlPort, 2*time.Second)
		if err == nil {
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			r := bufio.NewReaderSize(c, 64*1024)
			line, err := r.ReadBytes('\n')
			if err == nil {
				var msg struct {
					T string `json:"t"`
					Hello
				}
				if json.Unmarshal(line, &msg) == nil && msg.T == "hello" {
					c.SetReadDeadline(time.Time{})
					return c, r, msg.Hello, nil
				}
				err = fmt.Errorf("unexpected first message %q", line)
			}
			c.Close()
			lastErr = err
		} else {
			lastErr = err
		}
		time.Sleep(150 * time.Millisecond)
	}
	return nil, nil, Hello{}, fmt.Errorf("guest agent did not come up: %v", lastErr)
}

// waitForFile polls until path exists.
func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", filepath.Base(path))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
