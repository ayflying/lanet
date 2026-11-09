//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/selfupdate"
)

// These flow tests execute only copies of the Go test executable, with an exact
// helper-test selector. Signing keys are generated in memory by the existing
// fixture; no application main, production key or verification bypass is used.
type supervisorFlowFixture struct {
	t      *testing.T
	dir    string
	events string
	binary []byte
	signer supervisorTestSlot
	image  string
	opts   containerSupervisorOptions
}

type supervisorFlowEvent struct {
	Slot   string `json:"slot"`
	Kind   string `json:"kind"`
	PID    int    `json:"pid"`
	Signal string `json:"signal,omitempty"`
}

// TestContainerSupervisorFlowHelper is inert in a normal test run. The marker is
// an argument, not an environment-controlled signature-verification exception.
func TestContainerSupervisorFlowHelper(t *testing.T) {
	marker := -1
	for i, arg := range os.Args {
		if arg == "--supervisor-flow-helper" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return
	}
	if marker+1 >= len(os.Args) {
		os.Exit(90)
	}
	exe, err := os.Executable()
	if err != nil {
		os.Exit(91)
	}
	events := os.Args[marker+1]
	slot := filepath.Base(filepath.Dir(exe))
	emit := func(kind, sig string) {
		event := supervisorFlowEvent{Slot: slot, Kind: kind, PID: os.Getpid(), Signal: sig}
		name := fmt.Sprintf("%d-%s.json", os.Getpid(), kind)
		if err := containerSupervisorWriteJSON(filepath.Join(events, name), event); err != nil {
			os.Exit(92)
		}
	}
	mode, err := os.ReadFile(exe + ".flow-mode")
	if err != nil {
		os.Exit(93)
	}
	emit("start", "")
	if string(mode) == "exit" {
		os.Exit(23)
	}
	if string(mode) != "ready" || os.Getenv("LANET_CONTAINER_CHILD") != "1" {
		os.Exit(94)
	}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGTERM, syscall.SIGINT)
	signalContainerReady()
	emit("ready", "")
	sig := <-stopped
	emit("stop", sig.String())
	os.Exit(0)
}

func newSupervisorFlowFixture(t *testing.T) *supervisorFlowFixture {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	f := &supervisorFlowFixture{
		t: t, dir: t.TempDir(), events: t.TempDir(), binary: body,
		signer: newSupervisorTestSlot(t),
	}
	f.opts = containerSupervisorOptions{
		ReadyTimeout: 2 * time.Second, StableWindow: 300 * time.Millisecond,
		StopTimeout: time.Second, PollInterval: 10 * time.Millisecond,
		PublicKey: f.signer.publicKey,
	}
	// Keep even the image path test-owned; the application executable is never run.
	imageDir := filepath.Join(t.TempDir(), "image")
	if err := os.MkdirAll(imageDir, 0700); err != nil {
		t.Fatal(err)
	}
	f.image = filepath.Join(imageDir, "pvn-node")
	f.writeExecutable(f.image, "image", "ready")
	return f
}

func (f *supervisorFlowFixture) writeExecutable(path, identity, mode string) []byte {
	f.t.Helper()
	// ELF permits trailing data. Unique tails make independently signed,
	// content-addressed slots while retaining a real executable test helper.
	body := append(append([]byte(nil), f.binary...), []byte("\nflow-fixture:"+identity+"\n")...)
	if err := os.WriteFile(path, body, 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path+".flow-mode", []byte(mode), 0600); err != nil {
		f.t.Fatal(err)
	}
	return body
}

func (f *supervisorFlowFixture) slot(version, mode string) selfupdate.Manifest {
	f.t.Helper()
	body := append(append([]byte(nil), f.binary...), []byte("\nflow-fixture:"+version+"\n")...)
	digest := sha256.Sum256(body)
	s := f.signer
	s.dir = f.dir
	s.manifest = selfupdate.Manifest{
		Version: version, Platform: s.manifest.Platform,
		Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:]),
	}
	s.sign(f.t)
	s.path = filepath.Join(f.dir, s.manifest.SHA256, "pvn-node")
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		f.t.Fatal(err)
	}
	f.writeExecutable(s.path, version, mode)
	s.store(f.t, s.manifest)
	if _, err := containerSupervisorValidate(f.dir, s.manifest, f.opts.PublicKey, s.manifest.Platform); err != nil {
		f.t.Fatalf("invalid signed executable fixture: %v", err)
	}
	return s.manifest
}

func (f *supervisorFlowFixture) publish(name string, value any) {
	f.t.Helper()
	if err := containerSupervisorWriteJSON(filepath.Join(f.dir, name+".json"), value); err != nil {
		f.t.Fatal(err)
	}
}

func (f *supervisorFlowFixture) wait(what string, condition func() bool) {
	f.t.Helper()
	deadline := time.NewTimer(6 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			f.t.Fatalf("timed out waiting for %s; events=%+v", what, f.readEvents())
		case <-tick.C:
		}
	}
}

func (f *supervisorFlowFixture) readEvents() []supervisorFlowEvent {
	f.t.Helper()
	paths, err := filepath.Glob(filepath.Join(f.events, "*.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	var events []supervisorFlowEvent
	for _, path := range paths {
		var event supervisorFlowEvent
		if err := containerSupervisorReadJSON(path, &event); err != nil {
			f.t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func (f *supervisorFlowFixture) count(slot, kind string) int {
	f.t.Helper()
	n := 0
	for _, event := range f.readEvents() {
		if event.Slot == slot && event.Kind == kind {
			n++
		}
	}
	return n
}

func (f *supervisorFlowFixture) pointer(name string, want selfupdate.Manifest) bool {
	var got selfupdate.Manifest
	return containerSupervisorReadJSON(filepath.Join(f.dir, name+".json"), &got) == nil && got == want
}

func (f *supervisorFlowFixture) failure(m selfupdate.Manifest) (containerSupervisorFailure, bool) {
	var ledger map[string]containerSupervisorFailure
	if err := containerSupervisorReadJSON(filepath.Join(f.dir, "failed.json"), &ledger); err != nil {
		return containerSupervisorFailure{}, false
	}
	failure, ok := ledger[m.SHA256]
	return failure, ok
}

// Each start uses fresh channels and a fresh Loop invocation, sharing only the
// runtime volume. Cleanup always sends SIGTERM and joins the loop, even on Fatal.
func (f *supervisorFlowFixture) start(opts containerSupervisorOptions) func() {
	f.t.Helper()
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		containerSupervisorLoop(f.dir, f.image, "1.0.0", []string{
			"-test.run=^TestContainerSupervisorFlowHelper$", "--", "--supervisor-flow-helper", f.events,
		}, signals, opts)
	}()
	stopped := false
	stop := func() {
		f.t.Helper()
		if stopped {
			return
		}
		stopped = true
		signals <- syscall.SIGTERM
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			f.t.Fatal("supervisor loop did not stop after SIGTERM")
		}
	}
	f.t.Cleanup(stop)
	return stop
}

func (f *supervisorFlowFixture) assertTerminated(slot string, starts int) {
	f.t.Helper()
	if got := f.count(slot, "stop"); got != starts {
		f.t.Fatalf("slot %s TERM acknowledgements = %d, want %d", slot, got, starts)
	}
	for _, event := range f.readEvents() {
		if event.Slot == slot && event.Kind == "stop" {
			if event.Signal != syscall.SIGTERM.String() {
				f.t.Fatalf("child received %q, want SIGTERM", event.Signal)
			}
			if err := syscall.Kill(event.PID, 0); err != syscall.ESRCH {
				f.t.Fatalf("child %d not reaped: %v", event.PID, err)
			}
		}
	}
}

func TestContainerSupervisorLoopPromotesAndRecreates(t *testing.T) {
	f := newSupervisorFlowFixture(t)
	candidate := f.slot("2.0.0", "ready")
	f.publish("candidate", candidate)
	stop := f.start(f.opts)
	f.wait("candidate readiness", func() bool { return f.count(candidate.SHA256, "ready") == 1 })
	if _, err := os.Stat(filepath.Join(f.dir, "active.json")); !os.IsNotExist(err) {
		t.Fatalf("active published before stable window: %v", err)
	}
	f.wait("stable candidate promotion", func() bool { return f.pointer("active", candidate) })
	f.wait("candidate pointer cleared", func() bool {
		_, err := os.Stat(filepath.Join(f.dir, "candidate.json"))
		return os.IsNotExist(err)
	})
	if _, bad := f.failure(candidate); bad {
		t.Fatal("confirmed candidate remains in failure ledger")
	}
	if f.count("image", "start") != 0 {
		t.Fatal("image started instead of higher signed candidate")
	}
	stop()
	f.assertTerminated(candidate.SHA256, 1)

	stop = f.start(f.opts) // Same old image version; persisted active must win.
	f.wait("active child after recreation", func() bool { return f.count(candidate.SHA256, "ready") == 2 })
	if !f.pointer("active", candidate) || f.count("image", "start") != 0 {
		t.Fatal("recreated loop downgraded the active release to the old image")
	}
	stop()
	f.assertTerminated(candidate.SHA256, 2)
}

func TestContainerSupervisorLoopFailedCandidateFallsBackWithoutRetry(t *testing.T) {
	f := newSupervisorFlowFixture(t)
	active := f.slot("2.0.0", "ready")
	bad := f.slot("9.0.0", "exit")
	f.publish("active", active)
	f.publish("candidate", bad)
	for run := 1; run <= 2; run++ {
		stop := f.start(f.opts)
		f.wait("active fallback #"+strconv.Itoa(run), func() bool { return f.count(active.SHA256, "ready") == run })
		failure, ok := f.failure(bad)
		if !ok || failure.FailedAt.IsZero() || !strings.Contains(failure.Reason, "23") {
			t.Fatalf("missing actual candidate exit failure: %+v, present=%v", failure, ok)
		}
		// A healthy fallback remains supervised rather than returning or cycling.
		time.Sleep(2 * f.opts.StableWindow)
		if f.count(bad.SHA256, "start") != 1 || f.count(active.SHA256, "start") != run || f.count("image", "start") != 0 {
			t.Fatalf("failed candidate retried or fallback cycled: %+v", f.readEvents())
		}
		if !f.pointer("active", active) || !f.pointer("candidate", bad) {
			t.Fatal("failed trial changed active or silently removed the candidate evidence")
		}
		stop()
		f.assertTerminated(active.SHA256, run)
	}
}

func TestContainerSupervisorLoopInterruptedTrialIsPersistentlyQuarantined(t *testing.T) {
	f := newSupervisorFlowFixture(t)
	active := f.slot("2.0.0", "ready")
	highest := f.slot("99.0.0", "ready")
	f.publish("active", active)
	f.publish("candidate", highest)
	opts := f.opts
	opts.StableWindow = 2 * time.Second
	stop := f.start(opts)
	f.wait("highest candidate in trial", func() bool { return f.count(highest.SHA256, "ready") == 1 })
	failure, ok := f.failure(highest)
	if !ok || failure.FailedAt.IsZero() || failure.Reason != "trial interrupted before confirmation" {
		t.Fatalf("trial intent not durable before execution: %+v, present=%v", failure, ok)
	}
	stop()
	f.assertTerminated(highest.SHA256, 1)
	if !f.pointer("active", active) {
		t.Fatal("interrupted trial displaced old active")
	}
	stop = f.start(f.opts)
	f.wait("old active after interrupted-trial recreation", func() bool { return f.count(active.SHA256, "ready") == 1 })
	time.Sleep(2 * f.opts.StableWindow)
	if f.count(highest.SHA256, "start") != 1 || !f.pointer("candidate", highest) {
		t.Fatal("recreated loop retried quarantined highest candidate or erased its pointer")
	}
	if got, ok := f.failure(highest); !ok || got != failure {
		t.Fatalf("persisted quarantine changed: %+v, present=%v", got, ok)
	}
	stop()
	f.assertTerminated(active.SHA256, 1)
}

func TestContainerSupervisorLoopLastGoodFallback(t *testing.T) {
	f := newSupervisorFlowFixture(t)
	previous := f.slot("2.0.0", "ready")
	newer := f.slot("3.0.0", "ready")
	f.publish("active", previous)
	f.publish("candidate", newer)
	stop := f.start(f.opts)
	f.wait("new active and saved last-good", func() bool {
		return f.pointer("active", newer) && f.pointer("last-good", previous)
	})
	stop()
	f.assertTerminated(newer.SHA256, 1)
	// Simulate damage to the active pointer, not to any production artifact.
	invalid := newer
	invalid.Signature = "invalid-test-signature"
	f.publish("active", invalid)
	stop = f.start(f.opts)
	f.wait("last-good child after recreation", func() bool { return f.count(previous.SHA256, "ready") == 1 })
	if f.count(newer.SHA256, "start") != 1 || f.count("image", "start") != 0 {
		t.Fatalf("invalid active did not fall back to signed last-good: %+v", f.readEvents())
	}
	if !f.pointer("last-good", previous) {
		t.Fatal("last-good pointer was lost")
	}
	stop()
	f.assertTerminated(previous.SHA256, 1)
}
