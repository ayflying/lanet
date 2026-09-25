package lanet

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ayflying/pvn/pkg/serverless"
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

func TestMemberVirtualAddress_PreservesIPv6Literal(t *testing.T) {
	member := serverless.MemberRef{
		VirtualIP:   "10.7.0.2",
		VirtualIPv6: "fd00:6c61:6e65::2",
	}
	for _, target := range []string{"fd00:6c61:6e65::2", " fd00:6c61:6e65:0:0:0:0:2 "} {
		if got := memberVirtualAddress(target, member); got != member.VirtualIPv6 {
			t.Errorf("memberVirtualAddress(%q) = %q, want IPv6 %q", target, got, member.VirtualIPv6)
		}
	}
	if got := memberVirtualAddress("peer-name", member); got != member.VirtualIP {
		t.Errorf("名称目标地址 = %q, want IPv4 %q", got, member.VirtualIP)
	}
	member.VirtualIPv6 = ""
	if got := memberVirtualAddress("fd00:6c61:6e65::2", member); got != "" {
		t.Errorf("缺少IPv6的成员返回地址 = %q, want empty", got)
	}
}
