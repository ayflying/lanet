package p2pkit

import (
	"fmt"
	"strings"

	ma "github.com/multiformats/go-multiaddr"
)

// ValidateSeedSpec 校验「连接种子」输入：支持逗号、换行、空格或制表符分隔，允许留空。
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
	for _, item := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\r' || r == '\n' }) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if item == "none" || item == "public" {
			return fmt.Errorf("%q 不是连接种子：留空即可（留空会使用内置入口种子做首次接触）", item)
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

// DefaultEntrySeed 内置的默认入口种子：连接种子留空时用它做「首次接触」。
//
// 为什么可以内置一个固定地址：无服务器模式的私有 DHT 使用全网共享的 /lanet
// 路由前缀，任意一个公网可达的 lanet 节点都能充当所有节点的首次接触入口，
// 不需要与目标同群。反之，留空种子且局域网内没有其他 lanet 节点时，私有 DHT
// 路由表为空，节点只能广播自己、无法查找他人，表现为「看不到设备」——这正是
// 内置入口要消除的冷启动死角（见 pkg/serverless 的 dhtRound：地址簿为空时
// 只 Provide 不 FindProviders）。
//
// 该地址是官方机群的公网入口，可用控制台「连接种子」覆盖；也可以用
// -no-default-seed / LANET_NO_DEFAULT_SEED / 配置项 disable_default_seed
// 彻底关闭（关闭后行为回到「只有私有 DHT + mDNS」）。多个地址用逗号分隔，
// 逐个尝试，任一可用即可入网。
const DefaultEntrySeed = "/ip4/101.37.28.111/udp/4001/quic-v1/p2p/12D3KooWR4qSqJtc5LCzfWrNqBQ8rmr35YrwQR159d9H6fxhRigF"

// SeedSource 描述本次运行实际采用的连接种子来源（供日志与测试断言）。
type SeedSource string

const (
	// SeedSourceCustom 用户在控制台/命令行显式填写的种子（完全覆盖内置入口）。
	SeedSourceCustom SeedSource = "custom"
	// SeedSourceDefault 未填写种子，采用内置入口。
	SeedSourceDefault SeedSource = "default"
	// SeedSourceNone 未填写种子且显式关闭了内置入口：只走私有 DHT + mDNS。
	SeedSourceNone SeedSource = "none"
)

// StripLegacySeedLiterals 归一历史字面量：none / public 曾表示「不配种子 /
// 公共引导」，现在都不是地址。返回剔除这些项后的规格与命中的字面量列表
// （调用方据此打日志提示改法）。历史语义里 public 在公共 DHT 关闭时本就被
// 丢弃，故一律按「未配置种子」处理，不改变实际连接行为。
func StripLegacySeedLiterals(spec string) (string, []string) {
	var clean, legacy []string
	for _, item := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\r' || r == '\n' }) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if item == "none" || item == "public" {
			legacy = append(legacy, item)
			continue
		}
		clean = append(clean, item)
	}
	return strings.Join(clean, ","), legacy
}

// ResolveSeedSpec 决定实际使用的连接种子规格与来源。
//
// 优先级：自定义种子（填了就完全覆盖内置入口，不叠加，避免"填了还被牵去
// 连别的节点"的意外）> 内置默认入口 > 无种子（仅当显式关闭内置入口）。
// 历史字面量 none / public 视为未填写，因此老配置会平滑落到内置入口。
func ResolveSeedSpec(custom string, disableDefault bool) (string, SeedSource) {
	clean, _ := StripLegacySeedLiterals(custom)
	if clean != "" {
		return clean, SeedSourceCustom
	}
	if disableDefault {
		return "", SeedSourceNone
	}
	return DefaultEntrySeed, SeedSourceDefault
}
