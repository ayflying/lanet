package protocol

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/libp2p/go-libp2p/core/protocol"
)

func groupKeyFor(key string) []byte {
	h := sha256.Sum256([]byte("test-group:" + key))
	return h[:]
}

// TestGroupProtoIDDeterministic 同一群密钥派生的协议 ID 必须稳定可复现
// （节点重启、跨版本升级都不能变——变了等于好友关系全体重连失败）。
func TestGroupProtoIDDeterministic(t *testing.T) {
	gk := groupKeyFor("alpha")
	a := GroupProtoID(BaseInfo, gk)
	b := GroupProtoID(BaseInfo, gk)
	if a != b {
		t.Fatalf("派生不稳定: %s != %s", a, b)
	}
	fp := GroupFingerprint(gk)
	if !strings.HasPrefix(string(a), "/lanet/"+fp+"/") || !strings.HasSuffix(string(a), "/info/1.0.0") {
		t.Fatalf("协议 ID 形态不符: %s", a)
	}
}

// TestGroupProtoIDIsolatesGroups 不同网络密钥派生的协议 ID 必须互不相同——
// 这是「异群 multistream 协商即失败」的根据。
func TestGroupProtoIDIsolatesGroups(t *testing.T) {
	seen := map[protocol.ID]string{}
	for _, k := range []string{"alpha", "beta", "gamma", "lanet/public"} {
		for _, base := range []string{BaseInfo, BaseUnfriend, BaseEcho, BaseUpdManifest, BaseUpdFile} {
			id := GroupProtoID(base, groupKeyFor(k))
			if prev, ok := seen[id]; ok {
				t.Fatalf("协议 ID 碰撞: %s 同时来自 %s 与 %s", id, prev, k+"/"+base)
			}
			seen[id] = k + "/" + base
		}
	}
	// 派生 ID 绝不能等于任何历史固定 ID（否则隔离失效）。
	fixed := []protocol.ID{
		"/lanet/info/1.0.0", "/lanet/unfriend/1.0.0", "/lanet/echo/1.0.0",
		"/lanet/update-manifest/1.0.0", "/lanet/update-file/1.0.0",
	}
	for id := range seen {
		for _, f := range fixed {
			if id == f {
				t.Fatalf("派生协议 ID 撞上固定 ID: %s", id)
			}
		}
	}
}

// TestDHTPrefixFor 私有 DHT 前缀派生：稳定、异群不同、以 /lanet/ 为根、
// 且不等于历史固定前缀 /lanet（老网络与新网络必须隔离）。
func TestDHTPrefixFor(t *testing.T) {
	ga, gb := groupKeyFor("alpha"), groupKeyFor("beta")
	pa, pb := DHTPrefixFor(ga), DHTPrefixFor(ga)
	if pa != pb {
		t.Fatalf("前缀派生不稳定: %s != %s", pa, pb)
	}
	if DHTPrefixFor(gb) == pa {
		t.Fatal("异群前缀不应相同")
	}
	if pa == "/lanet" {
		t.Fatal("派生前缀不得等于历史固定前缀")
	}
	if !strings.HasPrefix(pa, "/lanet/") || strings.Contains(pa[7:], "/") {
		t.Fatalf("前缀形态不符: %s", pa)
	}
}
