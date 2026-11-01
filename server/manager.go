package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"time"
)

// Config holds the fairness and safety knobs.
type Config struct {
	Slots       int           // concurrent microVMs
	MaxQueue    int           // waiting sessions beyond this are turned away
	IdleTimeout time.Duration // no run / keystroke for this long -> session ends
	HardCap     time.Duration // max session length, enforced only while others wait
	CapGrace    time.Duration // minimum notice before the hard cap kicks someone out
	RunTimeout  time.Duration // wall clock per run (enforced in the guest)
	BootTimeout time.Duration
	MaxCode     int  // bytes
	PerIP       bool // at most one active/queued session per client IP
	MaxRestarts int  // VM crashes tolerated per session
}

type State int

const (
	Idle State = iota
	Waiting
	Booting
	Ready
)

// Client is the browser side of a session (a WebSocket). Sends never block.
type Client interface {
	SendJSON(v any)
	SendText(b []byte)
	SendBinary(b []byte)
}

type Session struct {
	ip string
	c  Client

	state      State
	slot       int
	gen        int // bumps on every boot / end; stale goroutines compare it
	cancelBoot chan struct{}
	joined     time.Time
	leaseStart time.Time
	lastActive time.Time
	deadline   time.Time // zero: no hard cap right now (nobody waiting)
	warned     bool      // idle warning sent

	vm       *vmConn
	term     net.Conn
	hello    Hello
	runID    int
	runUntil time.Time // watchdog: VM is considered hung after this

	pendingRun  *string
	pendingTerm *[2]int
	restarts    int
}

type vmConn struct {
	conn net.Conn
	r    *bufio.Reader
	sock string
	mu   sync.Mutex
}

func (v *vmConn) send(obj any) error {
	b, _ := json.Marshal(obj)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := v.conn.Write(append(b, '\n'))
	return err
}

type Manager struct {
	cfg Config
	l   Launcher
	now func() time.Time

	// dial hooks (tests replace these)
	waitAgent func(sock string, timeout time.Duration, cancel <-chan struct{}) (net.Conn, *bufio.Reader, Hello, error)
	dialPty   func(sock string) (net.Conn, error)

	mu       sync.Mutex
	sessions map[*Session]struct{}
	slots    []*Session
	tearing  []bool
	slotOps  []sync.Mutex // serializes launcher Start/Stop per slot
	queue    []*Session
	avg      time.Duration // moving average session length, for wait estimates
	ticks    int
	lastStat [3]int // last broadcast status (busy, slots, waiting)
}

func NewManager(cfg Config, l Launcher) *Manager {
	return &Manager{
		cfg:       cfg,
		l:         l,
		now:       time.Now,
		waitAgent: waitForAgent,
		dialPty:   func(sock string) (net.Conn, error) { return dialVsock(sock, PtyPort, 5*time.Second) },
		sessions:  map[*Session]struct{}{},
		slots:     make([]*Session, cfg.Slots),
		tearing:   make([]bool, cfg.Slots),
		slotOps:   make([]sync.Mutex, cfg.Slots),
		avg:       3 * time.Minute,
	}
}

// ---------------------------------------------------------------------------
// public API, called from the WebSocket handler

func (m *Manager) Connect(c Client, ip string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &Session{ip: ip, c: c, slot: -1}
	m.sessions[s] = struct{}{}
	c.SendJSON(m.statusLocked())
	return s
}

func (m *Manager) Disconnect(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.endLocked(s, "")
	delete(m.sessions, s)
}

func (m *Manager) Acquire(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acquireLocked(s)
}

func (m *Manager) Release(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.endLocked(s, "Session ended.")
}

func (m *Manager) Touch(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touchLocked(s)
}

func (m *Manager) Run(s *Session, code string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(code) > m.cfg.MaxCode {
		s.c.SendJSON(map[string]any{"t": "error", "msg": fmt.Sprintf("Code is too long (limit %d KB).", m.cfg.MaxCode/1024)})
		return
	}
	m.touchLocked(s)
	switch s.state {
	case Ready:
		m.startRunLocked(s, code)
	case Idle:
		s.pendingRun = &code
		m.acquireLocked(s)
	default: // waiting or booting: run as soon as the VM is up
		s.pendingRun = &code
	}
}

func (m *Manager) Kill(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.state == Ready && s.vm != nil {
		s.vm.send(map[string]any{"op": "kill"})
	}
}

func (m *Manager) OpenTerm(s *Session, cols, rows int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touchLocked(s)
	switch s.state {
	case Ready:
		m.openTermLocked(s, cols, rows)
	case Idle:
		s.pendingTerm = &[2]int{cols, rows}
		m.acquireLocked(s)
	default:
		s.pendingTerm = &[2]int{cols, rows}
	}
}

func (m *Manager) TermInput(s *Session, data []byte) {
	m.mu.Lock()
	t := s.term
	m.touchLocked(s)
	m.mu.Unlock()
	if t == nil {
		return
	}
	t.SetWriteDeadline(time.Now().Add(2 * time.Second))
	for len(data) > 0 {
		n := min(len(data), 4096)
		t.Write(ptyFrame(0, data[:n]))
		data = data[n:]
	}
}

func (m *Manager) Resize(s *Session, cols, rows int) {
	m.mu.Lock()
	t := s.term
	m.mu.Unlock()
	if t != nil {
		t.SetWriteDeadline(time.Now().Add(2 * time.Second))
		t.Write(resizeFrame(cols, rows))
	}
}

// Tick enforces idle timeouts, hard caps and the run watchdog. Call every second.
func (m *Manager) Tick() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.ticks++
	for _, s := range append([]*Session(nil), m.slots...) {
		if s == nil {
			continue
		}
		idle := now.Sub(s.lastActive)
		switch {
		case s.state == Ready && !s.runUntil.IsZero() && now.After(s.runUntil):
			log.Printf("slot %d: run watchdog fired, restarting VM", s.slot)
			m.vmDiedLocked(s, "The VM stopped responding.")
		case s.state == Ready && idle > m.cfg.IdleTimeout:
			m.endLocked(s, "Session ended after being idle. Press Run to start a new one.")
		case !s.deadline.IsZero() && now.After(s.deadline):
			m.endLocked(s, "Time's up: someone else is waiting for a VM. Press Run to rejoin the queue.")
		case s.state == Ready && !s.warned && idle > m.cfg.IdleTimeout-30*time.Second:
			s.warned = true
			s.c.SendJSON(map[string]any{"t": "idle_warning", "seconds": int((m.cfg.IdleTimeout - idle).Seconds())})
		}
	}
	if m.ticks%10 == 0 {
		m.sendQueueLocked()
	}
}

func (m *Manager) Status() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked()
}

// ---------------------------------------------------------------------------
// internals (m.mu held)

func (m *Manager) touchLocked(s *Session) {
	s.lastActive = m.now()
	s.warned = false
}

func (m *Manager) acquireLocked(s *Session) {
	if s.state != Idle {
		return
	}
	if m.cfg.PerIP {
		for o := range m.sessions {
			if o != s && o.ip == s.ip && o.state != Idle {
				s.pendingRun, s.pendingTerm = nil, nil
				s.c.SendJSON(map[string]any{"t": "error", "msg": "You already have a playground session open in another tab. Close it first, or wait for it to end."})
				return
			}
		}
	}
	if len(m.queue) == 0 {
		if slot := m.freeSlotLocked(); slot >= 0 {
			m.assignLocked(s, slot)
			m.broadcastLocked()
			return
		}
	}
	if len(m.queue) >= m.cfg.MaxQueue {
		s.pendingRun, s.pendingTerm = nil, nil
		s.c.SendJSON(map[string]any{"t": "error", "msg": "The playground is very busy right now and the queue is full. Please try again in a few minutes."})
		return
	}
	s.state = Waiting
	s.joined = m.now()
	m.queue = append(m.queue, s)
	m.updateDeadlinesLocked()
	m.broadcastLocked()
}

func (m *Manager) freeSlotLocked() int {
	for i, o := range m.slots {
		if o == nil && !m.tearing[i] {
			return i
		}
	}
	return -1
}

func (m *Manager) assignLocked(s *Session, slot int) {
	now := m.now()
	m.slots[slot] = s
	s.state = Booting
	s.slot = slot
	s.leaseStart = now
	s.lastActive = now
	s.warned = false
	s.restarts = 0
	log.Printf("slot %d: session start (ip %s, waited %s)", slot, s.ip, waitedFor(s, now))
	m.bootLocked(s)
}

func waitedFor(s *Session, now time.Time) time.Duration {
	if s.joined.IsZero() {
		return 0
	}
	return now.Sub(s.joined).Round(time.Second)
}

func (m *Manager) bootLocked(s *Session) {
	s.gen++
	gen, slot := s.gen, s.slot
	cancel := make(chan struct{})
	s.cancelBoot = cancel
	s.c.SendJSON(map[string]any{"t": "booting"})
	go m.boot(s, slot, gen, cancel)
}

func (m *Manager) boot(s *Session, slot, gen int, cancel chan struct{}) {
	t0 := time.Now()
	m.slotOps[slot].Lock()
	sock, err := m.l.Start(slot)
	m.slotOps[slot].Unlock()

	var conn net.Conn
	var r *bufio.Reader
	var hello Hello
	if err == nil {
		conn, r, hello, err = m.waitAgent(sock, m.cfg.BootTimeout, cancel)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if s.gen != gen || s.slot != slot || s.state != Booting {
		if conn != nil {
			conn.Close()
		}
		return // session ended or rebooted meanwhile; teardown handles the slot
	}
	if err != nil {
		log.Printf("slot %d: boot failed: %v", slot, err)
		m.endLocked(s, "Sorry, the VM failed to start. Please try again.")
		return
	}
	log.Printf("slot %d: VM ready in %s (kernel %s)", slot, time.Since(t0).Round(time.Millisecond), hello.Kernel)
	vm := &vmConn{conn: conn, r: r, sock: sock}
	s.vm = vm
	s.hello = hello
	s.state = Ready
	s.c.SendJSON(map[string]any{"t": "ready", "hello": hello, "deadline_ms": msOrZero(s.deadline)})
	go m.readCtrl(s, vm)
	if s.pendingTerm != nil {
		t := *s.pendingTerm
		s.pendingTerm = nil
		m.openTermLocked(s, t[0], t[1])
	}
	if s.pendingRun != nil {
		code := *s.pendingRun
		s.pendingRun = nil
		m.startRunLocked(s, code)
	}
}

func (m *Manager) startRunLocked(s *Session, code string) {
	s.runID++
	s.runUntil = m.now().Add(m.cfg.RunTimeout + 15*time.Second)
	err := s.vm.send(map[string]any{
		"op": "run", "id": s.runID, "code": code, "timeout": m.cfg.RunTimeout.Seconds(),
	})
	if err != nil {
		m.vmDiedLocked(s, "Lost connection to the VM.")
	}
}

// readCtrl forwards agent messages to the browser and watches for VM death.
func (m *Manager) readCtrl(s *Session, vm *vmConn) {
	for {
		line, err := vm.r.ReadBytes('\n')
		if err != nil {
			break
		}
		// Hot path: output/IR lines are forwarded as-is, without JSON decoding.
		// (The agent always writes the "t" key first, via json.dumps.)
		if bytes.HasPrefix(line, []byte(`{"t": "out"`)) || bytes.HasPrefix(line, []byte(`{"t": "ir"`)) {
			s.c.SendText(line)
			continue
		}
		var head struct {
			T  string `json:"t"`
			ID int    `json:"id"`
		}
		if json.Unmarshal(line, &head) != nil {
			continue
		}
		switch head.T {
		case "out", "ir", "start":
			s.c.SendText(line)
		case "exit":
			m.mu.Lock()
			if s.vm == vm && head.ID == s.runID {
				s.runUntil = time.Time{}
			}
			m.mu.Unlock()
			s.c.SendText(line)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.vm == vm && s.state == Ready {
		m.vmDiedLocked(s, "The VM crashed (a kernel panic, perhaps?).")
	}
}

func (m *Manager) vmDiedLocked(s *Session, why string) {
	m.closeConnsLocked(s)
	if !s.runUntil.IsZero() {
		s.c.SendJSON(map[string]any{"t": "exit", "id": s.runID, "code": nil, "reason": why})
		s.runUntil = time.Time{}
	}
	if s.restarts >= m.cfg.MaxRestarts {
		m.endLocked(s, why+" It kept crashing, so the session was ended.")
		return
	}
	s.restarts++
	s.state = Booting
	s.c.SendJSON(map[string]any{"t": "vm_restart", "reason": why})
	m.bootLocked(s) // Start() kills the old VM in this slot first
}

func (m *Manager) openTermLocked(s *Session, cols, rows int) {
	if s.term != nil {
		s.term.Close()
		s.term = nil
	}
	vm, gen := s.vm, s.gen
	go func() {
		c, err := m.dialPty(vm.sock)
		m.mu.Lock()
		defer m.mu.Unlock()
		if err != nil || s.vm != vm || s.gen != gen || s.state != Ready {
			if c != nil {
				c.Close()
			}
			if err != nil {
				s.c.SendJSON(map[string]any{"t": "term_closed"})
			}
			return
		}
		s.term = c
		c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		c.Write(resizeFrame(cols, rows))
		s.c.SendJSON(map[string]any{"t": "term_open"})
		go m.readTerm(s, c)
	}()
}

func (m *Manager) readTerm(s *Session, c net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			s.c.SendBinary(append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			break
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.term == c {
		s.term = nil
		s.c.SendJSON(map[string]any{"t": "term_closed"})
	}
}

func (m *Manager) closeConnsLocked(s *Session) {
	if s.cancelBoot != nil {
		close(s.cancelBoot)
		s.cancelBoot = nil
	}
	if s.term != nil {
		s.term.Close()
		s.term = nil
	}
	if s.vm != nil {
		s.vm.conn.Close()
		s.vm = nil
	}
}

// endLocked ends whatever the session holds (queue place or VM) and lets the
// next person in. reason == "" means the browser is gone; nothing is sent.
func (m *Manager) endLocked(s *Session, reason string) {
	now := m.now()
	switch s.state {
	case Idle:
		return
	case Waiting:
		for i, o := range m.queue {
			if o == s {
				m.queue = append(m.queue[:i], m.queue[i+1:]...)
				break
			}
		}
	case Booting, Ready:
		slot := s.slot
		m.closeConnsLocked(s)
		s.gen++
		m.slots[slot] = nil
		m.tearing[slot] = true
		dur := now.Sub(s.leaseStart)
		m.avg = (m.avg*4 + dur) / 5
		log.Printf("slot %d: session end after %s (%s)", slot, dur.Round(time.Second), orStr(reason, "disconnected"))
		go m.teardown(slot)
	}
	s.state = Idle
	s.slot = -1
	s.deadline = time.Time{}
	s.runUntil = time.Time{}
	s.pendingRun, s.pendingTerm = nil, nil
	if reason != "" {
		s.c.SendJSON(map[string]any{"t": "ended", "reason": reason})
	}
	m.promoteLocked()
	m.updateDeadlinesLocked()
	m.broadcastLocked()
}

func (m *Manager) teardown(slot int) {
	m.slotOps[slot].Lock()
	if err := m.l.Stop(slot); err != nil {
		log.Printf("slot %d: stop failed: %v", slot, err)
	}
	m.slotOps[slot].Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tearing[slot] = false
	m.promoteLocked()
	m.updateDeadlinesLocked()
	m.broadcastLocked()
}

func (m *Manager) promoteLocked() {
	for len(m.queue) > 0 {
		slot := m.freeSlotLocked()
		if slot < 0 {
			return
		}
		s := m.queue[0]
		m.queue = m.queue[1:]
		m.assignLocked(s, slot)
	}
}

// updateDeadlinesLocked: the hard cap only applies while someone is waiting,
// and nobody gets less than CapGrace notice.
func (m *Manager) updateDeadlinesLocked() {
	now := m.now()
	for _, s := range m.slots {
		if s == nil {
			continue
		}
		old := s.deadline
		if len(m.queue) == 0 {
			s.deadline = time.Time{}
		} else if s.deadline.IsZero() {
			d := s.leaseStart.Add(m.cfg.HardCap)
			if g := now.Add(m.cfg.CapGrace); d.Before(g) {
				d = g
			}
			s.deadline = d
		}
		if !old.Equal(s.deadline) {
			s.c.SendJSON(map[string]any{"t": "lease", "deadline_ms": msOrZero(s.deadline)})
		}
	}
}

// etasLocked estimates the wait for each queue position.
func (m *Manager) etasLocked() []time.Duration {
	now := m.now()
	ends := make([]time.Time, 0, len(m.slots))
	for i, s := range m.slots {
		var e time.Time
		switch {
		case s == nil && m.tearing[i]:
			e = now.Add(3 * time.Second)
		case s == nil:
			e = now
		default:
			e = s.leaseStart.Add(m.avg)
			if !s.deadline.IsZero() && s.deadline.Before(e) {
				e = s.deadline
			}
			if lo := now.Add(15 * time.Second); e.Before(lo) {
				e = lo
			}
		}
		ends = append(ends, e)
	}
	etas := make([]time.Duration, len(m.queue))
	for i := range m.queue {
		sort.Slice(ends, func(a, b int) bool { return ends[a].Before(ends[b]) })
		etas[i] = ends[0].Sub(now)
		ends[0] = ends[0].Add(m.avg)
	}
	return etas
}

func (m *Manager) statusLocked() map[string]any {
	busy := 0
	for i, s := range m.slots {
		if s != nil || m.tearing[i] {
			busy++
		}
	}
	return map[string]any{"t": "status", "busy": busy, "slots": len(m.slots), "waiting": len(m.queue)}
}

// broadcastLocked tells everyone about a changed status (only if it changed)
// and waiting sessions about their position.
func (m *Manager) broadcastLocked() {
	st := m.statusLocked()
	key := [3]int{st["busy"].(int), st["slots"].(int), st["waiting"].(int)}
	if key != m.lastStat {
		m.lastStat = key
		b, _ := json.Marshal(st) // encode once, send to all
		for s := range m.sessions {
			s.c.SendText(b)
		}
	}
	m.sendQueueLocked()
}

func (m *Manager) sendQueueLocked() {
	for i, eta := range m.etasLocked() {
		m.queue[i].c.SendJSON(map[string]any{
			"t": "queued", "position": i + 1, "eta_s": int(eta.Seconds()),
		})
	}
}

// ---------------------------------------------------------------------------

func ptyFrame(typ byte, payload []byte) []byte {
	b := make([]byte, 3+len(payload))
	b[0] = typ
	binary.BigEndian.PutUint16(b[1:3], uint16(len(payload)))
	copy(b[3:], payload)
	return b
}

func resizeFrame(cols, rows int) []byte {
	p := make([]byte, 4)
	binary.BigEndian.PutUint16(p[0:2], uint16(clamp(cols, 10, 500)))
	binary.BigEndian.PutUint16(p[2:4], uint16(clamp(rows, 2, 200)))
	return ptyFrame(1, p)
}

func clamp(v, lo, hi int) int { return max(lo, min(hi, v)) }

func msOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
