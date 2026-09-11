package invitecode

import (
	"testing"

	ma "github.com/multiformats/go-multiaddr"
)

const (
	// validID 本机节点的真实 PeerID（合法 Ed25519 multihash，base58btc）。
	validID  = "12D3KooWJVB9uVFftoYFxpB8y78hYxRY92wKfnY1M9dqjRenGQ3p"
	multiStr = "/ip4/1.2.3.4/tcp/4001/p2p/" + validID
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	code := Encode(validID, "1.2.3.4:4001")
	want := "lanet://" + validID + "@1.2.3.4:4001"
	if code != want {
		t.Fatalf("Encode 出错：got %q want %q", code, want)
	}
	id, hostPort, err := Decode(code)
	if err != nil {
		t.Fatalf("Decode 失败: %v", err)
	}
	if id != validID {
		t.Fatalf("ID 不匹配: %q", id)
	}
	if hostPort != "1.2.3.4:4001" {
		t.Fatalf("hostPort 不匹配: %q", hostPort)
	}
	if !IsInviteCode(code) {
		t.Fatal("IsInviteCode 应为 true")
	}
}

func TestDecodeIdentityOnly(t *testing.T) {
	code := Encode(validID, "")
	id, hostPort, err := Decode(code)
	if err != nil {
		t.Fatalf("仅身份连接码 Decode 失败: %v", err)
	}
	if id != validID || hostPort != "" {
		t.Fatalf("got id=%q hostPort=%q", id, hostPort)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"12D3KooWJVB9uVFftoYFxpB8y78hYxRY92wKfnY1M9dqjRenGQ3p", // 裸 ID 不是连接码
		"lanet://not-a-valid-id@1.2.3.4:4001",
		"lanet://" + validID + "@not-an-ip",   // 地址段无端口
		"lanet://" + validID + "@:4001",       // 缺 IP
		"http://" + validID + "@1.2.3.4:4001", // 错 scheme
	}
	for _, c := range cases {
		if IsInviteCode(c) {
			t.Errorf("IsInviteCode(%q) 应为 false", c)
		}
	}
}

func TestEncodeFromMultiaddr(t *testing.T) {
	code := EncodeFromMultiaddr(multiStr)
	want := "lanet://" + validID + "@1.2.3.4:4001"
	if code != want {
		t.Fatalf("EncodeFromMultiaddr 出错：got %q want %q", code, want)
	}
	// 无 /p2p 段的 multiaddr 应返回空串。
	if got := EncodeFromMultiaddr("/ip4/1.2.3.4/tcp/4001"); got != "" {
		t.Fatalf("无 p2p 段应返回空串，got %q", got)
	}
}

func TestToMultiaddrs(t *testing.T) {
	code := Encode(validID, "1.2.3.4:4001")
	id, addrs, err := ToMultiaddrs(code)
	if err != nil {
		t.Fatalf("ToMultiaddrs 失败: %v", err)
	}
	if id.String() != validID {
		t.Fatalf("ID 不匹配: %q", id)
	}
	if len(addrs) != 2 { // TCP + QUIC 两条
		t.Fatalf("应生成 2 条 multiaddr，got %d", len(addrs))
	}
	// 第一条应与原始 multiaddr 等价。
	if addrs[0].String() != "/ip4/1.2.3.4/tcp/4001" {
		t.Fatalf("TCP multiaddr 不匹配: %q", addrs[0].String())
	}
	// 仅身份连接码 → 无地址。
	id2, addrs2, err := ToMultiaddrs(Encode(validID, ""))
	if err != nil || id2.String() != validID || len(addrs2) != 0 {
		t.Fatalf("仅身份 ToMultiaddrs 异常: id=%v addrs=%v err=%v", id2, addrs2, err)
	}
}

func TestIPv6RoundTrip(t *testing.T) {
	code := Encode(validID, "[fe80::1]:4001")
	id, hostPort, err := Decode(code)
	if err != nil {
		t.Fatalf("IPv6 Decode 失败: %v", err)
	}
	if id != validID || hostPort != "[fe80::1]:4001" {
		t.Fatalf("got id=%q hostPort=%q", id, hostPort)
	}
	_, addrs, err := ToMultiaddrs(code)
	if err != nil || len(addrs) != 2 {
		t.Fatalf("IPv6 ToMultiaddrs 异常: err=%v n=%d", err, len(addrs))
	}
	if addrs[0].String() != "/ip6/fe80::1/tcp/4001" {
		t.Fatalf("IPv6 TCP multiaddr 不匹配: %q", addrs[0].String())
	}
}

// 保证 multiStr 常量真实可用（防止占位 ID 误留测试路径）。
func TestMultiaddrConstantValid(t *testing.T) {
	if _, err := ma.NewMultiaddr(multiStr); err != nil {
		t.Fatalf("multiStr 常量非法: %v", err)
	}
}
