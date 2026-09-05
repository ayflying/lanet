package lanet

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadOrCreateIdentity 身份密钥：首次生成、复用一致。
func TestLoadOrCreateIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.key")

	k1, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("首次生成: %v", err)
	}
	raw1, _ := k1.Raw()
	k2, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("二次加载: %v", err)
	}
	raw2, _ := k2.Raw()
	if string(raw1) != string(raw2) {
		t.Fatal("同一文件两次加载的密钥必须一致（PeerID/虚拟 IP 稳定的前提）")
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		t.Fatalf("密钥文件未落盘: %v", err)
	}
}

// TestResolveVirtualIP_Empty 空目标报错。
func TestResolveVirtualIP_Empty(t *testing.T) {
	c := &Client{}
	if _, err := c.resolveVirtualIP("  "); err == nil {
		t.Fatal("空目标必须报错")
	}
}
