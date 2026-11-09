//go:build linux

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ayflying/pvn/pkg/selfupdate"
)

// All signing material is generated in memory for this test. Production release
// keys, production paths and production verification policy are never modified.
type supervisorTestSlot struct {
	dir, path, publicKey string
	privateKey           ed25519.PrivateKey
	manifest             selfupdate.Manifest
}

func newSupervisorTestSlot(t *testing.T) supervisorTestSlot {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("test-only executable fixture\n")
	digest := sha256.Sum256(body)
	s := supervisorTestSlot{
		dir: t.TempDir(), publicKey: base64.StdEncoding.EncodeToString(pub), privateKey: priv,
		manifest: selfupdate.Manifest{Version: "9.8.7", Platform: "linux/" + runtime.GOARCH, Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:])},
	}
	s.sign(t)
	s.path = filepath.Join(s.dir, s.manifest.SHA256, "pvn-node")
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, body, 0700); err != nil {
		t.Fatal(err)
	}
	s.store(t, s.manifest)
	return s
}

func (s *supervisorTestSlot) sign(t *testing.T) {
	t.Helper()
	if err := selfupdate.SignManifest(s.privateKey, &s.manifest); err != nil {
		t.Fatal(err)
	}
}

func (s supervisorTestSlot) store(t *testing.T, m selfupdate.Manifest) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(s.path), "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestContainerSupervisorValidateSignedSlot(t *testing.T) {
	s := newSupervisorTestSlot(t)
	got, err := containerSupervisorValidate(s.dir, s.manifest, s.publicKey, s.manifest.Platform)
	if err != nil || got != s.path {
		t.Fatalf("validate signed slot = %q, %v; want %q", got, err, s.path)
	}
}

func TestContainerSupervisorValidateRejects(t *testing.T) {
	tests := []struct {
		name string
		edit func(*testing.T, *supervisorTestSlot)
	}{
		{"unsigned", func(t *testing.T, s *supervisorTestSlot) { s.manifest.Signature = "" }},
		{"tampered signed version", func(t *testing.T, s *supervisorTestSlot) { s.manifest.Version = "9.8.8" }},
		{"wrong public key", func(t *testing.T, s *supervisorTestSlot) {
			pub, _, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			s.publicKey = base64.StdEncoding.EncodeToString(pub)
		}},
		{"malformed public key", func(t *testing.T, s *supervisorTestSlot) { s.publicKey = "invalid" }},
		{"signed wrong platform", func(t *testing.T, s *supervisorTestSlot) {
			s.manifest.Platform = "windows/amd64"
			s.sign(t)
			s.store(t, s.manifest)
		}},
		{"signed zero size", func(t *testing.T, s *supervisorTestSlot) {
			s.manifest.Size = 0
			s.sign(t)
			s.store(t, s.manifest)
		}},
		{"signed negative size", func(t *testing.T, s *supervisorTestSlot) {
			s.manifest.Size = -1
			s.sign(t)
			s.store(t, s.manifest)
		}},
		{"signed incorrect size", func(t *testing.T, s *supervisorTestSlot) {
			s.manifest.Size++
			s.sign(t)
			s.store(t, s.manifest)
		}},
		{"same size corrupt binary", func(t *testing.T, s *supervisorTestSlot) {
			body := []byte(strings.Repeat("x", int(s.manifest.Size)))
			if err := os.WriteFile(s.path, body, 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"truncated binary", func(t *testing.T, s *supervisorTestSlot) {
			if err := os.Truncate(s.path, s.manifest.Size-1); err != nil {
				t.Fatal(err)
			}
		}},
		{"stored manifest differs", func(t *testing.T, s *supervisorTestSlot) {
			m := s.manifest
			m.Version = "0.0.1"
			s.store(t, m)
		}},
		{"non executable", func(t *testing.T, s *supervisorTestSlot) {
			if err := os.Chmod(s.path, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"slot symlink", func(t *testing.T, s *supervisorTestSlot) {
			slot := filepath.Dir(s.path)
			target := filepath.Join(s.dir, "outside-slot")
			if err := os.Rename(slot, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, slot); err != nil {
				t.Fatal(err)
			}
		}},
		{"binary symlink", func(t *testing.T, s *supervisorTestSlot) {
			target := filepath.Join(s.dir, "outside-binary")
			if err := os.Rename(s.path, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, s.path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSupervisorTestSlot(t)
			tc.edit(t, &s)
			if got, err := containerSupervisorValidate(s.dir, s.manifest, s.publicKey, "linux/"+runtime.GOARCH); err == nil {
				t.Fatalf("accepted invalid slot at %q", got)
			}
		})
	}
}

func TestContainerSupervisorValidateRejectsSignedUnsafeDigest(t *testing.T) {
	for _, digest := range []string{
		"../escape", "../../escape", "/tmp/escape", `..\escape`,
		strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64), strings.Repeat("A", 64),
	} {
		t.Run(digest, func(t *testing.T) {
			s := newSupervisorTestSlot(t)
			s.manifest.SHA256 = digest
			s.sign(t) // Valid signature must not make an unsafe content path acceptable.
			if !s.manifest.Verify(s.publicKey) {
				t.Fatal("fixture signature invalid")
			}
			if _, err := containerSupervisorValidate(s.dir, s.manifest, s.publicKey, s.manifest.Platform); err == nil || err.Error() != "invalid sha256" {
				t.Fatalf("unsafe digest rejected too late or accepted: %v", err)
			}
		})
	}
}

func TestContainerSupervisorChildEnv(t *testing.T) {
	input := []string{"PATH=/test/bin", "KEEP=a=b", "LANET_CONTAINER_CHILD=0", "LANET_CONFIG=old", "LANET_CONFIG=duplicate", "LANET_CONTAINER_RUNTIME=old", "LANET_CONTAINER_READY=old", "LANET_CONFIG_EXTRA=keep"}
	before := append([]string(nil), input...)
	got := containerSupervisorChildEnv(input, "/test/runtime", "/test/ready", "/test/config.json")
	want := []string{"PATH=/test/bin", "KEEP=a=b", "LANET_CONFIG_EXTRA=keep", "LANET_CONTAINER_CHILD=1", "LANET_CONTAINER_RUNTIME=/test/runtime", "LANET_CONTAINER_READY=/test/ready", "LANET_CONFIG=/test/config.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %#v; want %#v", got, want)
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatal("input environment mutated")
	}
}

func TestContainerSupervisorConfigPath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		env, want string
	}{
		{"default", nil, "", "/data/lanet.json"},
		{"environment", nil, "/volume/custom.json", "/volume/custom.json"},
		{"single dash", []string{"-config", "/cli/a.json"}, "/env.json", "/cli/a.json"},
		{"double dash", []string{"--config", "/cli/b.json"}, "/env.json", "/cli/b.json"},
		{"single equals", []string{"-config=/cli/c.json"}, "/env.json", "/cli/c.json"},
		{"double equals", []string{"--config=/cli/d.json"}, "/env.json", "/cli/d.json"},
		{"unrelated flags", []string{"--debug", "--config-extra=no"}, "/env.json", "/env.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := containerConfigPath("/image/pvn-node", tc.args, tc.env); got != tc.want {
				t.Fatalf("config = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestContainerSupervisorNewer(t *testing.T) {
	for _, tc := range []struct {
		name, candidate, image, active string
		want                           bool
	}{
		{"newer than image", "1.2.4", "1.2.3", "", true},
		{"equal image", "1.2.3", "1.2.3", "", false},
		{"older image", "1.2.2", "1.2.3", "", false},
		{"newer than both", "1.10.0", "1.2.3", "1.9.9", true},
		{"equal active", "1.9.9", "1.2.3", "1.9.9", false},
		{"older active", "1.3.0", "1.2.3", "1.9.9", false},
		{"must exceed image too", "1.3.0", "2.0.0", "1.2.0", false},
		{"version prefix", "v2.0.0", "1.9.9", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var active *selfupdate.Manifest
			if tc.active != "" {
				active = &selfupdate.Manifest{Version: tc.active}
			}
			if got := containerSupervisorNewer(selfupdate.Manifest{Version: tc.candidate}, tc.image, active); got != tc.want {
				t.Fatalf("newer = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestContainerSupervisorBackoff(t *testing.T) {
	for _, tc := range []struct {
		failures int
		seconds  time.Duration
	}{{-1, 1}, {0, 1}, {1, 1}, {2, 2}, {3, 4}, {4, 8}, {5, 16}, {6, 30}, {7, 30}, {1000, 30}} {
		if got := containerSupervisorBackoff(tc.failures); got != tc.seconds*time.Second {
			t.Errorf("backoff(%d) = %s; want %s", tc.failures, got, tc.seconds*time.Second)
		}
	}
}

// The only executed child is this test binary, restricted to this helper test.
// It never enters the application main function or contacts any service.
func TestContainerSupervisorHelperProcess(t *testing.T) {
	switch os.Getenv("LANET_SUPERVISOR_TEST_PROCESS") {
	case "exit":
		os.Exit(23)
	case "no-ready":
		for {
			time.Sleep(time.Hour)
		}
	case "wrong-pid":
		if err := containerSupervisorWriteJSON(os.Getenv("LANET_CONTAINER_READY"), struct {
			PID int `json:"pid"`
		}{os.Getpid() + 1}); err != nil {
			os.Exit(25)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "ready-exit":
		signalContainerReady()
		os.Exit(24)
	case "ready":
		signalContainerReady()
		for {
			time.Sleep(time.Hour)
		}
	}
}

func TestContainerSupervisorChildLifecycle(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"exit", "no-ready", "wrong-pid", "ready-exit", "ready"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("LANET_SUPERVISOR_TEST_PROCESS", mode)
			t.Setenv("LANET_CONFIG", filepath.Join(t.TempDir(), "unused.json"))
			dir := t.TempDir()
			signals := make(chan os.Signal, 1)
			opts := containerSupervisorOptions{ReadyTimeout: 600 * time.Millisecond, StableWindow: 120 * time.Millisecond, StopTimeout: time.Second, PollInterval: 10 * time.Millisecond}
			// A watchdog bounds regressions and still lets RunChild reap its child.
			watchdog := time.AfterFunc(5*time.Second, func() {
				select {
				case signals <- syscall.SIGTERM:
				default:
				}
			})
			defer watchdog.Stop()
			confirmed := 0
			start := time.Now()
			result := containerSupervisorRunChild(dir, containerSupervisorSource{Path: exe}, []string{"-test.run=^TestContainerSupervisorHelperProcess$"}, true, signals, opts, func() error {
				confirmed++
				if elapsed := time.Since(start); elapsed < opts.StableWindow {
					t.Errorf("confirmed before stable window: %s", elapsed)
				}
				signals <- syscall.SIGTERM
				return nil
			})
			switch mode {
			case "exit", "ready-exit":
				wantCode := 23
				if mode == "ready-exit" {
					wantCode = 24
				}
				if result.Stopped || !result.FailedTrial || result.ExitCode != wantCode || confirmed != 0 {
					t.Fatalf("early exit result = %+v; confirmations = %d", result, confirmed)
				}
			case "no-ready", "wrong-pid":
				if result.Stopped || !result.FailedTrial || result.Reason != "ready timeout" || confirmed != 0 {
					t.Fatalf("timeout result = %+v; confirmations = %d", result, confirmed)
				}
				if time.Since(start) < opts.ReadyTimeout {
					t.Fatal("ready timed out prematurely")
				}
			case "ready":
				if !result.Stopped || result.FailedTrial || confirmed != 1 {
					t.Fatalf("stable ready result = %+v; confirmations = %d", result, confirmed)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".ready-") {
					t.Errorf("readiness file leaked: %s", entry.Name())
				}
			}
		})
	}
}
