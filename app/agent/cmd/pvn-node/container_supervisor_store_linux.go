//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ayflying/pvn/pkg/selfupdate"
)

// containerConfigPath preserves CLI precedence, then LANET_CONFIG, then the
// container volume default. updateHelperConfigPath remains the CLI parser.
func containerConfigPath(exe string, args []string, envConfig string) string {
	for _, arg := range args {
		if arg == "-config" || arg == "--config" || strings.HasPrefix(arg, "-config=") || strings.HasPrefix(arg, "--config=") {
			return updateHelperConfigPath(exe, args)
		}
	}
	if envConfig != "" {
		return envConfig
	}
	return "/data/lanet.json"
}

func containerRuntimeDir() string {
	if dir := os.Getenv("LANET_CONTAINER_RUNTIME"); dir != "" && os.Getenv("LANET_CONTAINER_CHILD") == "1" {
		return dir
	}
	if !containerized() {
		return ""
	}
	return filepath.Join(filepath.Dir(containerConfigPath(selfExe(), os.Args[1:], os.Getenv("LANET_CONFIG"))), "runtime")
}

type containerSupervisorSource struct {
	Path     string
	Manifest *selfupdate.Manifest // nil is the image binary
}

type containerSupervisorFailure struct {
	Reason   string    `json:"reason"`
	FailedAt time.Time `json:"failed_at"`
}

// containerSupervisorValidate uses an explicit key/platform to allow tests to
// generate their own signing key. The production caller always pins ReleasePublicKey.
func containerSupervisorValidate(dir string, m selfupdate.Manifest, publicKey, platform string) (string, error) {
	if !m.Verify(publicKey) {
		return "", errors.New("invalid release signature")
	}
	if m.Platform != platform {
		return "", fmt.Errorf("platform %q does not match %q", m.Platform, platform)
	}
	hash, err := hex.DecodeString(m.SHA256)
	if err != nil || len(hash) != sha256.Size || m.SHA256 != strings.ToLower(m.SHA256) {
		return "", errors.New("invalid sha256")
	}
	if m.Size <= 0 {
		return "", errors.New("invalid binary size")
	}
	slot := filepath.Join(dir, m.SHA256)
	if info, err := os.Lstat(slot); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("runtime slot must be a real directory")
	}
	path := filepath.Join(slot, "pvn-node")
	var stored selfupdate.Manifest
	if err := containerSupervisorReadJSON(filepath.Join(dir, m.SHA256, "manifest.json"), &stored); err != nil {
		return "", err
	}
	if stored != m {
		return "", errors.New("content manifest differs from pointer")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() != m.Size || info.Mode().Perm()&0111 == 0 {
		return "", errors.New("binary must be a regular executable of signed size")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	digest := sha256.New()
	n, err := io.Copy(digest, f)
	if err != nil {
		return "", err
	}
	if n != m.Size || hex.EncodeToString(digest.Sum(nil)) != m.SHA256 {
		return "", errors.New("binary sha256 mismatch")
	}
	return path, nil
}

func containerSupervisorReadJSON(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	// Manifests/failure ledgers are small. Bound corrupt or hostile volume data.
	dec := json.NewDecoder(io.LimitReader(f, 1<<20))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

// Atomic publication: readers never observe half a manifest. Sync both the
// temporary file and directory so a confirmed active pointer survives power loss.
func containerSupervisorWriteJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".container-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func containerSupervisorNewer(m selfupdate.Manifest, imageVersion string, active *selfupdate.Manifest) bool {
	if !versionLess(imageVersion, m.Version) {
		return false
	}
	return active == nil || versionLess(active.Version, m.Version)
}

func containerSupervisorChildEnv(env []string, dir, ready, config string) []string {
	out := make([]string, 0, len(env)+4)
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if key == "LANET_CONTAINER_CHILD" || key == "LANET_CONTAINER_RUNTIME" || key == "LANET_CONTAINER_READY" || key == "LANET_CONFIG" {
			continue
		}
		out = append(out, item)
	}
	return append(out, "LANET_CONTAINER_CHILD=1", "LANET_CONTAINER_RUNTIME="+dir, "LANET_CONTAINER_READY="+ready, "LANET_CONFIG="+config)
}
