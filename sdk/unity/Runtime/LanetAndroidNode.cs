using System;
using UnityEngine;

namespace Lanet.Sdk
{
    /// <summary>
    /// 启动本地节点的参数。
    ///
    /// **字段名就是传给原生层的 JSON 键（snake_case），不要改** —— 原生侧
    /// <c>VpnAuthProxyActivity.Options.parse</c> 按这些键取值。
    /// </summary>
    [Serializable]
    public class LanetNodeOptions
    {
        /// <summary>本机在成员表里的显示名。</summary>
        public string name = "unity";

        /// <summary>
        /// 网络密钥：与目标网络一致才能互相发现。**必填**——留空只会按 PeerID 派生
        /// 「本机专属网」，与任何人都不同网，原生层会直接拒绝。
        /// </summary>
        public string network_key = "";

        /// <summary>引导种子 multiaddr（填任意已在网成员的地址即可入网）。</summary>
        public string[] bootstrap;

        /// <summary>自动同意陌生节点申请（默认开，方便联调；生产建议关掉走人工审批）。</summary>
        public bool auto_accept = true;

        /// <summary>是否建立虚拟网卡（默认 true；关掉则退化为纯应用层，不能 ping 虚拟 IP）。</summary>
        public bool want_tun = true;

        /// <summary>本客户端版本，随 info 协议上报给同网络成员。</summary>
        public string version = "";
    }

    /// <summary>
    /// Android 本地节点桥：让 Unity App 自己成为 lanet 网络成员（而不只是经网关中转）。
    ///
    /// 复用与 uni-app 插件同一份 <c>lanet-plugin.aar</c>（UPM 包内
    /// <c>Runtime/Plugins/Android/</c>），AAR 里声明的 VpnService 与授权 Activity
    /// 会被 Unity 的 Gradle 构建自动合并进 manifest。
    ///
    /// 只调**零参数或单 String 参数**的原生静态方法：Unity 的
    /// <c>AndroidJavaClass.CallStatic</c> 按传入对象的实际类推导参数签名，若把 Activity
    /// 当 Context 传会推到 <c>UnityPlayerActivity</c> 从而找不到方法。AAR 里的
    /// <c>LanetInitProvider</c> 会在进程启动时把 Application Context 交给原生层，
    /// 因此这里用 <c>startAuto</c> / <c>stopAuto</c> / <c>isAuthorizedAuto</c>。
    ///
    /// 所有方法都必须在主线程调用（<c>startAuto</c> 会拉起授权 Activity）——
    /// 用 <see cref="StartAsync"/> / <see cref="StopAsync"/> 可自动满足。
    /// </summary>
    public static class LanetAndroidNode
    {
        /// <summary>原生门面的全限定类名。</summary>
        public const string NodeClass = "com.lanet.plugin.LanetNode";

        private static AndroidJavaClass _cls;
        private static bool _classError;

        /// <summary>当前平台是否支持本地节点（仅 Android）。</summary>
        public static bool IsSupported => Application.platform == RuntimePlatform.Android;

        /// <summary>原生实现是否可用（AAR 已导入且类可加载）。</summary>
        public static bool IsAvailable
        {
            get
            {
                if (!IsSupported) return false;
                try
                {
                    return Cls() != null;
                }
                catch (Exception)
                {
                    return false;
                }
            }
        }

        // ------------------------------------------------------------------
        // 生命周期
        // ------------------------------------------------------------------

        /// <summary>
        /// 启动并入网。返回 <c>stage=awaiting_permission</c> 表示已弹系统 VPN 授权框，
        /// 用户同意后服务会自动起来；此时应轮询 <see cref="Status"/> 直到
        /// <c>running=true</c>（<see cref="LanetManager"/> 会自动轮询）。
        /// </summary>
        public static LanetResult Start(LanetNodeOptions options)
            => CallResult("startAuto", JsonUtility.ToJson(options ?? new LanetNodeOptions()));

        /// <summary>在主线程启动（推荐入口；完成回调也在主线程）。</summary>
        public static void StartAsync(LanetNodeOptions options, Action<LanetResult> onDone = null)
            => LanetDispatcher.Enqueue(() => onDone?.Invoke(Start(options)));

        /// <summary>停止节点并下线虚拟网卡。幂等。</summary>
        public static LanetResult Stop() => CallResult("stopAuto");

        /// <summary>在主线程停止（完成回调也在主线程）。</summary>
        public static void StopAsync(Action<LanetResult> onDone = null)
            => LanetDispatcher.Enqueue(() => onDone?.Invoke(Stop()));

        // ------------------------------------------------------------------
        // 状态查询
        // ------------------------------------------------------------------

        /// <summary>节点状态；未运行时 <c>running=false</c>。</summary>
        public static LanetNodeStatus Status()
            => LanetJson.Parse<LanetNodeStatus>(CallString("status")) ?? new LanetNodeStatus();

        public static bool IsRunning() => CallBool("isRunning");

        /// <summary>是否已获得系统 VPN 授权（已授权时 <see cref="Start"/> 不弹窗）。</summary>
        public static bool IsAuthorized() => CallBool("isAuthorizedAuto");

        /// <summary>成员表（双方互相审批后才会出现）。</summary>
        public static LanetMemberInfo[] Members()
            => LanetJson.ParseArray<LanetMemberInfo>(CallString("members"));

        /// <summary>待审批申请。</summary>
        public static LanetPendingInfo[] Pending()
            => LanetJson.ParseArray<LanetPendingInfo>(CallString("pending"));

        /// <summary>地址簿（已信任节点）。</summary>
        public static LanetPeerInfo[] Peers()
            => LanetJson.ParseArray<LanetPeerInfo>(CallString("peers"));

        /// <summary>附近节点（发现到但未互信）。</summary>
        public static LanetNearbyInfo[] Nearby()
            => LanetJson.ParseArray<LanetNearbyInfo>(CallString("nearby"));

        /// <summary>本机可作为引导种子的 multiaddr。</summary>
        public static string[] SeedAddrs() => LanetJson.ParseArray<string>(CallString("seedAddrs"));

        /// <summary>
        /// 本机连接码 <c>lanet://&lt;PeerID&gt;@addr,…</c>：把它发给别人，对端即可连进来。
        /// 未运行时返回空串。
        /// </summary>
        public static string InviteCode() => CallString("inviteCode");

        /// <summary>最近一次失败原因；无错误时为空串。</summary>
        public static string LastError() => CallString("lastError");

        /// <summary>原生门面版本，用于排查 AAR 与 C# 侧不匹配。</summary>
        public static string Version() => CallString("version");

        // ------------------------------------------------------------------
        // 交互操作
        // ------------------------------------------------------------------

        /// <summary>
        /// 主动连接一个节点。address 支持裸 PeerID / 连接码（<c>lanet://…</c>）/ multiaddr。
        ///
        /// 这是入网的关键动作：对端主动拨入**不会**在本机产生待审批，只有本机主动
        /// 填对方地址才会「先信任对方」，所以 UI 必须提供这个入口。
        /// </summary>
        public static LanetResult Connect(string address)
            => CallResult("connect", address ?? "");

        /// <summary>同意（approve=true）或拒绝一个待审批节点。</summary>
        public static LanetResult Approve(string peerId, bool approve = true)
            => CallResult("approve", peerId ?? "", approve);

        /// <summary>移除一个成员（撤信任 + 出地址簿 + 记墓碑）。</summary>
        public static LanetResult Remove(string peerId)
            => CallResult("remove", peerId ?? "");

        // ------------------------------------------------------------------
        // 底层调用
        // ------------------------------------------------------------------

        private static AndroidJavaClass Cls()
        {
            if (_cls != null) return _cls;
            if (!IsSupported) return null;
            try
            {
                _cls = new AndroidJavaClass(NodeClass);
                _classError = false;
                return _cls;
            }
            catch (Exception)
            {
                _classError = true;
                _cls = null;
                return null;
            }
        }

        private static string CallString(string method, params object[] args)
        {
            try
            {
                var cls = Cls();
                if (cls == null) return "";
                return cls.CallStatic<string>(method, args) ?? "";
            }
            catch (Exception ex)
            {
                Debug.LogWarning($"[Lanet] 原生调用 {method} 失败: {Describe(ex)}");
                return "";
            }
        }

        private static bool CallBool(string method, params object[] args)
        {
            try
            {
                var cls = Cls();
                if (cls == null) return false;
                return cls.CallStatic<bool>(method, args);
            }
            catch (Exception ex)
            {
                Debug.LogWarning($"[Lanet] 原生调用 {method} 失败: {Describe(ex)}");
                return false;
            }
        }

        private static LanetResult CallResult(string method, params object[] args)
        {
            try
            {
                var cls = Cls();
                if (cls == null) return new LanetResult { ok = false, error = UnavailableReason() };
                var raw = cls.CallStatic<string>(method, args);
                return LanetJson.Parse<LanetResult>(raw)
                       ?? new LanetResult { ok = false, error = "原生层返回为空" };
            }
            catch (Exception ex)
            {
                return new LanetResult { ok = false, error = Describe(ex) };
            }
        }

        private static string UnavailableReason()
        {
            if (!IsSupported) return "该能力仅在 Android 平台可用（当前编辑器/其他平台不可用）";
            return _classError
                ? $"未找到原生类 {NodeClass}：请确认 Runtime/Plugins/Android/lanet-plugin.aar 已随包导入并参与构建"
                : "原生实现不可用";
        }

        private static string Describe(Exception ex)
        {
            if (ex is AndroidJavaException aje)
            {
                // 原生层自己抛的错（network_key 为空等）会原样带回来，直接用更清楚。
                var msg = aje.Message ?? "";
                if (!string.IsNullOrEmpty(msg)) return msg;
            }
            return ex.Message;
        }
    }
}
