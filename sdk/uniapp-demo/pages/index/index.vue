<template>
    <view class="page">
        <view class="card">
            <view class="card-title">
                <text>lanet 组网</text>
                <text :class="['badge', running ? 'on' : 'off']">{{ running ? '已连接' : '未连接' }}</text>
            </view>

            <view v-if="!supported" class="warn">
                原生插件不可用。请确认：① 运行在 App 平台（不是 H5/小程序）；
                ② nativeplugins/lanet-vpn 已放进项目并走云打包（或自定义基座）。
            </view>

            <view class="row">
                <text class="label">节点名</text>
                <input class="input" v-model="form.name" placeholder="例如 my-phone" />
            </view>
            <view class="row">
                <text class="label">网络密钥</text>
                <input class="input" v-model="form.networkKey" placeholder="同一张网的所有成员必须一致" />
            </view>
            <view class="row">
                <text class="label">种子地址</text>
                <textarea
                    class="textarea"
                    v-model="form.bootstrap"
                    placeholder="每行一个，例如 /ip4/1.2.3.4/tcp/4001/p2p/12D3Koo…"
                    auto-height
                />
            </view>
            <view class="row">
                <text class="label">自动同意</text>
                <switch :checked="form.autoAccept" @change="(e) => (form.autoAccept = e.detail.value)" />
                <text class="hint">手机在 NAT 后建议打开：对端拨不进来，也就无法在本机弹审批</text>
            </view>

            <view class="btns">
                <button class="btn primary" :disabled="!supported || busy" @click="onStart">
                    {{ busy ? '处理中…' : '启动并联入网络' }}
                </button>
                <button class="btn" :disabled="!supported || !running" @click="onStop">停止</button>
            </view>
        </view>

        <view class="card">
            <view class="card-title">
                <text>本机状态</text>
                <text class="link" @click="refresh">刷新</text>
            </view>
            <view class="kv"><text class="k">虚拟 IP</text><text class="v mono">{{ st.virtual_ip || '—' }}</text></view>
            <view class="kv"><text class="k">PeerID</text><text class="v mono small">{{ st.peer_id || '—' }}</text></view>
            <view class="kv"><text class="k">网络密钥</text><text class="v mono small">{{ st.network_key || '—' }}</text></view>
            <view class="kv"><text class="k">入向防火墙</text><text class="v mono small">{{ st.firewall || '—' }}</text></view>
            <view class="kv"><text class="k">TUN fd</text><text class="v mono small">{{ st.tun_fd || '—' }}</text></view>
            <view class="kv"><text class="k">插件版本</text><text class="v mono small">{{ pluginVersion || '—' }}</text></view>
            <view v-if="err" class="err">{{ err }}</view>
            <view class="copyrow">
                <text class="k">连接码</text>
                <text class="v mono small" selectable>{{ invite || '（启动后生成）' }}</text>
            </view>
            <text v-if="invite" class="hint" @click="copyInvite">点此复制，发给对端即可让对方连进来</text>
        </view>

        <view class="card">
            <view class="card-title"><text>手动连接节点</text></view>
            <text class="hint">
                可以填对方的 PeerID、连接码（lanet://…）或 multiaddr。对端主动拨入不会在本机
                产生待审批，所以必须由你这边主动填一次。
            </text>
            <view class="row">
                <input class="input" v-model="connectTarget" placeholder="12D3Koo… 或 lanet://…" />
            </view>
            <view class="btns">
                <button class="btn" :disabled="!supported || !running" @click="onConnect">连接</button>
            </view>
        </view>

        <view class="card">
            <view class="card-title">
                <text>成员</text><text class="count">{{ members.length }}</text>
            </view>
            <view v-if="!members.length" class="hint">（暂无。入网后其它成员会陆续出现）</view>
            <view v-for="m in members" :key="m.peer_id" class="item">
                <text class="item-name">{{ m.name || '(未命名)' }}</text>
                <text class="item-sub mono">{{ m.virtual_ip }} · {{ m.platform || '?' }}</text>
                <text :class="['tag', m.online ? 'on' : 'off']">{{ m.online ? '在线' : '离线' }}</text>
            </view>
        </view>

        <view class="card">
            <view class="card-title">
                <text>待审批</text><text class="count">{{ pending.length }}</text>
            </view>
            <view v-if="!pending.length" class="hint">（无）</view>
            <view v-for="p in pending" :key="p.peer_id" class="item">
                <text class="item-name">{{ p.name || '(未知设备)' }}</text>
                <text class="item-sub mono">{{ p.peer_id }}</text>
                <view class="tag-btns">
                    <text class="tag on" @click="onApprove(p.peer_id, true)">同意</text>
                    <text class="tag off" @click="onApprove(p.peer_id, false)">拒绝</text>
                </view>
            </view>
        </view>

        <view class="foot">lanet · 无服务器 P2P 虚拟局域网</view>
    </view>
</template>

<script>
import lanet from '@/utils/lanet.js'

export default {
    data() {
        return {
            supported: false,
            busy: false,
            running: false,
            err: '',
            invite: '',
            pluginVersion: '',
            connectTarget: '',
            st: {},
            members: [],
            pending: [],
            form: {
                name: 'android',
                networkKey: '',
                bootstrap: '',
                autoAccept: true
            },
            timer: null
        }
    },
    onLoad() {
        this.supported = lanet.available()
        this.pluginVersion = lanet.version()
        this.refresh()
        // 状态轮询：成员上下线、待审批都是异步变化的，5 秒一次足够
        this.timer = setInterval(() => this.refresh(), 5000)
    },
    onUnload() {
        if (this.timer) {
            clearInterval(this.timer)
            this.timer = null
        }
    },
    methods: {
        parseSeeds() {
            return String(this.form.bootstrap || '')
                .split('\n')
                .map((s) => s.trim())
                .filter((s) => !!s)
        },
        refresh() {
            if (!this.supported) return
            try {
                this.st = lanet.status() || {}
                this.running = !!this.st.running
                this.members = lanet.members() || []
                this.pending = lanet.pending() || []
                this.invite = lanet.inviteCode() || ''
                const le = lanet.lastError()
                if (le) this.err = le
            } catch (e) {
                console.warn('[lanet-demo] 刷新失败', e)
            }
        },
        async onStart() {
            if (!this.form.networkKey) {
                uni.showToast({ title: '请填写网络密钥', icon: 'none' })
                return
            }
            this.busy = true
            this.err = ''
            try {
                const res = await lanet.startAndWait({
                    name: this.form.name || 'android',
                    network_key: this.form.networkKey,
                    bootstrap: this.parseSeeds(),
                    auto_accept: this.form.autoAccept
                })
                uni.showToast({
                    title: `已入网 ${res.status.virtual_ip}`,
                    icon: 'none'
                })
            } catch (e) {
                this.err = e.message || String(e)
                uni.showModal({
                    title: '连接失败',
                    content: this.err,
                    showCancel: false
                })
            } finally {
                this.busy = false
                this.refresh()
            }
        },
        async onStop() {
            try {
                await lanet.stop()
                this.running = false
                this.err = ''
            } catch (e) {
                this.err = e.message || String(e)
            } finally {
                this.refresh()
            }
        },
        async onConnect() {
            const target = (this.connectTarget || '').trim()
            if (!target) {
                uni.showToast({ title: '请填写目标地址', icon: 'none' })
                return
            }
            try {
                const res = await lanet.connect(target)
                uni.showToast({ title: '已发起连接', icon: 'none' })
                console.log('[lanet-demo] connect ->', res.data)
            } catch (e) {
                uni.showModal({
                    title: '连接失败',
                    content: e.message || String(e),
                    showCancel: false
                })
            } finally {
                this.refresh()
            }
        },
        async onApprove(peerId, ok) {
            try {
                await lanet.approve(peerId, ok)
            } catch (e) {
                uni.showToast({ title: e.message || '操作失败', icon: 'none' })
            } finally {
                this.refresh()
            }
        },
        copyInvite() {
            uni.setClipboardData({
                data: this.invite,
                success: () => uni.showToast({ title: '已复制', icon: 'none' })
            })
        }
    }
}
</script>

<style>
.page {
    padding: 24rpx;
}

.card {
    background: #fff;
    border-radius: 16rpx;
    padding: 28rpx;
    margin-bottom: 24rpx;
}

.card-title {
    display: flex;
    align-items: center;
    justify-content: space-between;
    font-size: 32rpx;
    font-weight: 600;
    color: #111827;
    margin-bottom: 20rpx;
}

.badge {
    font-size: 24rpx;
    padding: 4rpx 16rpx;
    border-radius: 20rpx;
}
.badge.on {
    background: #dcfce7;
    color: #166534;
}
.badge.off {
    background: #f3f4f6;
    color: #6b7280;
}

.warn {
    background: #fef3c7;
    color: #92400e;
    font-size: 26rpx;
    line-height: 1.6;
    padding: 20rpx;
    border-radius: 12rpx;
    margin-bottom: 20rpx;
}

.err {
    background: #fee2e2;
    color: #991b1b;
    font-size: 24rpx;
    padding: 16rpx;
    border-radius: 12rpx;
    margin-top: 16rpx;
    word-break: break-all;
}

.row {
    display: flex;
    align-items: center;
    margin-bottom: 20rpx;
}

.label {
    width: 170rpx;
    font-size: 28rpx;
    color: #374151;
    flex-shrink: 0;
}

.input {
    flex: 1;
    font-size: 28rpx;
    border-bottom: 1rpx solid #e5e7eb;
    padding: 10rpx 0;
}

.textarea {
    flex: 1;
    font-size: 26rpx;
    border: 1rpx solid #e5e7eb;
    border-radius: 10rpx;
    padding: 14rpx;
    min-height: 130rpx;
    width: 100%;
}

.hint {
    font-size: 22rpx;
    color: #9ca3af;
    line-height: 1.6;
    display: block;
    margin-top: 6rpx;
}

.btns {
    display: flex;
    margin-top: 10rpx;
}

.btn {
    flex: 1;
    font-size: 28rpx;
    margin-right: 16rpx;
    background: #f3f4f6;
    color: #374151;
}
.btn:last-child {
    margin-right: 0;
}
.btn.primary {
    background: #2563eb;
    color: #fff;
}

.kv {
    display: flex;
    align-items: flex-start;
    padding: 10rpx 0;
}
.copyrow {
    display: flex;
    align-items: flex-start;
    padding: 10rpx 0;
    border-top: 1rpx solid #f3f4f6;
    margin-top: 8rpx;
}
.k {
    width: 170rpx;
    font-size: 26rpx;
    color: #6b7280;
    flex-shrink: 0;
}
.v {
    flex: 1;
    font-size: 28rpx;
    color: #111827;
    word-break: break-all;
}
.mono {
    font-family: Consolas, Menlo, monospace;
}
.small {
    font-size: 24rpx;
}

.item {
    padding: 16rpx 0;
    border-bottom: 1rpx solid #f3f4f6;
}
.item-name {
    font-size: 28rpx;
    color: #111827;
}
.item-sub {
    font-size: 22rpx;
    color: #9ca3af;
    display: block;
    margin-top: 4rpx;
    word-break: break-all;
}
.tag {
    font-size: 22rpx;
    padding: 2rpx 14rpx;
    border-radius: 16rpx;
    display: inline-block;
    margin-top: 8rpx;
}
.tag.on {
    background: #dcfce7;
    color: #166534;
}
.tag.off {
    background: #f3f4f6;
    color: #6b7280;
}
.tag-btns {
    margin-top: 8rpx;
}
.tag-btns .tag {
    margin-right: 16rpx;
}

.count {
    font-size: 24rpx;
    color: #9ca3af;
}
.link {
    font-size: 26rpx;
    color: #2563eb;
}

.foot {
    text-align: center;
    font-size: 22rpx;
    color: #d1d5db;
    padding: 20rpx 0 40rpx;
}
</style>
