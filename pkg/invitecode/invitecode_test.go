package invitecode

import (
	"fmt"
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

// TestEncodeListRoundTrip 多地址连接码：本机多网卡地址应全部带上且顺序保持。
func TestEncodeListRoundTrip(t *testing.T) {
	addrs := []string{"192.168.50.170:4001", "[2408:824e:1592:7d80::577]:4001", "10.70.38.92:4001"}
	code := EncodeList(validID, addrs)
	want := "lanet://" + validID + "@192.168.50.170:4001,[2408:824e:1592:7d80::577]:4001,10.70.38.92:4001"
	if code != want {
		t.Fatalf("EncodeList 出错：\n got %q\nwant %q", code, want)
	}
	id, got, err := DecodeList(code)
	if err != nil {
		t.Fatalf("DecodeList 失败: %v", err)
	}
	if id != validID {
		t.Fatalf("ID 不匹配: %q", id)
	}
	if len(got) != len(addrs) {
		t.Fatalf("地址数不匹配: got %v", got)
	}
	for i := range addrs {
		if got[i] != addrs[i] {
			t.Fatalf("第 %d 个地址不匹配: got %q want %q", i, got[i], addrs[i])
		}
	}
	// 旧单值签名返回首个地址，老调用方语义不变。
	_, first, err := Decode(code)
	if err != nil || first != addrs[0] {
		t.Fatalf("Decode 应返回首个地址: got %q err=%v", first, err)
	}
}

// TestEncodeListSanitize 空元素、重复项被剔除；超过 MaxAddrs 被截断。
func TestEncodeListSanitize(t *testing.T) {
	code := EncodeList(validID, []string{"", " 1.1.1.1:4001 ", "1.1.1.1:4001", "2.2.2.2:4001"})
	_, got, err := DecodeList(code)
	if err != nil {
		t.Fatalf("DecodeList 失败: %v", err)
	}
	if len(got) != 2 || got[0] != "1.1.1.1:4001" || got[1] != "2.2.2.2:4001" {
		t.Fatalf("去重/去空失败: %v", got)
	}

	many := make([]string, 0, MaxAddrs+3)
	for i := 0; i < MaxAddrs+3; i++ {
		many = append(many, fmt.Sprintf("10.0.0.%d:4001", i+1))
	}
	_, got2, err := DecodeList(EncodeList(validID, many))
	if err != nil {
		t.Fatalf("DecodeList 失败: %v", err)
	}
	if len(got2) != MaxAddrs {
		t.Fatalf("应截断到 %d 个地址，got %d", MaxAddrs, len(got2))
	}

	// 全空 → 退化为仅身份形式。
	if code := EncodeList(validID, []string{"", "  "}); code != Scheme+"://"+validID {
		t.Fatalf("无有效地址应退化为仅身份形式，got %q", code)
	}
}

// TestDecodeListBackwardCompatible 单地址旧格式（0.5.43 及以前生成）必须仍可解析。
func TestDecodeListBackwardCompatible(t *testing.T) {
	legacy := "lanet://" + validID + "@1.2.3.4:4001"
	id, addrs, err := DecodeList(legacy)
	if err != nil {
		t.Fatalf("旧格式解析失败: %v", err)
	}
	if id != validID || len(addrs) != 1 || addrs[0] != "1.2.3.4:4001" {
		t.Fatalf("旧格式解析异常: id=%q addrs=%v", id, addrs)
	}
	if !IsInviteCode(legacy) {
		t.Fatal("旧格式应仍被识别为连接码")
	}
}

// TestToMultiaddrsMultiAddr 多地址连接码应展开为「每个地址 TCP+QUIC」共 2N 条。
func TestToMultiaddrsMultiAddr(t *testing.T) {
	code := EncodeList(validID, []string{"1.2.3.4:4001", "[2408:824e::577]:4001"})
	id, addrs, err := ToMultiaddrs(code)
	if err != nil {
		t.Fatalf("ToMultiaddrs 失败: %v", err)
	}
	if id.String() != validID {
		t.Fatalf("ID 不匹配: %q", id)
	}
	want := []string{
		"/ip4/1.2.3.4/tcp/4001", "/ip4/1.2.3.4/udp/4001/quic-v1",
		"/ip6/2408:824e::577/tcp/4001", "/ip6/2408:824e::577/udp/4001/quic-v1",
	}
	if len(addrs) != len(want) {
		t.Fatalf("应生成 %d 条 multiaddr，got %d: %v", len(want), len(addrs), addrs)
	}
	for i := range want {
		if addrs[i].String() != want[i] {
			t.Fatalf("第 %d 条不匹配: got %q want %q", i, addrs[i].String(), want[i])
		}
	}
}

// TestToMultiaddrsNormalizesIPv4Mapped IPv4-mapped IPv6 需归一为 /ip4，
// 否则会拼出非法的 /ip4/::ffff:1.2.3.4/tcp/4001。
func TestToMultiaddrsNormalizesIPv4Mapped(t *testing.T) {
	_, addrs, err := ToMultiaddrs(Encode(validID, "[::ffff:1.2.3.4]:4001"))
	if err != nil {
		t.Fatalf("ToMultiaddrs 失败: %v", err)
	}
	if addrs[0].String() != "/ip4/1.2.3.4/tcp/4001" {
		t.Fatalf("IPv4-mapped 未归一: %q", addrs[0].String())
	}
}

// TestDecodeListRejectsBadSegment 多地址里任一地址段非法即整体拒绝（不静默降级，
// 否则用户以为「带上了所有网卡」实际只连上第一个）。
// 尾随/多余分隔符属宽容范围（见 TestSplitHostPorts），不算非法。
func TestDecodeListRejectsBadSegment(t *testing.T) {
	cases := []string{
		"lanet://" + validID + "@1.2.3.4:4001,bad-addr",
		"lanet://" + validID + "@1.2.3.4:4001,:4001",
		"lanet://" + validID + "@1.2.3.4:4001,not-an-ip:4001",
	}
	for _, c := range cases {
		if IsInviteCode(c) {
			t.Errorf("IsInviteCode(%q) 应为 false", c)
		}
	}
	// 尾随逗号：解析出 1 个地址，不报错。
	id, addrs, err := DecodeList("lanet://" + validID + "@1.2.3.4:4001,")
	if err != nil || id != validID || len(addrs) != 1 {
		t.Fatalf("尾随逗号应被宽容: id=%q addrs=%v err=%v", id, addrs, err)
	}
}

// TestSplitHostPorts 分隔符宽容度：逗号为主，分号与空白（多行粘贴）也接受。
func TestSplitHostPorts(t *testing.T) {
	got := splitHostPorts("1.1.1.1:1, 2.2.2.2:2;3.3.3.3:3\n4.4.4.4:4")
	want := []string{"1.1.1.1:1", "2.2.2.2:2", "3.3.3.3:3", "4.4.4.4:4"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
