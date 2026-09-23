package serverless

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ayflying/pvn/pkg/p2pkit"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	libprotocol "github.com/libp2p/go-libp2p/core/protocol"
	multistream "github.com/multiformats/go-multistream"
)

const (
	nearbyTimeout             = 4 * time.Second
	nearbyNameBytes           = 128
	nearbyNonceBytes          = 32
	nearbyResponseBytes       = 1 + 2 + nearbyNameBytes + sha256.Size
	nearbyMaxConcurrent       = 8
	nearbyMaxOutbound         = 4
	nearbyPeerCooldown        = 30 * time.Second
	nearbyUnsupportedCooldown = time.Hour
	nearbyMaxPeers            = 512
)

// ErrNearbyCooldown 表示本机近期已探测该节点，调用方不应立即重试。
var ErrNearbyCooldown = errors.New("serverless: 附近节点探测冷却中或并发已满")

// ErrNearbyUnsupported 表示对端未注册设备名探测协议（包括旧版本）。
var ErrNearbyUnsupported = errors.New("serverless: 对端不支持附近设备名探测")

// NearbyProbe 只表示一次新鲜协议往返；DHT/PEX 地址记录本身绝不构成在线证据。
// Name 是对端自报设备名，不构成受信任的身份认证或连接授权。
type NearbyProbe struct {
	PeerID string
	Name   string
	Alive  bool
}

// 双向证明均绑定经过 libp2p 连接认证的双方 ID；响应额外绑定一次性挑战与名称。
// 长度前缀使可变字段的编码无歧义；两个方向使用不同域分隔。
func (d *Discovery) nearbyMAC(domain string, requester, responder peer.ID, payload []byte) [sha256.Size]byte {
	mac := hmac.New(sha256.New, d.groupKey)
	for _, field := range [][]byte{[]byte(domain), []byte(requester), []byte(responder), payload} {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		_, _ = mac.Write(size[:])
		_, _ = mac.Write(field)
	}
	var out [sha256.Size]byte
	copy(out[:], mac.Sum(nil))
	return out
}

type nearbyRateLimiter struct {
	mu       sync.Mutex
	incoming int
	outgoing int
	lastIn   map[peer.ID]time.Time
	lastOut  map[peer.ID]time.Time
}

func (r *nearbyRateLimiter) acquire(id peer.ID, inbound bool, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	last, active, max := r.lastOut, &r.outgoing, nearbyMaxOutbound
	if inbound {
		last, active, max = r.lastIn, &r.incoming, nearbyMaxConcurrent
	}
	if *active >= max {
		return false
	}
	if last == nil {
		last = make(map[peer.ID]time.Time)
		if inbound {
			r.lastIn = last
		} else {
			r.lastOut = last
		}
	}
	if until, ok := last[id]; ok && now.Before(until) {
		return false
	}
	if len(last) >= nearbyMaxPeers {
		for key, until := range last {
			if !now.Before(until) {
				delete(last, key)
			}
		}
		if len(last) >= nearbyMaxPeers {
			return false
		}
	}
	last[id] = now.Add(nearbyPeerCooldown)
	*active++
	return true
}

func (r *nearbyRateLimiter) release(inbound bool) {
	r.mu.Lock()
	if inbound {
		r.incoming--
	} else {
		r.outgoing--
	}
	r.mu.Unlock()
}

func (r *nearbyRateLimiter) unsupported(id peer.ID) {
	r.mu.Lock()
	r.lastOut[id] = time.Now().Add(nearbyUnsupportedCooldown)
	r.mu.Unlock()
}

func nearbyName(name string) string {
	if len(name) <= nearbyNameBytes {
		return name
	}
	for end := nearbyNameBytes; end > 0; end-- {
		if utf8.ValidString(name[:end]) {
			return name[:end]
		}
	}
	return ""
}

// ProbeNearby 对一个已发现的候选节点执行有界在线设备名探测；SDK 可将 DHT、
// mDNS 或 PEX 候选交给此方法，仅在返回 Alive=true 时展示为「在线附近」。
// 此方法不调用审批回调，不写成员表，也不授予任何数据面权限。
func (d *Discovery) ProbeNearby(ctx context.Context, ai peer.AddrInfo) (NearbyProbe, error) {
	if d == nil || d.host == nil || ai.ID == "" || ai.ID == d.host.ID() {
		return NearbyProbe{}, fmt.Errorf("serverless: 非法附近探测目标")
	}
	if err := ctx.Err(); err != nil {
		return NearbyProbe{}, err
	}
	d.mu.RLock()
	closed := d.closed
	d.mu.RUnlock()
	if closed || d.serviceCtx.Err() != nil {
		return NearbyProbe{}, fmt.Errorf("serverless: 发现服务已关闭")
	}
	if !d.nearbyGate.acquire(ai.ID, false, time.Now()) {
		return NearbyProbe{}, ErrNearbyCooldown
	}
	defer d.nearbyGate.release(false)
	probeCtx, cancel := context.WithTimeout(ctx, nearbyTimeout)
	defer cancel()
	stopService := context.AfterFunc(d.serviceCtx, cancel)
	defer stopService()
	addrs := p2pkit.CleanUnderlayAddrs(ai.Addrs)
	if len(addrs) > 4 {
		addrs = addrs[:4]
	}
	if len(addrs) > 0 {
		d.host.Peerstore().AddAddrs(ai.ID, addrs, peerstore.TempAddrTTL)
	}
	stream, err := d.host.NewStream(probeCtx, ai.ID, d.protoNearby)
	if err != nil {
		var unsupported multistream.ErrNotSupported[libprotocol.ID]
		if errors.As(err, &unsupported) {
			d.nearbyGate.unsupported(ai.ID)
			return NearbyProbe{}, ErrNearbyUnsupported
		}
		var unsupportedString multistream.ErrNotSupported[string]
		if errors.As(err, &unsupportedString) {
			d.nearbyGate.unsupported(ai.ID)
			return NearbyProbe{}, ErrNearbyUnsupported
		}
		return NearbyProbe{}, err
	}
	defer stream.Close()
	stop := context.AfterFunc(probeCtx, func() { _ = stream.Reset() })
	defer stop()
	_ = stream.SetDeadline(time.Now().Add(nearbyTimeout))
	var nonce [nearbyNonceBytes]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return NearbyProbe{}, err
	}
	proof := d.nearbyMAC("request-v1", d.host.ID(), ai.ID, nonce[:])
	var request [nearbyNonceBytes + sha256.Size]byte
	copy(request[:nearbyNonceBytes], nonce[:])
	copy(request[nearbyNonceBytes:], proof[:])
	if _, err := stream.Write(request[:]); err != nil {
		return NearbyProbe{}, err
	}
	if err := stream.CloseWrite(); err != nil {
		return NearbyProbe{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(stream, nearbyResponseBytes+1))
	if err != nil {
		return NearbyProbe{}, err
	}
	if len(raw) > nearbyResponseBytes {
		return NearbyProbe{}, fmt.Errorf("serverless: 附近探测响应超限")
	}
	if err := probeCtx.Err(); err != nil {
		return NearbyProbe{}, err
	}
	if len(raw) < 1+2+sha256.Size || raw[0] != 1 {
		return NearbyProbe{}, fmt.Errorf("serverless: 附近探测响应无效")
	}
	nameLen := int(binary.BigEndian.Uint16(raw[1:3]))
	if nameLen > nearbyNameBytes || len(raw) != 3+nameLen+sha256.Size || !utf8.Valid(raw[3:3+nameLen]) {
		return NearbyProbe{}, fmt.Errorf("serverless: 附近探测响应无效")
	}
	binding := make([]byte, 0, len(nonce)+3+nameLen)
	binding = append(binding, nonce[:]...)
	binding = append(binding, raw[:3+nameLen]...)
	want := d.nearbyMAC("response-v1", d.host.ID(), ai.ID, binding)
	if subtle.ConstantTimeCompare(raw[3+nameLen:], want[:]) != 1 {
		return NearbyProbe{}, fmt.Errorf("serverless: 附近探测响应认证失败")
	}
	return NearbyProbe{PeerID: ai.ID.String(), Name: string(raw[3 : 3+nameLen]), Alive: true}, nil
}

func (d *Discovery) handleNearby(s network.Stream) {
	defer s.Close()
	if !d.nearbyGate.acquire(s.Conn().RemotePeer(), true, time.Now()) {
		_ = s.Reset()
		return
	}
	defer d.nearbyGate.release(true)
	_ = s.SetDeadline(time.Now().Add(nearbyTimeout))
	var req [nearbyNonceBytes + sha256.Size + 1]byte
	n, err := io.ReadFull(s, req[:])
	if err != io.ErrUnexpectedEOF && err != nil {
		_ = s.Reset()
		return
	}
	requester, responder := s.Conn().RemotePeer(), d.host.ID()
	want := d.nearbyMAC("request-v1", requester, responder, req[:nearbyNonceBytes])
	if n != nearbyNonceBytes+sha256.Size || subtle.ConstantTimeCompare(req[nearbyNonceBytes:nearbyNonceBytes+sha256.Size], want[:]) != 1 {
		_ = s.Reset()
		return
	}
	name := []byte(nearbyName(d.cfg.Name))
	response := make([]byte, 3+len(name)+sha256.Size)
	response[0] = 1 // 活性：仅成功认证且实际处理请求时回复。
	binary.BigEndian.PutUint16(response[1:3], uint16(len(name)))
	copy(response[3:], name)
	binding := make([]byte, 0, nearbyNonceBytes+3+len(name))
	binding = append(binding, req[:nearbyNonceBytes]...)
	binding = append(binding, response[:3+len(name)]...)
	proof := d.nearbyMAC("response-v1", requester, responder, binding)
	copy(response[3+len(name):], proof[:])
	_, _ = s.Write(response)
}
