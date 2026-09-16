using System;
using System.Threading.Tasks;
using UnityEngine;

namespace Lanet.Sdk
{
    /// <summary>
    /// Lanet 在 Unity 里的统一门面：一个组件同时管住两条链路。
    ///
    /// <list type="bullet">
    /// <item><b>链路 1 — ws-gateway 网关</b>（<see cref="useGateway"/>）：跨平台可用，
    /// 经 WebSocket 帧协议访问网格内任意节点的 TCP 服务；网关中转，不是端到端。</item>
    /// <item><b>链路 2 — Android 本地节点</b>（<see cref="useAndroidNode"/>）：仅 Android，
    /// 复用 <c>lanet-plugin.aar</c> 让 App 自己成为网络成员（可 ping 虚拟 IP、被 mDNS 发现）。</item>
    /// </list>
    ///
    /// 事件一律在主线程触发（后台线程的事件会先过 <see cref="LanetDispatcher"/>），
    /// 所以可以直接在里面碰 Unity API。
    /// </summary>
    [AddComponentMenu("Lanet/Lanet Manager")]
    public class LanetManager : MonoBehaviour
    {
        [Header("启动行为")]
        [Tooltip("Start 时按下面勾选的链路自动连接")]
        public bool connectOnStart = true;

        [Header("链路 1：ws-gateway 网关（跨平台）")]
        [Tooltip("经网关访问网格内 TCP 服务；网关地址形如 ws://host:8700/gateway（生产用 wss://）")]
        public bool useGateway = true;
        public string gatewayUrl = "ws://127.0.0.1:8700/gateway";
        [Tooltip("群组邀请码（网关启动日志可查）")]
        public string inviteCode = "";
        public string clientName = "unity";
        [Tooltip("client = 主动开流；service = 接收入向流（网关同一时刻只允许一个 service 连接）")]
        public string gatewayMode = Frames.ModeClient;

        [Header("链路 2：Android 本地节点（仅 Android）")]
        [Tooltip("需要 Runtime/Plugins/Android/lanet-plugin.aar；构建平台必须是 Android")]
        public bool useAndroidNode = false;
        public string nodeName = "unity";
        [Tooltip("网络密钥：与目标网络一致才能互相发现（必填，留空会被原生层拒绝）")]
        public string networkKey = "";
        [Tooltip("引导种子 multiaddr（填任意已在网成员的地址）")]
        public string[] bootstrap = new string[0];
        [Tooltip("自动同意陌生节点申请（联调方便；生产建议关掉走人工审批）")]
        public bool autoAccept = true;
        [Tooltip("是否建立虚拟网卡（关掉则退化为纯应用层，不能 ping 虚拟 IP）")]
        public bool wantTun = true;
        [Tooltip("节点状态轮询间隔（秒）")]
        public float nodeStateInterval = 2f;

        // ---- 事件（均在主线程触发）----

        /// <summary>网关鉴权成功。</summary>
        public event Action<GatewayInfo> OnGatewayConnected;

        /// <summary>网关连接关闭（之后需自行重连）。</summary>
        public event Action OnGatewayClosed;

        /// <summary>网关连接级异常。</summary>
        public event Action<Exception> OnGatewayError;

        /// <summary>收到网格入向流（service 模式）。</summary>
        public event Action<GatewayStream> OnInboundStream;

        /// <summary>本地节点状态发生有意义的变化（运行态/虚拟 IP/成员数/待审批数）。</summary>
        public event Action<LanetNodeStatus> OnNodeStateChanged;

        /// <summary>本地节点的可读日志（启动/停止/审批的结果与失败原因）。</summary>
        public event Action<string> OnNodeMessage;

        private LanetGatewayClient _client;
        private LanetNodeStatus _nodeStatus = new LanetNodeStatus();
        private float _nextNodePoll;

        /// <summary>当前网关客户端（未连接时为 null）。</summary>
        public LanetGatewayClient Client => _client;

        /// <summary>网关是否已连接。</summary>
        public bool IsGatewayConnected => _client != null;

        /// <summary>本地节点最近一次状态快照。</summary>
        public LanetNodeStatus NodeStatus => _nodeStatus;

        /// <summary>本地节点是否在运行。</summary>
        public bool IsNodeRunning => _nodeStatus != null && _nodeStatus.running;

        /// <summary>本机连接码（未运行/非 Android 时为空串）。</summary>
        public string InviteCode => LanetAndroidNode.IsSupported ? LanetAndroidNode.InviteCode() : "";

        private void Start()
        {
            if (!connectOnStart) return;
            if (useGateway) _ = ConnectGatewayAsync();
            if (useAndroidNode) StartNode();
        }

        private void Update()
        {
            if (!useAndroidNode || !LanetAndroidNode.IsSupported) return;
            if (Time.unscaledTime < _nextNodePoll) return;
            _nextNodePoll = Time.unscaledTime + Mathf.Max(0.5f, nodeStateInterval);
            PollNodeState();
        }

        private void OnDestroy() => DisconnectGateway();

        // ------------------------------------------------------------------
        // 链路 1：网关
        // ------------------------------------------------------------------

        /// <summary>
        /// 连接网关并完成邀请码鉴权。已连接时会先断开旧的。
        /// 结果同时通过 <see cref="OnGatewayConnected"/> / <see cref="OnGatewayError"/> 广播。
        /// </summary>
        public async Task<bool> ConnectGatewayAsync()
        {
            DisconnectGateway();
            try
            {
                var client = await LanetGatewayClient.ConnectAsync(new GatewayOptions
                {
                    Url = gatewayUrl,
                    InviteCode = inviteCode,
                    Name = clientName,
                    Mode = gatewayMode,
                }).ConfigureAwait(true); // 回到 Unity 主线程，事件订阅者可直接碰引擎 API

                client.OnStream += HandleInboundStream;
                client.Closed += HandleGatewayClosed;
                client.OnError += HandleGatewayError;
                _client = client;
                OnGatewayConnected?.Invoke(client.Info);
                return true;
            }
            catch (Exception ex)
            {
                OnGatewayError?.Invoke(ex);
                return false;
            }
        }

        /// <summary>断开网关并中止所有流。</summary>
        public void DisconnectGateway()
        {
            var client = _client;
            if (client == null) return;
            _client = null;
            client.OnStream -= HandleInboundStream;
            client.Closed -= HandleGatewayClosed;
            client.OnError -= HandleGatewayError;
            try
            {
                client.Dispose();
            }
            catch (Exception)
            {
                // 关闭期的异常没有处理价值
            }
        }

        /// <summary>访问网格内 TCP 服务（经网关 PortFWD 转发到目标节点）。</summary>
        public Task<GatewayStream> DialAsync(string virtualIp, int port)
        {
            var client = _client;
            if (client == null) throw new InvalidOperationException("尚未连接网关，请先 await ConnectGatewayAsync()");
            return client.DialAsync(virtualIp, port);
        }

        /// <summary>一问一答：开流 → 写 → 半关闭 → 读到对端 EOF，返回 UTF-8 文本。</summary>
        public async Task<string> RequestAsync(string virtualIp, int port, string payload)
        {
            using (var stream = await DialAsync(virtualIp, port))
            {
                return await stream.RequestStringAsync(payload);
            }
        }

        private void HandleInboundStream(GatewayStream stream)
            => LanetDispatcher.Enqueue(() => OnInboundStream?.Invoke(stream));

        private void HandleGatewayClosed()
            => LanetDispatcher.Enqueue(() => OnGatewayClosed?.Invoke());

        private void HandleGatewayError(Exception ex)
            => LanetDispatcher.Enqueue(() => OnGatewayError?.Invoke(ex));

        // ------------------------------------------------------------------
        // 链路 2：Android 本地节点
        // ------------------------------------------------------------------

        /// <summary>用 Inspector 配置启动本地节点（参数为 null 时使用 Inspector 取值）。</summary>
        public void StartNode(LanetNodeOptions options = null)
        {
            if (!LanetAndroidNode.IsSupported)
            {
                Report("本地节点仅在 Android 平台可用");
                return;
            }
            if (!LanetAndroidNode.IsAvailable)
            {
                Report("未找到原生实现：请确认 Runtime/Plugins/Android/lanet-plugin.aar 已导入，且构建平台为 Android");
                return;
            }

            LanetAndroidNode.StartAsync(options ?? BuildNodeOptions(), result =>
            {
                if (result.ok)
                {
                    Report(result.stage == "awaiting_permission"
                        ? "已弹出系统 VPN 授权框，同意后节点自动启动"
                        : "节点启动中…");
                }
                else
                {
                    Report("启动失败：" + result.error);
                }
                _nextNodePoll = 0f; // 立刻拉一次状态
            });
        }

        /// <summary>停止本地节点。</summary>
        public void StopNode()
        {
            if (!LanetAndroidNode.IsSupported) return;
            LanetAndroidNode.StopAsync(result =>
            {
                Report(result.ok ? "节点已停止" : "停止失败：" + result.error);
                _nextNodePoll = 0f;
            });
        }

        /// <summary>主动连接一个节点（裸 PeerID / 连接码 / multiaddr）。入网的关键动作。</summary>
        public void ConnectPeer(string address)
        {
            var result = LanetAndroidNode.Connect(address);
            if (!result.ok)
            {
                Report("连接失败：" + result.error);
                return;
            }
            var data = LanetJson.Parse<LanetConnectResult>(result.data);
            Report(data == null
                ? "已提交连接请求"
                : (data.pending ? $"已向 {data.name} 提交申请，等对方同意" : $"已连接 {data.name}（{data.virtual_ip}）"));
            _nextNodePoll = 0f;
        }

        /// <summary>同意/拒绝待审批节点。</summary>
        public void ApprovePeer(string peerId, bool approve = true)
        {
            var result = LanetAndroidNode.Approve(peerId, approve);
            Report(result.ok ? (approve ? "已同意该节点" : "已拒绝该节点") : "审批失败：" + result.error);
            _nextNodePoll = 0f;
        }

        /// <summary>移除成员（撤信任 + 出地址簿）。</summary>
        public void RemovePeer(string peerId)
        {
            var result = LanetAndroidNode.Remove(peerId);
            Report(result.ok ? "已移除该节点" : "移除失败：" + result.error);
            _nextNodePoll = 0f;
        }

        /// <summary>重新拉取成员表。</summary>
        public LanetMemberInfo[] Members() => LanetAndroidNode.Members();

        /// <summary>重新拉取待审批列表。</summary>
        public LanetPendingInfo[] Pending() => LanetAndroidNode.Pending();

        /// <summary>重新拉取附近节点列表。</summary>
        public LanetNearbyInfo[] Nearby() => LanetAndroidNode.Nearby();

        private LanetNodeOptions BuildNodeOptions() => new LanetNodeOptions
        {
            name = string.IsNullOrEmpty(nodeName) ? "unity" : nodeName,
            network_key = networkKey,
            bootstrap = bootstrap,
            auto_accept = autoAccept,
            want_tun = wantTun,
            version = Application.version,
        };

        private void PollNodeState()
        {
            var st = LanetAndroidNode.Status();
            bool changed = _nodeStatus == null
                || st.running != _nodeStatus.running
                || st.peer_id != _nodeStatus.peer_id
                || st.virtual_ip != _nodeStatus.virtual_ip
                || st.member_count != _nodeStatus.member_count
                || st.pending_count != _nodeStatus.pending_count
                || st.firewall != _nodeStatus.firewall;
            _nodeStatus = st;
            if (changed) OnNodeStateChanged?.Invoke(st);
        }

        private void Report(string message)
        {
            Debug.Log("[Lanet] " + message);
            OnNodeMessage?.Invoke(message);
        }
    }
}
