package serverless

import (
	"strings"
	"testing"
)

func TestDNSLabel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"yunloli", "yunloli"},
		{"Ay-Win", "ay-win"},
		{"我的电脑 PC1", "pc1"},
		{"安酱 的 笔记本", ""},
		{"  __Weird__ Name__  ", "weird-name"},
		{"multi--space", "multi-space"},
		{"-leading-and-trailing-", "leading-and-trailing"},
		{"节点机.192.168.1.5", "19216815"},
		{"", ""},
		{"   ", ""},
		{"a-b_c d", "a-b-c-d"},
	}
	for _, c := range cases {
		if got := DNSLabel(c.in); got != c.want {
			t.Errorf("DNSLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDNSLabelTruncates(t *testing.T) {
	long := strings.Repeat("ab", 40) // 80 字符
	got := DNSLabel(long)
	if len(got) != 63 {
		t.Errorf("DNSLabel 长度 = %d, want 63", len(got))
	}
	if strings.HasSuffix(got, "-") {
		t.Error("截断后不应以连字符结尾")
	}
}

func members() []MemberRef {
	return []MemberRef{
		{PeerID: "peerCcccc", Name: "Yunloli", VirtualIP: "10.7.3.3"},
		{PeerID: "peerAaaaa", Name: "ay-win", VirtualIP: "10.7.92.241"},
		{PeerID: "peerBbbbb", Name: "ay-win", VirtualIP: "10.7.5.5"}, // 与上一条重名
		{PeerID: "peerDdddd", Name: "安酱的机器", VirtualIP: "10.7.8.8"},  // 无法规范化
	}
}

func TestHostnamesDeterministic(t *testing.T) {
	m := members()
	h1 := Hostnames(m)
	// 乱序输入结果必须一致（确定性）。
	rev := append([]MemberRef(nil), m...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	h2 := Hostnames(rev)
	for k, v := range h1 {
		if h2[k] != v {
			t.Fatalf("Hostnames 不确定：%s → %q vs %q", k, v, h2[k])
		}
	}
	// 重名：PeerID 排序靠前者保留原名，靠后者带后缀（PeerID 尾 4 位小写）。
	if h1["peerAaaaa"] != "ay-win" {
		t.Errorf("peerAaaaa label = %q, want ay-win", h1["peerAaaaa"])
	}
	if h1["peerBbbbb"] != "ay-win-bbbb" {
		t.Errorf("peerBbbbb label = %q, want ay-win-bbbb（PeerID 尾 4 位小写）", h1["peerBbbbb"])
	}
	if h1["peerDdddd"] != "node-8-8" {
		t.Errorf("无法规范化的名字应回退 node-<IP 尾两段>，got %q", h1["peerDdddd"])
	}
	if h1["peerCcccc"] != "yunloli" {
		t.Errorf("大小写应归一化，got %q", h1["peerCcccc"])
	}
}

func TestResolveTarget(t *testing.T) {
	m := members()

	// 虚拟 IP。
	got, err := ResolveTarget(m, "10.7.92.241")
	if err != nil || got.PeerID != "peerAaaaa" {
		t.Fatalf("虚拟 IP 解析失败: %v %+v", err, got)
	}

	// 完整虚拟地址（大小写不敏感、允许结尾点）。
	got, err = ResolveTarget(m, "Yunloli.LANET.")
	if err != nil || got.PeerID != "peerCcccc" {
		t.Fatalf("完整虚拟地址解析失败: %v %+v", err, got)
	}

	// 短名。
	got, err = ResolveTarget(m, "yunloli")
	if err != nil || got.PeerID != "peerCcccc" {
		t.Fatalf("短名解析失败: %v %+v", err, got)
	}

	// 重名短名解析到排序靠前者（确定性），带后缀地址解析到指定者。
	got, err = ResolveTarget(m, "ay-win.lanet")
	if err != nil || got.PeerID != "peerAaaaa" {
		t.Fatalf("重名短名应解析到确定性成员: %v %+v", err, got)
	}
	got, err = ResolveTarget(m, "ay-win-bbbb.lanet")
	if err != nil || got.PeerID != "peerBbbbb" {
		t.Fatalf("重名后缀地址解析失败: %v %+v", err, got)
	}

	// 原始名兜底（无法规范化的中文名）。
	got, err = ResolveTarget(m, "安酱的机器")
	if err != nil || got.PeerID != "peerDdddd" {
		t.Fatalf("原始名兜底解析失败: %v %+v", err, got)
	}

	// 未找到。
	if _, err = ResolveTarget(m, "ghost.lanet"); err == nil {
		t.Fatal("不存在的目标应报错")
	}
	if _, err = ResolveTarget(m, "10.7.99.99"); err == nil {
		t.Fatal("成员表外 IP 应报错")
	}
	if _, err = ResolveTarget(m, ""); err == nil {
		t.Fatal("空目标应报错")
	}
}

func TestResolveTargetPeerIDSuffixCollision(t *testing.T) {
	// 同名成员后缀取 PeerID 尾 4 位（随机尾段，区分度高）。
	m := []MemberRef{
		{PeerID: "peer1111aaaa", Name: "pc", VirtualIP: "10.7.1.1"},
		{PeerID: "peer2222bbbb", Name: "pc", VirtualIP: "10.7.1.2"},
	}
	hosts := Hostnames(m)
	if hosts["peer1111aaaa"] != "pc" || hosts["peer2222bbbb"] != "pc-bbbb" {
		t.Fatalf("unexpected: %v", hosts)
	}
	// 解析仍应无歧义。
	if got, err := ResolveTarget(m, "pc-bbbb.lanet"); err != nil || got.PeerID != "peer2222bbbb" {
		t.Fatalf("后缀解析失败: %v %+v", err, got)
	}

	// 重名成员尾 4 位相同 → 后者取 pc-xxxx，与前者 pc 仍无歧义。
	m2 := []MemberRef{
		{PeerID: "12D3KooWxxxx", Name: "pc", VirtualIP: "10.7.1.1"},
		{PeerID: "9a8bKooWxxxx", Name: "pc", VirtualIP: "10.7.1.2"},
	}
	hosts2 := Hostnames(m2)
	if hosts2["12D3KooWxxxx"] != "pc" || hosts2["9a8bKooWxxxx"] != "pc-xxxx" {
		t.Fatalf("尾 4 位相同时应得到 pc / pc-xxxx: %v", hosts2)
	}

	// 字面名 pc-xxxx 与已占用的 pc-xxxx 冲突 → label+尾4位：pc-xxxx-cc33。
	m3 := append(append([]MemberRef(nil), m2...), MemberRef{PeerID: "aa11bb22cc33", Name: "pc-xxxx", VirtualIP: "10.7.1.3"})
	hosts3 := Hostnames(m3)
	if hosts3["aa11bb22cc33"] != "pc-xxxx-cc33" {
		t.Fatalf("字面名冲突应得到 pc-xxxx-cc33: %v", hosts3)
	}

	// 真·-x 兜底：三个成员同名且其中两个 PeerID 尾 4 位相同 → 最后者追加 -x。
	m4 := []MemberRef{
		{PeerID: "aa11bb22cc33", Name: "pc-xxxx", VirtualIP: "10.7.1.1"},
		{PeerID: "dd44ee55cc33", Name: "pc-xxxx", VirtualIP: "10.7.1.2"},
		{PeerID: "ff66gg77cc33", Name: "pc-xxxx", VirtualIP: "10.7.1.3"},
	}
	hosts4 := Hostnames(m4)
	if hosts4["aa11bb22cc33"] != "pc-xxxx" ||
		hosts4["dd44ee55cc33"] != "pc-xxxx-cc33" ||
		hosts4["ff66gg77cc33"] != "pc-xxxx-cc33-x" {
		t.Fatalf("-x 兜底失败: %v", hosts4)
	}
}
