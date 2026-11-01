// pgd: the PythonBPF playground daemon.
//
//	pgd serve     web UI + WebSocket API + queue (unprivileged)
//	pgd launcher  starts/stops jailed Firecracker microVMs (root)
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed static
var staticFS embed.FS

type server struct {
	m       *Manager
	trustCF bool
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pgd serve|launcher [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "launcher":
		launcher(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: pgd serve|launcher [flags]")
		os.Exit(2)
	}
}

func serve(args []string) {
	fl := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fl.String("listen", "127.0.0.1:8090", "HTTP listen address")
	launcherSock := fl.String("launcher", "/run/pythonbpf-playground/launcher.sock", "launcher daemon socket")
	direct := fl.String("direct", "", "dev mode: path to a firecracker binary to run VMs directly (no jailer)")
	kernel := fl.String("kernel", "guest/out/vmlinux", "dev mode: guest kernel")
	imagesDir := fl.String("images", "/opt/pythonbpf-playground/images", "playground versions: <dir>/<id>/rootfs.squashfs")
	workDir := fl.String("workdir", "/tmp/pgd-dev", "dev mode: VM working directory")
	mem := fl.Int("mem", 256, "dev mode: guest memory (MiB)")
	slots := fl.Int("slots", 2, "concurrent microVMs")
	queue := fl.Int("queue", 20, "max waiting sessions")
	idle := fl.Duration("idle", 2*time.Minute, "idle timeout")
	hardCap := fl.Duration("cap", 10*time.Minute, "max session length while others wait")
	grace := fl.Duration("grace", 2*time.Minute, "minimum notice before the cap applies")
	runTimeout := fl.Duration("run-timeout", 20*time.Second, "wall clock per run")
	perIP := fl.Bool("per-ip", true, "one session per client IP")
	trustCF := fl.Bool("trust-cf", true, "trust the CF-Connecting-IP header (listen on localhost only!)")
	frameAncestors := fl.String("frame-ancestors", "https://pythonbpf.github.io",
		"space-separated origins allowed to embed the playground in an iframe (CSP frame-ancestors); empty = none")
	fl.Parse(args)

	var l Launcher = launcherClient{sock: *launcherSock}
	if *direct != "" {
		l = newDirectLauncher(*direct, *workDir, abs(*imagesDir), VMSpec{Kernel: abs(*kernel), MemMiB: *mem, VCPUs: 1})
		log.Printf("dev mode: running firecracker directly (no jailer)")
	}
	images, err := loadImages(*imagesDir)
	if err != nil {
		log.Fatal(err)
	}
	for _, img := range images {
		log.Printf("version %q: %s (pythonbpf %s, pylibbpf %s)%s", img.ID, img.Label, img.PythonBPF, img.PyLibBPF,
			map[bool]string{true: " [default]"}[img.Default])
	}
	m := NewManager(Config{
		Slots: *slots, MaxQueue: *queue, IdleTimeout: *idle, HardCap: *hardCap, CapGrace: *grace,
		RunTimeout: *runTimeout, BootTimeout: 45 * time.Second, MaxCode: 64 << 10,
		PerIP: *perIP, MaxRestarts: 3,
	}, l, images)
	go func() {
		for range time.Tick(time.Second) {
			m.Tick()
		}
	}()
	// Stop every VM on shutdown so nothing outlives pgd.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		for i := 0; i < *slots; i++ {
			l.Stop(i)
		}
		os.Exit(0)
	}()
	for i := 0; i < *slots; i++ {
		l.Stop(i) // clean up anything left from a previous run
	}

	srv := &server{m: m, trustCF: *trustCF}
	sub, _ := fs.Sub(staticFS, "static")
	static, err := loadAssets(sub)
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.handleWS)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(m.Status())
	})
	mux.Handle("GET /", static)

	log.Printf("pgd serving on http://%s (%d slots, assets %s)", *listen, *slots, static.version)
	hs := &http.Server{
		Addr:              *listen,
		Handler:           securityHeaders(mux, *trustCF, *frameAncestors),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(hs.ListenAndServe())
}

// visitorScheme is the scheme the visitor used to reach Cloudflare ("" if unknown).
func visitorScheme(r *http.Request) string {
	if v := r.Header.Get("CF-Visitor"); v != "" {
		if strings.Contains(v, `"scheme":"https"`) {
			return "https"
		}
		if strings.Contains(v, `"scheme":"http"`) {
			return "http"
		}
	}
	return strings.ToLower(r.Header.Get("X-Forwarded-Proto"))
}

func securityHeaders(h http.Handler, trustCF bool, frameAncestors string) http.Handler {
	if frameAncestors = strings.TrimSpace(frameAncestors); frameAncestors == "" {
		frameAncestors = "'none'"
	}
	csp := "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; connect-src 'self'; frame-ancestors " + frameAncestors + "; base-uri 'none'; form-action 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		if trustCF {
			switch visitorScheme(r) {
			case "http": // plain-HTTP visitor: send them to HTTPS
				hd.Set("Cache-Control", "public, max-age=3600")
				http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusMovedPermanently)
				return
			case "https": // browsers then refuse plain HTTP for this host only
				hd.Set("Strict-Transport-Security", "max-age=63072000")
			}
		}
		hd.Set("Content-Security-Policy", csp)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.ServeHTTP(w, r)
	})
}

func launcher(args []string) {
	fl := flag.NewFlagSet("launcher", flag.ExitOnError)
	sock := fl.String("socket", "/run/pythonbpf-playground/launcher.sock", "control socket")
	jailer := fl.String("jailer", "/usr/local/bin/jailer", "jailer binary")
	fc := fl.String("firecracker", "/usr/local/bin/firecracker", "firecracker binary")
	kernel := fl.String("kernel", "/opt/pythonbpf-playground/vmlinux", "guest kernel")
	imagesDir := fl.String("images", "/opt/pythonbpf-playground/images", "playground versions: <dir>/<id>/rootfs.squashfs")
	chroot := fl.String("chroot-base", "/srv/jailer", "jailer chroot base (same filesystem as kernel/images)")
	netns := fl.String("netns", "/var/run/netns/pythonbpf-playground", "empty network namespace for VMs")
	cgroup := fl.String("cgroup", "pythonbpf-playground", "parent cgroup (v2)")
	// One full vCPU per VM; the low cpu.weight on their parent cgroup keeps the
	// host's own services ahead whenever there is contention.
	cpuMax := fl.String("cpu-max", "100000 100000", "cgroup cpu.max per VM")
	mem := fl.Int("mem", 256, "guest memory (MiB)")
	slots := fl.Int("slots", 2, "slots")
	pgUser := fl.String("user", "playground", "user that runs `pgd serve`")
	fl.Parse(args)

	if os.Geteuid() != 0 {
		log.Fatal("launcher must run as root (the jailer needs it)")
	}
	u, err := user.Lookup(*pgUser)
	if err != nil {
		log.Fatal(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	j := &jailerLauncher{
		jailer: *jailer, firecracker: *fc, chrootBase: *chroot, netns: *netns,
		parentCgroup: *cgroup, cpuMax: *cpuMax, sockGid: gid, procs: newProcTable(),
		imagesDir: *imagesDir,
		spec:      VMSpec{Kernel: *kernel, MemMiB: *mem, VCPUs: 1},
	}
	for i := 0; i < *slots; i++ {
		j.cleanup(i)
	}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		for i := 0; i < *slots; i++ {
			j.cleanup(i)
		}
		os.Exit(0)
	}()
	log.Fatal(serveLauncher(*sock, j, *slots, uid, gid))
}

func abs(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p
	}
	wd, _ := os.Getwd()
	return wd + "/" + p
}
