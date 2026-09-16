using System;
using System.Threading.Tasks;
using UnityEngine;

namespace Lanet.Sdk.Samples
{
    /// <summary>
    /// 两条链路的最小可跑示例。把它挂到任意 GameObject 上会自动补一个
    /// <see cref="LanetManager"/>，运行时左上角出现一块操作面板。
    ///
    /// 用法：
    /// <list type="number">
    /// <item>先在 Inspector 里给 LanetManager 填好参数（网关地址/邀请码，或网络密钥/种子）。</item>
    /// <item>链路 1：点「经网关请求」，对网格内某节点的 TCP 服务做一问一答。</item>
    /// <item>链路 2（仅 Android 真机）：点「启动本地节点」，同意系统 VPN 授权后本机入网，
    /// 面板会显示虚拟 IP 与连接码——把连接码发给同网络的其他成员即可互连。</item>
    /// </list>
    /// </summary>
    [RequireComponent(typeof(LanetManager))]
    [AddComponentMenu("Lanet/Lanet Demo (Samples)")]
    public class LanetDemo : MonoBehaviour
    {
        [Tooltip("点「经网关请求」时访问的虚拟 IP（网格内某节点）")]
        public string targetVirtualIp = "10.7.207.102";

        [Tooltip("点「经网关请求」时访问的端口")]
        public int targetPort = 4001;

        [Tooltip("请求负载；默认走 HTTP 头，能直接验证到目标是否为 HTTP 服务")]
        public string payload = "GET / HTTP/1.0\r\nHost: lanet\r\n\r\n";

        private LanetManager _mgr;
        private string _log = "";
        private string _peersText = "";

        private void Awake()
        {
            _mgr = GetComponent<LanetManager>();
            _mgr.OnGatewayConnected += info => Append($"网关已连接 virtual_ip={info.VirtualIP} peer={Short(info.PeerID)}");
            _mgr.OnGatewayClosed += () => Append("网关已断开");
            _mgr.OnGatewayError += ex => Append("网关错误：" + ex.Message);
            _mgr.OnNodeStateChanged += st => Append($"节点状态 running={st.running} ip={st.virtual_ip} 成员={st.member_count} 待审批={st.pending_count}");
            _mgr.OnNodeMessage += Append;
            _mgr.OnInboundStream += stream => _ = HandleInboundAsync(stream);
        }

        // ------------------------------------------------------------------
        // 链路 1：经网关访问网格内 TCP 服务
        // ------------------------------------------------------------------

        private async void DoGatewayRequest()
        {
            try
            {
                Append($"→ 经网关请求 {targetVirtualIp}:{targetPort}");
                var reply = await _mgr.RequestAsync(targetVirtualIp, targetPort, payload);
                Append($"← 收到 {reply.Length} 字符：\n{Head(reply, 300)}");
            }
            catch (Exception ex)
            {
                Append("请求失败：" + ex.Message);
            }
        }

        /// <summary>
        /// service 模式下网关会把别人拨入的流交给我们。典型回应是「读一行、回一行」，
        /// 这里对任何入向流都回一个带时间戳的文本，方便对端确认链路是通的。
        /// </summary>
        private async Task HandleInboundAsync(GatewayStream stream)
        {
            try
            {
                using (stream)
                {
                    Append($"⇢ 收到入向流（peer={Short(stream.RemotePeer)} 中继={stream.ViaRelay}）");
                    await stream.SendStringAsync($"lanet-unity/{Application.version} {DateTime.Now:HH:mm:ss}\n");
                }
            }
            catch (Exception ex)
            {
                Append("入向流处理失败：" + ex.Message);
            }
        }

        // ------------------------------------------------------------------
        // 链路 2：Android 本地节点
        // ------------------------------------------------------------------

        private void RefreshPeers()
        {
            if (!LanetAndroidNode.IsSupported) { _peersText = "（非 Android，本地节点不可用）"; return; }
            if (!LanetAndroidNode.IsAvailable) { _peersText = "（AAR 未导入）"; return; }

            var members = _mgr.Members();
            var pending = _mgr.Pending();
            var sb = new System.Text.StringBuilder();
            sb.AppendLine($"成员 {members.Length}：");
            foreach (var m in members) sb.AppendLine($"  {m.name} {m.virtual_ip} {Short(m.peer_id)}");
            sb.AppendLine($"待审批 {pending.Length}：");
            foreach (var p in pending) sb.AppendLine($"  {p.name} {Short(p.peer_id)} {p.reason}");
            _peersText = sb.ToString();
        }

        // ------------------------------------------------------------------

        private void Append(string line)
        {
            _log = $"{DateTime.Now:HH:mm:ss} {line}\n{_log}";
            var lines = _log.Split('\n');
            if (lines.Length > 40) _log = string.Join("\n", lines, 0, 40);
            Debug.Log("[LanetDemo] " + line);
        }

        private static string Short(string id)
            => string.IsNullOrEmpty(id) ? "-" : (id.Length <= 12 ? id : id.Substring(0, 8) + "…");

        private static string Head(string s, int n)
            => string.IsNullOrEmpty(s) ? "" : (s.Length <= n ? s : s.Substring(0, n) + "…");

        private void OnGUI()
        {
            GUILayout.BeginArea(new Rect(10, 10, 460, 520), GUI.skin.box);

            GUILayout.Label($"<b>Lanet 示例</b>  平台={Application.platform}  网关={( _mgr.IsGatewayConnected ? "已连" : "未连")}", new GUIStyle(GUI.skin.label) { richText = true });

            GUILayout.Space(4);
            GUILayout.Label("链路 1：经 ws-gateway 访问网格内服务");
            GUILayout.BeginHorizontal();
            targetVirtualIp = GUILayout.TextField(targetVirtualIp, GUILayout.Width(140));
            targetPort = ParseInt(GUILayout.TextField(targetPort.ToString(), GUILayout.Width(50)), targetPort);
            if (GUILayout.Button("经网关请求")) DoGatewayRequest();
            GUILayout.EndHorizontal();

            GUILayout.Space(8);
            GUILayout.Label("链路 2：Android 本地节点（成为网络成员）");
            GUILayout.BeginHorizontal();
            GUI.enabled = LanetAndroidNode.IsAvailable;
            if (GUILayout.Button(_mgr.IsNodeRunning ? "停止本地节点" : "启动本地节点"))
            {
                if (_mgr.IsNodeRunning) _mgr.StopNode();
                else _mgr.StartNode();
            }
            if (GUILayout.Button("刷新成员")) RefreshPeers();
            GUI.enabled = true;
            GUILayout.EndHorizontal();

            if (LanetAndroidNode.IsAvailable)
            {
                var st = _mgr.NodeStatus;
                GUILayout.Label($"运行={st.running}  虚拟IP={st.virtual_ip}  PeerID={Short(st.peer_id)}  防火墙={st.firewall}");
                var code = _mgr.InviteCode;
                GUILayout.Label("连接码（发给同网络成员）：");
                GUILayout.TextField(string.IsNullOrEmpty(code) ? "（未运行）" : code);
                if (!string.IsNullOrEmpty(code) && GUILayout.Button("复制连接码")) GUIUtility.systemCopyBuffer = code;
            }
            else
            {
                GUILayout.Label("（非 Android 或 AAR 未导入：编辑器里只能验证链路 1）");
            }

            GUILayout.Space(6);
            if (!string.IsNullOrEmpty(_peersText)) GUILayout.Label(_peersText);
            GUILayout.Space(4);
            GUILayout.Label("日志：");
            GUILayout.TextArea(_log, GUILayout.ExpandHeight(true));

            GUILayout.EndArea();
        }

        private static int ParseInt(string s, int fallback)
            => int.TryParse(s, out var v) ? v : fallback;
    }
}
