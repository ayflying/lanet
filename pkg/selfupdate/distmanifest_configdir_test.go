package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDistManifestWithSeparateConfigDirectory(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	configDir := filepath.Join(root, "config")
	for _, dir := range []string{binDir, configDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	exe, manifest, key := makeSignedExe(t, binDir, "lanet.exe", "0.5.87")
	if err := saveManifest(filepath.Join(binDir, DistManifestName), manifest); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{cfg: Config{ExePath: exe, ManifestPath: filepath.Join(configDir, "update-manifest.json"), PublicKey: key}}
	if got := c.loadSelfManifest(); got == nil || got.Version != "0.5.87" {
		t.Fatalf("separate config directory hides valid distribution credential: %+v", got)
	}
	if err := os.WriteFile(exe, []byte("tampered binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if got := c.loadSelfManifest(); got != nil {
		t.Fatal("fallback must still validate binary hash")
	}
}
