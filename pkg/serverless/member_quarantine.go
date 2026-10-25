package serverless

import "time"

// 失效隔离只驻留内存，不按时间解禁：陈旧 PEX/provider 的寿命不受本机控制。
// 容量耗尽后，所有新 ID 都先握手再入表，不能靠淘汰旧 ID 重新制造假上线。
const expiredMemberLimit = 4096

type memberQuarantine struct {
	ids  map[string]struct{}
	full bool
}

// 以下方法由 Discovery.mu 保护；重验证拨号仍复用有界调度器及失败退避。
func (q *memberQuarantine) remember(id string) {
	if q.ids == nil {
		q.ids = make(map[string]struct{})
	}
	if _, ok := q.ids[id]; ok {
		return
	}
	if len(q.ids) >= expiredMemberLimit {
		q.full = true
		return
	}
	q.ids[id] = struct{}{}
}

func (q *memberQuarantine) requiresHandshake(id string) bool {
	_, exists := q.ids[id]
	return q.full || exists
}

// 验证成功才清除该 ID；饱和保护不能清除，否则溢出时漏记的 ID 会再次假上线。
func (q *memberQuarantine) verified(id string) { delete(q.ids, id) }

// verifiedMemberLocked 仅在审批、群指纹和真实 info 往返均成功后调用。
// 出向重验证必须能从空表恢复，不能为拿到握手结果而预先发布占位。
func (d *Discovery) verifiedMemberLocked(id, source string) *Member {
	if !d.trustPolicyPassive(id, nil, "") {
		return nil
	}
	m := d.members[id]
	if m == nil {
		if d.unfriendGuardBlocked(id) {
			return nil
		}
		m = &Member{PeerID: id, VirtualIP: DeriveVirtualIP(d.groupKey, id),
			VirtualIPv6: DeriveVirtualIPv6(d.groupKey, id), Source: source,
			FirstSeen: time.Now(), LastDiscovered: time.Now()}
		d.members[id] = m
	}
	d.expiredMembers.verified(id)
	return m
}
