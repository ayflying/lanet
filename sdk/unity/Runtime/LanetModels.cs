using System;

namespace Lanet.Sdk
{
    /// <summary>
    /// 原生门面（AAR 侧 <c>com.lanet.plugin.LanetNode</c>）的统一返回信封。
    /// <code>
    /// {"ok":true,"stage":"starting","data":"…"}
    /// {"ok":false,"error":"…"}
    /// </code>
    /// 所有原生调用都返回这个结构，不必猜类型。
    /// </summary>
    [Serializable]
    public class LanetResult
    {
        /// <summary>是否成功。</summary>
        public bool ok;

        /// <summary>阶段标记，如 <c>awaiting_permission</c> / <c>starting</c> / <c>stopping</c>。</summary>
        public string stage;

        /// <summary>需要结构化数据时是内嵌的 JSON 文本（对象或数组），再解析一层。</summary>
        public string data;

        /// <summary>失败原因（<see cref="ok"/> 为 false 时有效）。</summary>
        public string error;

        public override string ToString()
            => ok ? $"ok(stage={stage}{(string.IsNullOrEmpty(data) ? "" : $", data={data}")})" : $"error({error})";
    }

    /// <summary>
    /// JsonUtility 不支持解析**顶层数组**，解析数组时用它补一层信封。
    /// 见 <see cref="LanetJson.ParseArray{T}"/>。
    /// </summary>
    [Serializable]
    public class LanetArray<T>
    {
        public T[] items;
    }

    /// <summary>
    /// 节点状态，对应 AAR 侧 <c>LanetNode.status()</c>。
    /// 未运行时只有 <see cref="running"/> = false，其余字段为空。
    /// </summary>
    [Serializable]
    public class LanetNodeStatus
    {
        public bool running;
        public string peer_id;
        public string virtual_ip;
        public string virtual_host;
        public string group;
        public string name;
        public int member_count;
        public int pending_count;
        public int trusted_count;
        public string data_dir;
        public int tun_fd;
        /// <summary>原生门面版本。</summary>
        public string mobile;
        /// <summary>入向防火墙模式：deny-all / allow-list / allow-all。</summary>
        public string firewall;
    }

    /// <summary>成员表一项（需双方互相审批后才会出现）。</summary>
    [Serializable]
    public class LanetMemberInfo
    {
        public string peer_id;
        public string name;
        public string virtual_ip;
        /// <summary>虚拟主机名（如 <c>yunloli.lanet</c>）。</summary>
        public string hostname;
        public string platform;
        public string version;
        /// <summary>最近一次使用的连接路径（直连/中继）。</summary>
        public string path;
    }

    /// <summary>待审批申请一项。</summary>
    [Serializable]
    public class LanetPendingInfo
    {
        public string peer_id;
        public string name;
        public string[] addrs;
        public string reason;
        /// <summary>形如 <c>2026-09-16 13:05:00</c>。</summary>
        public string requested_at;
    }

    /// <summary>地址簿（已信任节点）一项。</summary>
    [Serializable]
    public class LanetPeerInfo
    {
        public string peer_id;
        public string name;
        public string last_ip;
    }

    /// <summary>附近节点一项（发现到但未互信，可用于「发现新设备」列表）。</summary>
    [Serializable]
    public class LanetNearbyInfo
    {
        public string peer_id;
        public string name;
        public string[] addrs;
        /// <summary>发现来源：dht / dht-private / mdns。</summary>
        public string source;
        public string first_seen;
        public string last_seen;
    }

    /// <summary>
    /// 主动连接的结果，对应 AAR 侧 <c>LanetNode.connect()</c> 的 <c>data</c> 字段。
    /// </summary>
    [Serializable]
    public class LanetConnectResult
    {
        public string peer_id;
        /// <summary>已提交申请、等对端同意。</summary>
        public bool pending;
        /// <summary>已记录地址但暂未在 DHT 找到对方（不是失败，周期发现会补上）。</summary>
        public bool searching;
        public string message;
        public string name;
        public string virtual_ip;
        /// <summary>完成连接的路径，如 <c>local</c>。</summary>
        public string via;
    }

    /// <summary>
    /// JSON 解析小工具。原生层统一返回 JSON 文本，这里集中处理两件 Unity 的麻烦事：
    /// 顶层数组不能直接解析、缺字段时的兜底。
    /// </summary>
    public static class LanetJson
    {
        /// <summary>解析单对象；输入为空时返回 null。</summary>
        public static T Parse<T>(string json) where T : class
        {
            if (string.IsNullOrEmpty(json) || json == "null") return null;
            try
            {
                return UnityEngine.JsonUtility.FromJson<T>(json);
            }
            catch (Exception)
            {
                // 原生层返回异常格式（例如 gomobile 抛错的回溯文本）时不该炸掉调用方。
                return null;
            }
        }

        /// <summary>
        /// 解析顶层数组：JsonUtility 只认对象，这里补一层 <c>{"items":…}</c> 再解析。
        /// </summary>
        public static T[] ParseArray<T>(string json)
        {
            if (string.IsNullOrEmpty(json) || json == "null") return Array.Empty<T>();
            try
            {
                var boxed = UnityEngine.JsonUtility.FromJson<LanetArray<T>>("{\"items\":" + json + "}");
                return boxed?.items ?? Array.Empty<T>();
            }
            catch (Exception)
            {
                return Array.Empty<T>();
            }
        }

        /// <summary>解析 <see cref="LanetResult"/> 的 <c>data</c> 字段为对象。</summary>
        public static T ParseData<T>(LanetResult result) where T : class
            => result == null ? null : Parse<T>(result.data);
    }
}
