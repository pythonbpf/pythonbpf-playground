package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// ---- fakes ----------------------------------------------------------------

type fakeClient struct {
	mu   sync.Mutex
	msgs []map[string]any
}

func (c *fakeClient) SendJSON(v any) {
	b, _ := json.Marshal(v)
	c.SendText(b)
}

func (c *fakeClient) SendText(b []byte) {
	var m map[string]any
	json.Unmarshal(b, &m)
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
}

func (c *fakeClient) SendBinary([]byte) {}

// last returns the most recent message of type t, or nil.
func (c *fakeClient) last(t string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.msgs) - 1; i >= 0; i-- {
		if c.msgs[i]["t"] == t {
			return c.msgs[i]
		}
	}
	return nil
}

type fakeLauncher struct {
	mu            sync.Mutex
	starts, stops int
	images        []string // image of each Start, in order
}

func (f *fakeLauncher) Start(slot int, image string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.images = append(f.images, image)
	return fmt.Sprintf("sock%d-%d", slot, f.starts), nil
}

func (f *fakeLauncher) Stop(slot int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeAgent is the guest side of a VM control connection.
type fakeAgent struct {
	conn net.Conn
	mu   sync.Mutex
	got  []map[string]any
}

type harness struct {
	t      *testing.T
	m      *Manager
	l      *fakeLauncher
	clk    *clock
	mu     sync.Mutex
	agents map[string]*fakeAgent // by vsock path
}

func newHarness(t *testing.T, cfg Config) *harness {
	h := &harness{t: t, l: &fakeLauncher{}, clk: &clock{t: time.Unix(1_000_000, 0)}, agents: map[string]*fakeAgent{}}
	h.m = NewManager(cfg, h.l, []Image{{ID: "master", Label: "master", Default: true}, {ID: "0.2.0", Label: "0.2.0"}})
	h.m.now = h.clk.now
	h.m.waitAgent = func(sock string, _ time.Duration, _ <-chan struct{}) (net.Conn, *bufio.Reader, Hello, error) {
		host, guest := net.Pipe()
		a := &fakeAgent{conn: guest}
		go func() {
			r := bufio.NewReader(guest)
			for {
				line, err := r.ReadBytes('\n')
				if err != nil {
					return
				}
				var m map[string]any
				json.Unmarshal(line, &m)
				a.mu.Lock()
				a.got = append(a.got, m)
				a.mu.Unlock()
				if m["op"] == "run" {
					fmt.Fprintf(guest, `{"t":"exit","id":%v,"code":0,"reason":"exited"}`+"\n", m["id"])
				}
			}
		}()
		h.mu.Lock()
		h.agents[sock] = a
		h.mu.Unlock()
		return host, bufio.NewReader(host), Hello{Kernel: "test"}, nil
	}
	return h
}

func defaultCfg() Config {
	return Config{
		Slots: 2, MaxQueue: 3, IdleTimeout: 2 * time.Minute, HardCap: 10 * time.Minute,
		CapGrace: 2 * time.Minute, RunTimeout: 20 * time.Second, BootTimeout: time.Second,
		MaxCode: 1024, PerIP: true, MaxRestarts: 2,
	}
}

func (h *harness) connect(ip string) (*Session, *fakeClient) {
	c := &fakeClient{}
	return h.m.Connect(c, ip), c
}

func (h *harness) state(s *Session) State {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return s.state
}

func (h *harness) waitState(s *Session, want State) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.state(s) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("session state = %d, want %d", h.state(s), want)
}

func (h *harness) agentFor(s *Session) *fakeAgent {
	h.m.mu.Lock()
	sock := s.vm.sock
	h.m.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.agents[sock]
}

// ---- tests ------------------------------------------------------------------

func TestFIFOAndPromotion(t *testing.T) {
	h := newHarness(t, defaultCfg())
	s1, _ := h.connect("1.1.1.1")
	s2, _ := h.connect("2.2.2.2")
	s3, c3 := h.connect("3.3.3.3")
	s4, c4 := h.connect("4.4.4.4")
	for _, s := range []*Session{s1, s2, s3, s4} {
		h.m.Acquire(s)
	}
	h.waitState(s1, Ready)
	h.waitState(s2, Ready)
	if h.state(s3) != Waiting || h.state(s4) != Waiting {
		t.Fatal("3 and 4 should be waiting")
	}
	if p := c3.last("queued")["position"]; p != 1.0 {
		t.Fatalf("s3 position = %v", p)
	}
	if p := c4.last("queued")["position"]; p != 2.0 {
		t.Fatalf("s4 position = %v", p)
	}
	h.m.Release(s1)
	h.waitState(s3, Ready) // first in line gets the freed slot
	if h.state(s4) != Waiting {
		t.Fatal("s4 should still wait")
	}
	if p := c4.last("queued")["position"]; p != 1.0 {
		t.Fatalf("s4 position after promotion = %v", p)
	}
}

func TestPerIPLimit(t *testing.T) {
	h := newHarness(t, defaultCfg())
	a, _ := h.connect("9.9.9.9")
	b, cb := h.connect("9.9.9.9")
	h.m.Acquire(a)
	h.m.Acquire(b)
	if h.state(b) != Idle || cb.last("error") == nil {
		t.Fatal("second tab from the same IP must be refused")
	}
	h.m.Release(a)
	h.m.Acquire(b)
	h.waitState(b, Ready)
}

func TestQueueCap(t *testing.T) {
	h := newHarness(t, defaultCfg()) // 2 slots + 3 queue
	var last *fakeClient
	for i := 0; i < 6; i++ {
		s, c := h.connect(fmt.Sprintf("10.0.0.%d", i))
		h.m.Acquire(s)
		last = c
	}
	if last.last("error") == nil {
		t.Fatal("6th session should be turned away")
	}
	if st := h.m.Status(); st["waiting"] != 3 {
		t.Fatalf("waiting = %v", st["waiting"])
	}
}

func TestDisconnectWhileWaiting(t *testing.T) {
	h := newHarness(t, defaultCfg())
	for i := 0; i < 2; i++ {
		s, _ := h.connect(fmt.Sprintf("10.0.1.%d", i))
		h.m.Acquire(s)
	}
	w, _ := h.connect("10.0.1.9")
	h.m.Acquire(w)
	h.m.Disconnect(w)
	if st := h.m.Status(); st["waiting"] != 0 {
		t.Fatalf("waiting = %v after disconnect", st["waiting"])
	}
}

func TestIdleTimeout(t *testing.T) {
	h := newHarness(t, defaultCfg())
	s, c := h.connect("1.2.3.4")
	h.m.Acquire(s)
	h.waitState(s, Ready)
	h.clk.add(100 * time.Second)
	h.m.Tick()
	if c.last("idle_warning") == nil {
		t.Fatal("expected an idle warning 30s before the timeout")
	}
	h.m.Touch(s)
	h.clk.add(100 * time.Second)
	h.m.Tick()
	if h.state(s) != Ready {
		t.Fatal("activity should reset the idle timer")
	}
	h.clk.add(30 * time.Second)
	h.m.Tick()
	if h.state(s) != Idle || c.last("ended") == nil {
		t.Fatal("session should end after 2 idle minutes")
	}
}

func TestHardCapOnlyWhenSomeoneWaits(t *testing.T) {
	cfg := defaultCfg()
	cfg.Slots = 1
	h := newHarness(t, cfg)
	s, c := h.connect("1.1.1.1")
	h.m.Acquire(s)
	h.waitState(s, Ready)
	for i := 0; i < 30; i++ { // 30 active minutes, nobody waiting
		h.clk.add(time.Minute)
		h.m.Touch(s)
		h.m.Tick()
	}
	if h.state(s) != Ready {
		t.Fatal("no one is waiting: the session must not be capped")
	}
	w, _ := h.connect("2.2.2.2")
	h.m.Acquire(w)
	if c.last("lease")["deadline_ms"] == 0.0 {
		t.Fatal("expected a deadline once someone waits")
	}
	h.clk.add(time.Minute)
	h.m.Touch(s)
	h.m.Tick()
	if h.state(s) != Ready {
		t.Fatal("grace period: must not cut off before 2 minutes notice")
	}
	h.clk.add(61 * time.Second)
	h.m.Touch(s)
	h.m.Tick()
	if h.state(s) != Idle {
		t.Fatal("hard cap should end the session after the grace period")
	}
	h.waitState(w, Ready)
}

func TestDeadlineClearedWhenQueueEmpties(t *testing.T) {
	cfg := defaultCfg()
	cfg.Slots = 1
	h := newHarness(t, cfg)
	s, c := h.connect("1.1.1.1")
	h.m.Acquire(s)
	h.waitState(s, Ready)
	w, _ := h.connect("2.2.2.2")
	h.m.Acquire(w)
	h.m.Disconnect(w)
	if c.last("lease")["deadline_ms"] != 0.0 {
		t.Fatal("deadline should be cleared when nobody waits any more")
	}
}

func TestPendingRunAfterBootAndRunForwarding(t *testing.T) {
	h := newHarness(t, defaultCfg())
	s, c := h.connect("1.1.1.1")
	h.m.Run(s, "print('hi')") // not acquired yet: acquires, boots, then runs
	h.waitState(s, Ready)
	deadline := time.Now().Add(2 * time.Second)
	for c.last("exit") == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if c.last("exit") == nil {
		t.Fatal("run never finished")
	}
	a := h.agentFor(s)
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.got) != 1 || a.got[0]["code"] != "print('hi')" {
		t.Fatalf("agent got %v", a.got)
	}
}

func TestCodeLimit(t *testing.T) {
	h := newHarness(t, defaultCfg())
	s, c := h.connect("1.1.1.1")
	h.m.Run(s, string(make([]byte, 2048)))
	if c.last("error") == nil || h.state(s) != Idle {
		t.Fatal("oversized code must be rejected before acquiring a VM")
	}
}

func TestVMCrashRestartsThenGivesUp(t *testing.T) {
	h := newHarness(t, defaultCfg()) // MaxRestarts: 2
	s, c := h.connect("1.1.1.1")
	h.m.Acquire(s)
	for i := 0; i < 2; i++ {
		h.waitState(s, Ready)
		h.agentFor(s).conn.Close() // VM dies
		deadline := time.Now().Add(2 * time.Second)
		for c.last("vm_restart") == nil && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if c.last("vm_restart") == nil {
			t.Fatal("expected a VM restart")
		}
		c.mu.Lock()
		c.msgs = nil
		c.mu.Unlock()
	}
	h.waitState(s, Ready)
	h.agentFor(s).conn.Close()
	h.waitState(s, Idle)
	if c.last("ended") == nil {
		t.Fatal("session should end after too many crashes")
	}
}

func TestRunWatchdog(t *testing.T) {
	h := newHarness(t, defaultCfg())
	h.m.waitAgent = func(sock string, _ time.Duration, _ <-chan struct{}) (net.Conn, *bufio.Reader, Hello, error) {
		host, guest := net.Pipe()
		go func() { // a hung agent: reads but never answers
			r := bufio.NewReader(guest)
			for {
				if _, err := r.ReadBytes('\n'); err != nil {
					return
				}
			}
		}()
		return host, bufio.NewReader(host), Hello{}, nil
	}
	s, c := h.connect("1.1.1.1")
	h.m.Run(s, "x")
	h.waitState(s, Ready)
	time.Sleep(20 * time.Millisecond) // let the pending run be sent
	h.clk.add(40 * time.Second)
	h.m.Touch(s)
	h.m.Tick()
	if c.last("vm_restart") == nil || c.last("exit") == nil {
		t.Fatal("watchdog should report the run and restart the VM")
	}
}

func TestETAOrdering(t *testing.T) {
	h := newHarness(t, defaultCfg())
	for i := 0; i < 2; i++ {
		s, _ := h.connect(fmt.Sprintf("10.0.2.%d", i))
		h.m.Acquire(s)
		h.waitState(s, Ready)
	}
	var cs []*fakeClient
	for i := 0; i < 3; i++ {
		s, c := h.connect(fmt.Sprintf("10.0.3.%d", i))
		h.m.Acquire(s)
		cs = append(cs, c)
	}
	prev := -1.0
	for _, c := range cs {
		eta := c.last("queued")["eta_s"].(float64)
		if eta < prev {
			t.Fatalf("ETAs must not decrease down the queue: %v then %v", prev, eta)
		}
		prev = eta
	}
}

func TestVersionSelection(t *testing.T) {
	h := newHarness(t, defaultCfg())
	s, c := h.connect("1.1.1.1")
	if c.last("images") == nil || c.last("images")["default"] != "master" {
		t.Fatal("new tabs must be told the available versions")
	}
	h.m.SetImage(s, "../etc")
	if c.last("error") == nil {
		t.Fatal("unknown versions must be rejected")
	}
	h.m.SetImage(s, "0.2.0") // before acquiring: just remembered
	h.m.Acquire(s)
	h.waitState(s, Ready)
	h.m.SetImage(s, "master") // while running: reboot in the same slot
	h.waitState(s, Ready)
	h.l.mu.Lock()
	defer h.l.mu.Unlock()
	if len(h.l.images) != 2 || h.l.images[0] != "0.2.0" || h.l.images[1] != "master" {
		t.Fatalf("launcher saw images %v, want [0.2.0 master]", h.l.images)
	}
	if h.state(s) != Ready || s.slot != 0 {
		t.Fatal("switching versions must keep the slot")
	}
}
