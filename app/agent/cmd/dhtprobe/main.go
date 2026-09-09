// dhtprobe 双 DHT 流量测量探针。
//
// 目的：验证「公共 DHT 兜底」到底耗多少流量。
// 方法：与官方节点同一 host/Discovery 配置，分别在【双 DHT 全开】与
// 【仅私有 DHT】两种模式下运行相同时长，用 libp2p 自带带宽统计器
// （按协议 + 方向精确计数）输出：
//   - 总流量（含 DHT、identify、ping/keepalive、中继等全部）
//   - 每协议流量排行（/ipfs/kad vs /lanet/kad 一目了然）
//   - 连接的对端数量与 Top 对端
//
// 用法：dhtprobe -mode both|private -minutes 10 [-key xxx] [-bootstrap addr]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/metrics"

	"github.com/ayflying/pvn/pkg/serverless"
)

var bwc *metrics.BandwidthCounter

func main() {
	mode := flag.String("mode", "both", "both=双DHT（私有+公共兜底）, private=仅私有DHT")
	minutes := flag.Int("minutes", 10, "测量时长（分钟）")
	key := flag.String("key", "dht-traffic-probe-1", "网络密钥")
	bootstrap := flag.String("bootstrap", "", "私有 DHT 种子 multiaddr（逗号分隔；空=仅靠公共DHT冷启动）")
	port := flag.Int("port", 0, "TCP 监听端口（0=随机）")
	quiet := flag.Bool("quiet", false, "减少 Discovery 日志")
	flag.Parse()

	runFor := time.Duration(*minutes) * time.Minute
	fmt.Printf("== dhtprobe mode=%s duration=%s key=%s bootstrap=%q ==\n",
		*mode, runFor, *key, *bootstrap)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, runFor+20*time.Second)
	defer cancel()

	bwc = metrics.NewBandwidthCounter()
	h, err := libp2p.New(
		libp2p.ListenAddrStrings(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", *port)),
		libp2p.NATPortMap(),
		libp2p.BandwidthReporter(bwc),
	)
	must(err)
	defer h.Close()

	fmt.Printf("self=%s\n", h.ID().ShortString())
	for _, a := range h.Addrs() {
		fmt.Printf("  addr: %s/p2p/%s\n", a, h.ID())
	}

	boot := []string{}
	if *bootstrap != "" {
		boot = strings.Split(*bootstrap, ",")
	}
	discCfg := serverless.Config{
		NetworkKey: *key,
		Name:       "dhtprobe-" + *mode,
		Bootstrap:  boot,
		EnableMDNS: false, // 只测 DHT，隔离 mDNS 变量
		Interval:   30 * time.Second,
		Version:    "probe",
		Platform:   "probe",
		Quiet:      *quiet,
	}
	if *mode == "private" {
		discCfg.DisablePublicFallback = true
	}
	disc, err := serverless.New(ctx, h, discCfg)
	must(err)
	must(disc.Start(ctx))
	go disc.Run(ctx)

	// ---- 周期采样 ----
	t0 := time.Now()
	fmt.Printf("%-8s %-10s %-10s %-10s %-10s %s\n",
		"时间", "累计上行", "累计下行", "轮上行", "轮下行", "连接数")
	var prevUp, prevDown int64
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	sample := func() {
		up, down := totals()
		fmt.Printf("%-8s %-10s %-10s %-10s %-10s %d\n",
			time.Since(t0).Round(time.Second),
			human(up), human(down),
			human(up-prevUp), human(down-prevDown),
			len(h.Network().Peers()))
		prevUp, prevDown = up, down
	}
	for {
		select {
		case <-ctx.Done():
			report(h, t0, *mode)
			return
		case <-tick.C:
			sample()
		}
	}
}

// totals 汇总全部协议的总上行/下行。
func totals() (int64, int64) {
	var up, down int64
	for _, s := range bwc.GetBandwidthByProtocol() {
		up += s.TotalOut
		down += s.TotalIn
	}
	return up, down
}

// byProto 每协议流量，降序。
func byProto() [][3]any {
	var out [][3]any
	for p, s := range bwc.GetBandwidthByProtocol() {
		out = append(out, [3]any{string(p), s.TotalOut, s.TotalIn})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][1].(int64)+out[i][2].(int64) > out[j][1].(int64)+out[j][2].(int64) })
	return out
}

func report(h host.Host, t0 time.Time, mode string) {
	d := time.Since(t0).Round(time.Second)
	up, down := totals()
	fmt.Printf("\n== 结果 mode=%s 时长=%s ==\n", mode, d)
	fmt.Printf("总上行: %s（%.1f KB/分钟）\n", human(up), float64(up)/1024/(d.Minutes()))
	fmt.Printf("总下行: %s（%.1f KB/分钟）\n", human(down), float64(down)/1024/(d.Minutes()))
	fmt.Println("\n按协议排行（up/down）：")
	for _, e := range byProto() {
		fmt.Printf("  %-28s up=%-9s down=%s\n", e[0], human(e[1].(int64)), human(e[2].(int64)))
	}
	fmt.Printf("\n连接过的对端（当前 %d 个在线）：\n", len(h.Network().Peers()))
	var peers []string
	for _, p := range h.Peerstore().Peers() {
		peers = append(peers, p.ShortString())
	}
	sort.Strings(peers)
	for i, p := range peers {
		if i >= 20 {
			fmt.Printf("  … 共 %d 个\n", len(peers))
			break
		}
		fmt.Printf("  %s\n", p)
	}
}

func human(v any) string {
	b, _ := v.(int64)
	const k = 1024
	switch {
	case b >= k*k*k:
		return fmt.Sprintf("%.2fGB", float64(b)/k/k/k)
	case b >= k*k:
		return fmt.Sprintf("%.2fMB", float64(b)/k/k)
	case b >= k:
		return fmt.Sprintf("%.1fKB", float64(b)/k)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		os.Exit(1)
	}
}
