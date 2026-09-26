package p2pkit

import (
	"fmt"
	"strings"

	ma "github.com/multiformats/go-multiaddr"
)

// ValidateSeedSpec 校验「连接种子」输入：逗号分隔的引导地址，允许留空。
//
// 连接种子是「一条已在网成员的 multiaddr」，语义上只有两种合法状态：填地址，
// 或者留空（留空 = 不配自定义种子，只走私有 DHT + mDNS）。历史实现里
// none / public 这类字面量被当成特殊指令（public 表示公共引导），结果用户
// 以为填了 public 就挂上了公共网络，实际公共 DHT 关闭时它会被整条丢弃——
// 这里直接判为非法并给出改法，让误填在保存那一刻就暴露。
//
// 地址本身要求能确定对端身份：普通 multiaddr 必须带 /p2p/<节点ID>；
// /dnsaddr/… 由 DNS TXT 展开，展开项自带 /p2p，故豁免。
func ValidateSeedSpec(spec string) error {
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if item == "none" || item == "public" {
			return fmt.Errorf("%q 不是连接种子：留空即可；跨网冷启动请用「公共 DHT 临时引导」开关", item)
		}
		a, err := ma.NewMultiaddr(item)
		if err != nil {
			return fmt.Errorf("%q 不是合法地址：%v", item, err)
		}
		if _, err := a.ValueForProtocol(ma.P_DNSADDR); err == nil {
			continue
		}
		if _, err := a.ValueForProtocol(ma.P_P2P); err != nil {
			return fmt.Errorf("%q 缺少 /p2p/<节点ID> 组件，无法确定对端身份", item)
		}
	}
	return nil
}
