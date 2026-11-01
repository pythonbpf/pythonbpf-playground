package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// procTable: one Firecracker process per slot, shared by both launchers.

type vmProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	tail *ringBuffer
	stop bool // set when we killed it on purpose
}

type procTable struct {
	mu    sync.Mutex
	procs map[int]*vmProc
}

func newProcTable() *procTable { return &procTable{procs: map[int]*vmProc{}} }

func (t *procTable) start(slot int, cmd *exec.Cmd) error {
	p := &vmProc{cmd: cmd, done: make(chan struct{}), tail: newRingBuffer(16 * 1024)}
	cmd.Stdin = nil // /dev/null: nobody types into the serial console
	cmd.Stdout = p.tail
	cmd.Stderr = p.tail
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return err
	}
	t.mu.Lock()
	t.procs[slot] = p
	t.mu.Unlock()
	go func() {
		err := cmd.Wait()
		t.mu.Lock()
		stopped := p.stop
		t.mu.Unlock()
		if !stopped {
			log.Printf("slot %d: firecracker exited on its own (%v); console tail:\n%s",
				slot, err, lastLines(p.tail.String(), 15))
		}
		close(p.done)
	}()
	return nil
}

// kill stops the slot's VM (if any) and waits for the process to be gone.
func (t *procTable) kill(slot int) {
	t.mu.Lock()
	p := t.procs[slot]
	delete(t.procs, slot)
	if p != nil {
		p.stop = true
	}
	t.mu.Unlock()
	if p == nil {
		return
	}
	p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		log.Printf("slot %d: firecracker did not exit after SIGKILL", slot)
	}
}

func (t *procTable) exited(slot int) <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p := t.procs[slot]; p != nil {
		return p.done
	}
	c := make(chan struct{})
	close(c)
	return c
}

// ---------------------------------------------------------------------------
// directLauncher: plain firecracker, no jail. For local development only.

type directLauncher struct {
	firecracker string
	workDir     string
	imagesDir   string
	spec        VMSpec
	procs       *procTable
}

func newDirectLauncher(fc, workDir, imagesDir string, spec VMSpec) *directLauncher {
	return &directLauncher{firecracker: fc, workDir: workDir, imagesDir: imagesDir, spec: spec, procs: newProcTable()}
}

func (d *directLauncher) Start(slot int, image string) (string, error) {
	rootfs, err := imageRootfs(d.imagesDir, image)
	if err != nil {
		return "", err
	}
	d.procs.kill(slot)
	dir := filepath.Join(d.workDir, fmt.Sprintf("slot%d", slot))
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sock := filepath.Join(dir, "v.sock")
	cfg := filepath.Join(dir, "vm.json")
	if err := os.WriteFile(cfg, firecrackerConfig(d.spec, d.spec.Kernel, rootfs, sock), 0o600); err != nil {
		return "", err
	}
	cmd := exec.Command(d.firecracker, "--no-api", "--config-file", cfg, "--level", "Warning")
	if err := d.procs.start(slot, cmd); err != nil {
		return "", err
	}
	if err := waitForSocket(sock, d.procs.exited(slot)); err != nil {
		d.procs.kill(slot)
		return "", err
	}
	return sock, nil
}

func (d *directLauncher) Stop(slot int) error {
	d.procs.kill(slot)
	os.RemoveAll(filepath.Join(d.workDir, fmt.Sprintf("slot%d", slot)))
	return nil
}

// ---------------------------------------------------------------------------
// jailerLauncher: production. Runs as root inside `pgd launcher`.
//
// Each slot's Firecracker runs through the jailer: chroot, its own uid/gid,
// an empty network namespace, cgroup v2 CPU/memory/pids caps, and
// Firecracker's built-in seccomp filters.

type jailerLauncher struct {
	jailer, firecracker string
	chrootBase          string // e.g. /srv/jailer
	netns               string // e.g. /var/run/netns/pythonbpf-playground
	parentCgroup        string
	cpuMax              string // cgroup cpu.max, e.g. "100000 100000"
	imagesDir           string // <id>/rootfs.squashfs per playground version
	spec                VMSpec
	sockGid             int // group that may connect to the vsock socket (pgd's group)
	procs               *procTable
}

func (j *jailerLauncher) slotUser(slot int) (uid, gid int, err error) {
	u, err := user.Lookup(fmt.Sprintf("pg-fc%d", slot))
	if err != nil {
		return 0, 0, err
	}
	uid, _ = strconv.Atoi(u.Uid)
	gid, _ = strconv.Atoi(u.Gid)
	return uid, gid, nil
}

func (j *jailerLauncher) jailDir(slot int) string {
	return filepath.Join(j.chrootBase, filepath.Base(j.firecracker), fmt.Sprintf("slot%d", slot))
}

func (j *jailerLauncher) cleanup(slot int) {
	j.procs.kill(slot)
	// The jailer creates this cgroup. cgroup.kill also catches a VM left over
	// from a previous launcher process that procs no longer knows about.
	cg := filepath.Join("/sys/fs/cgroup", j.parentCgroup, fmt.Sprintf("slot%d", slot))
	os.WriteFile(filepath.Join(cg, "cgroup.kill"), []byte("1"), 0)
	os.RemoveAll(j.jailDir(slot))
	for i := 0; i < 20; i++ {
		if err := os.Remove(cg); err == nil || errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (j *jailerLauncher) Start(slot int, image string) (string, error) {
	rootfs, err := imageRootfs(j.imagesDir, image)
	if err != nil {
		return "", err
	}
	j.cleanup(slot)
	uid, gid, err := j.slotUser(slot)
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("slot%d", slot)
	root := filepath.Join(j.jailDir(slot), "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	// Kernel and rootfs are hard links (same filesystem), read-only for the VMM.
	if err := linkOrCopy(j.spec.Kernel, filepath.Join(root, "vmlinux")); err != nil {
		return "", err
	}
	if err := linkOrCopy(rootfs, filepath.Join(root, "rootfs.squashfs")); err != nil {
		return "", err
	}
	cfg := firecrackerConfig(j.spec, "vmlinux", "rootfs.squashfs", "v.sock")
	if err := os.WriteFile(filepath.Join(root, "vm.json"), cfg, 0o644); err != nil {
		return "", err
	}
	memMax := (j.spec.MemMiB + 64) << 20
	cmd := exec.Command(j.jailer,
		"--id", id,
		"--exec-file", j.firecracker,
		"--uid", strconv.Itoa(uid), "--gid", strconv.Itoa(gid),
		"--chroot-base-dir", j.chrootBase,
		"--netns", j.netns,
		"--cgroup-version", "2",
		"--parent-cgroup", j.parentCgroup,
		"--cgroup", "cpu.max="+j.cpuMax,
		"--cgroup", "memory.max="+strconv.Itoa(memMax),
		"--cgroup", "pids.max=32",
		"--resource-limit", "no-file=256",
		"--resource-limit", "fsize=1048576",
		"--",
		"--no-api", "--config-file", "vm.json", "--level", "Warning",
	)
	if err := j.procs.start(slot, cmd); err != nil {
		return "", err
	}
	sock := filepath.Join(root, "v.sock")
	if err := waitForSocket(sock, j.procs.exited(slot)); err != nil {
		j.cleanup(slot)
		return "", err
	}
	// All VMs together get a low CPU weight versus the rest of the host
	// (pgd, cloudflared, ...), on top of each VM's own cpu.max cap.
	os.WriteFile(filepath.Join("/sys/fs/cgroup", j.parentCgroup, "cpu.weight"), []byte("25"), 0)
	// Let pgd (group sockGid) reach the socket and nothing else in the jail.
	for _, p := range []string{j.jailDir(slot), root} {
		os.Chown(p, -1, j.sockGid)
		os.Chmod(p, 0o750)
	}
	os.Chown(sock, uid, j.sockGid)
	os.Chmod(sock, 0o660)
	return sock, nil
}

func (j *jailerLauncher) Stop(slot int) error {
	j.cleanup(slot)
	return nil
}

func waitForSocket(path string, exited <-chan struct{}) error {
	deadline := time.After(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-exited:
			return errors.New("firecracker exited during startup")
		case <-deadline:
			return errors.New("firecracker did not create its vsock socket")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---------------------------------------------------------------------------
// Launcher daemon (root) <-> pgd (unprivileged) over a unix socket.
// Protocol, one request per connection: "start N <image>\n" -> "ok <path>\n" | "err <msg>\n"
//                                        "stop N\n"          -> "ok\n"
// <image> is checked against the images directory by the launcher itself.

func serveLauncher(sockPath string, l Launcher, slots int, allowUID int, sockGid int) error {
	os.Remove(sockPath)
	os.MkdirAll(filepath.Dir(sockPath), 0o755)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	os.Chown(sockPath, 0, sockGid)
	os.Chmod(sockPath, 0o660)
	log.Printf("launcher listening on %s", sockPath)
	var slotLocks = make([]sync.Mutex, slots)
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func(c net.Conn) {
			defer c.Close()
			if uid, ok := peerUID(c); !ok || (uid != 0 && uid != allowUID) {
				fmt.Fprintf(c, "err permission denied\n")
				return
			}
			c.SetDeadline(time.Now().Add(5 * time.Second))
			line, err := bufio.NewReader(io.LimitReader(c, 128)).ReadString('\n')
			if err != nil {
				return
			}
			c.SetDeadline(time.Now().Add(60 * time.Second))
			f := strings.Fields(line)
			if len(f) < 2 {
				fmt.Fprintf(c, "err bad request\n")
				return
			}
			op := f[0]
			slot, err := strconv.Atoi(f[1])
			if err != nil || slot < 0 || slot >= slots || (op == "start") != (len(f) == 3) || len(f) > 3 {
				fmt.Fprintf(c, "err bad request\n")
				return
			}
			slotLocks[slot].Lock()
			defer slotLocks[slot].Unlock()
			switch op {
			case "start":
				path, err := l.Start(slot, f[2])
				if err != nil {
					log.Printf("slot %d: start failed: %v", slot, err)
					fmt.Fprintf(c, "err %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
					return
				}
				fmt.Fprintf(c, "ok %s\n", path)
			case "stop":
				l.Stop(slot)
				fmt.Fprintf(c, "ok\n")
			default:
				fmt.Fprintf(c, "err bad request\n")
			}
		}(c)
	}
}

func peerUID(c net.Conn) (int, bool) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var cred *syscall.Ucred
	raw.Control(func(fd uintptr) {
		cred, err = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil || cred == nil {
		return 0, false
	}
	return int(cred.Uid), true
}

// launcherClient is what pgd uses to reach the root launcher daemon.
type launcherClient struct{ sock string }

func (lc launcherClient) call(req string) (string, error) {
	c, err := net.DialTimeout("unix", lc.sock, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(90 * time.Second))
	if _, err := io.WriteString(c, req+"\n"); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if rest, ok := strings.CutPrefix(line, "ok"); ok {
		return strings.TrimSpace(rest), nil
	}
	return "", errors.New(strings.TrimPrefix(line, "err "))
}

func (lc launcherClient) Start(slot int, image string) (string, error) {
	return lc.call(fmt.Sprintf("start %d %s", slot, image))
}

func (lc launcherClient) Stop(slot int) error {
	_, err := lc.call(fmt.Sprintf("stop %d", slot))
	return err
}

// ---------------------------------------------------------------------------

type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newRingBuffer(max int) *ringBuffer { return &ringBuffer{max: max} }

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = append(r.buf[:0], r.buf[len(r.buf)-r.max:]...)
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
