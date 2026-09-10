//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHasServiceArg(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"-service"}, true},
		{[]string{"--SERVICE"}, true},
		{[]string{"-config", "lanet.json"}, false},
	} {
		if got := hasServiceArg(tc.args); got != tc.want {
			t.Fatalf("hasServiceArg(%v)=%v want %v", tc.args, got, tc.want)
		}
	}
}

func TestServiceConfigPath(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()

	t.Run("explicit equals", func(t *testing.T) {
		os.Args = []string{"lanet.exe", "-config=relative/lanet.json"}
		got := serviceConfigPath()
		if !filepath.IsAbs(got) || !strings.HasSuffix(filepath.ToSlash(got), "relative/lanet.json") {
			t.Fatalf("serviceConfigPath=%q", got)
		}
	})

	t.Run("explicit separate", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "custom.json")
		os.Args = []string{"lanet.exe", "-config", want}
		if got := serviceConfigPath(); got != want {
			t.Fatalf("serviceConfigPath=%q want %q", got, want)
		}
	})
}

func TestServiceBinaryPathQuotesPaths(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()
	config := filepath.Join(t.TempDir(), "with space", "lanet.json")
	os.Args = []string{"lanet.exe", "-config", config}
	got := serviceBinaryPath()
	if !strings.Contains(got, `" -service -config "`) || !strings.HasSuffix(got, `"`) {
		t.Fatalf("服务命令行未正确引用路径: %q", got)
	}
}
