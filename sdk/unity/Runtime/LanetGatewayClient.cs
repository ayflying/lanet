using System;
using System.Collections.Concurrent;
using System.Net.WebSockets;
using System.Text;
using System.Threading;
using System.Threading.Tasks;

namespace Lanet.Sdk
{
    /// <summary>网关连接选项。</summary>
    public sealed class GatewayOptions
    {
        /// <summary>ws-gateway 地址，如 ws://host:8700/gateway。</summary>
        public string Url { get; set; }
        /// <summary>群组邀请码（网关启动日志可查）。</summary>
        public string InviteCode { get; set; }
        /// <summary>客户端名称。</summary>
        public string Name { get; set; } = "dotnet-client";
        /// <summary>连接模式：client（默认，主动开流）或 service（接收入向流）。</summary>
        public string Mode { get; set; } = Frames.ModeClient;
    }

    /// <summary>入网身份信息。</summary>
    public sealed class GatewayInfo
    {
        public string VirtualIP { get; internal set; }
        public string PeerID { get; internal set; }
        public string Group { get; internal set; }
        public string Mode { get; internal set; }
    }

    /// <summary>
    /// Lanet ws-gateway 异步客户端（Unity / .NET / MAUI 通用，零外部依赖）。
    /// <code>
    /// var client = await LanetGatewayClient.ConnectAsync(new GatewayOptions {
    ///     Url = "ws://host:8700/gateway", InviteCode = "XXXXXX" });
    /// var stream = await client.DialAsync("10.7.0.3", 8080);
    /// await stream.WriteStringAsync("ping");
    /// await stream.CloseWriteAsync();
    /// byte[] reply = await stream.ReadAllAsync();
    /// </code>
    /// </summary>
    public sealed class LanetGatewayClient : IDisposable
    {
        private readonly ClientWebSocket _ws;
        private readonly SemaphoreSlim _sendLock = new SemaphoreSlim(1, 1);
        private readonly ConcurrentDictionary<uint, GatewayStream> _streams
            = new ConcurrentDictionary<uint, GatewayStream>();
        private readonly CancellationTokenSource _cts = new CancellationTokenSource();

        private LanetGatewayClient(ClientWebSocket ws)
        {
            _ws = ws;
        }

        /// <summary>入网身份信息（鉴权成功后有效）。</summary>
        public GatewayInfo Info { get; private set; }

        /// <summary>网格入向流到达事件（service 模式）。</summary>
        public event Action<GatewayStream> OnStream;

        /// <summary>连接关闭事件。</summary>
        public event Action Closed;

        /// <summary>连接错误事件。</summary>
        public event Action<Exception> OnError;

        /// <summary>连接网关并完成邀请码鉴权。</summary>
        public static async Task<LanetGatewayClient> ConnectAsync(
            GatewayOptions options, CancellationToken ct = default)
        {
            if (options == null) throw new ArgumentNullException(nameof(options));
            if (string.IsNullOrEmpty(options.Url)) throw new ArgumentException("Url 必填");
            if (string.IsNullOrEmpty(options.InviteCode)) throw new ArgumentException("InviteCode 必填");

            var ws = new ClientWebSocket();
            await ws.ConnectAsync(new Uri(options.Url), ct).ConfigureAwait(false);

            var client = new LanetGatewayClient(ws);
            var dialTcs = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);

            // 接收循环。
            _ = Task.Run(() => client.ReceiveLoop(dialTcs), CancellationToken.None);

            var authJson = "{\"invite_code\":\"" + EscapeJson(options.InviteCode)
                + "\",\"name\":\"" + EscapeJson(options.Name ?? "dotnet-client")
                + "\",\"mode\":\"" + (options.Mode ?? Frames.ModeClient) + "\"}";
            await client.SendFrameAsync(Frames.TypeAuth, 0,
                Encoding.UTF8.GetBytes(authJson), ct).ConfigureAwait(false);

            using (var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct))
            {
                timeout.CancelAfter(TimeSpan.FromSeconds(10));
                try
                {
                    await dialTcs.Task.ConfigureAwait(false);
                }
                catch (OperationCanceledException)
                {
                    client.Dispose();
                    throw new TimeoutException("网关鉴权超时");
                }
                catch
                {
                    client.Dispose();
                    throw;
                }
            }
            return client;
        }

        /// <summary>入网身份信息。</summary>
        public GatewayInfo GetInfo() => Info;

        /// <summary>
        /// 访问网格内 TCP 服务（经网关 PortFWD 转发到目标节点；
        /// 目标节点为 SDK 节点时指其本机端口）。
        /// </summary>
        public async Task<GatewayStream> DialAsync(string virtualIP, int port, CancellationToken ct = default)
        {
            var json = "{\"ip\":\"" + EscapeJson(virtualIP) + "\",\"port\":" + port + "}";
            return await DialCoreAsync(json, ct).ConfigureAwait(false);
        }

        /// <summary>打开自定义协议流（对端节点需注册了该协议处理器）。</summary>
        public async Task<GatewayStream> DialProtocolAsync(
            string virtualIP, string protocolId, CancellationToken ct = default)
        {
            var json = "{\"ip\":\"" + EscapeJson(virtualIP)
                + "\",\"protocol\":\"" + EscapeJson(protocolId) + "\"}";
            return await DialCoreAsync(json, ct).ConfigureAwait(false);
        }

        /// <summary>发送心跳（网关回 Pong）。</summary>
        public Task PingAsync(CancellationToken ct = default)
            => SendFrameAsync(Frames.TypePing, 0, new byte[] { 0 }, ct);

        private async Task<GatewayStream> DialCoreAsync(string json, CancellationToken ct)
        {
            uint id = NextStreamId();
            var stream = new GatewayStream(this, id, false);
            var tcs = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            stream.PendingDial = tcs;
            _streams[id] = stream;
            await SendFrameAsync(Frames.TypeDial, id, Encoding.UTF8.GetBytes(json), ct).ConfigureAwait(false);
            using (var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct))
            {
                timeout.CancelAfter(TimeSpan.FromSeconds(15));
                try
                {
                    await tcs.Task.ConfigureAwait(false);
                }
                catch (OperationCanceledException)
                {
                    ForgetStream(id);
                    throw new TimeoutException("开流超时");
                }
                catch
                {
                    ForgetStream(id);
                    throw;
                }
            }
            return stream;
        }

        private int _nextId;

        private uint NextStreamId()
        {
            return (uint)System.Threading.Interlocked.Increment(ref _nextId);
        }

        internal async Task SendFrameAsync(byte type, uint streamId, byte[] payload, CancellationToken ct)
        {
            var frame = Frames.Marshal(type, streamId, payload);
            await _sendLock.WaitAsync(ct).ConfigureAwait(false);
            try
            {
                await _ws.SendAsync(new ArraySegment<byte>(frame),
                    WebSocketMessageType.Binary, true, ct).ConfigureAwait(false);
            }
            finally
            {
                _sendLock.Release();
            }
        }

        internal void ForgetStream(uint streamId)
        {
            _streams.TryRemove(streamId, out _);
        }

        private async Task ReceiveLoop(TaskCompletionSource<bool> dialTcs)
        {
            var accumulate = new System.IO.MemoryStream();
            var buf = new byte[64 * 1024];
            try
            {
                while (!_cts.IsCancellationRequested && _ws.State == WebSocketState.Open)
                {
                    var result = await _ws.ReceiveAsync(new ArraySegment<byte>(buf), _cts.Token).ConfigureAwait(false);
                    if (result.MessageType == WebSocketMessageType.Close)
                    {
                        await _ws.CloseAsync(WebSocketCloseStatus.NormalClosure, null, CancellationToken.None).ConfigureAwait(false);
                        break;
                    }
                    accumulate.Write(buf, 0, result.Count);
                    if (!result.EndOfMessage) continue;
                    var message = accumulate.ToArray();
                    accumulate.SetLength(0);

                    var frame = Frames.Unmarshal(message);
                    HandleFrame(frame, dialTcs);
                }
            }
            catch (Exception ex)
            {
                OnError?.Invoke(ex);
            }
            dialTcs.TrySetException(new InvalidOperationException("网关连接在鉴权前关闭"));
            foreach (var kv in _streams)
            {
                kv.Value.MarkError(new InvalidOperationException("连接已关闭"));
            }
            Closed?.Invoke();
        }

        private void HandleFrame(Frame frame, TaskCompletionSource<bool> dialTcs)
        {
            switch (frame.Type)
            {
                case Frames.TypeAuthOk:
                {
                    var json = Encoding.UTF8.GetString(frame.Payload);
                    Info = ParseInfo(json);
                    dialTcs.TrySetResult(true);
                    break;
                }
                case Frames.TypeAuthErr:
                {
                    var message = ExtractError(frame.Payload);
                    dialTcs.TrySetException(new InvalidOperationException("网关鉴权失败: " + message));
                    break;
                }
                case Frames.TypeDialOk:
                {
                    if (_streams.TryGetValue(frame.StreamId, out var st))
                    {
                        var json = Encoding.UTF8.GetString(frame.Payload);
                        st.MarkOpen(json.Contains("\"via_relay\":true"));
                    }
                    break;
                }
                case Frames.TypeDialErr:
                {
                    if (_streams.TryGetValue(frame.StreamId, out var st))
                    {
                        st.MarkError(new InvalidOperationException(Encoding.UTF8.GetString(frame.Payload)));
                        ForgetStream(frame.StreamId);
                    }
                    break;
                }
                case Frames.TypeData:
                {
                    if (_streams.TryGetValue(frame.StreamId, out var st)) st.PushData(frame.Payload);
                    break;
                }
                case Frames.TypeClose:
                {
                    if (_streams.TryGetValue(frame.StreamId, out var st)) st.MarkEof();
                    break;
                }
                case Frames.TypeReset:
                {
                    if (_streams.TryGetValue(frame.StreamId, out var st))
                    {
                        st.MarkError(new InvalidOperationException("流被对端重置"));
                        ForgetStream(frame.StreamId);
                    }
                    break;
                }
                case Frames.TypeStreamOpen:
                {
                    var stream = new GatewayStream(this, frame.StreamId, true);
                    var json = Encoding.UTF8.GetString(frame.Payload);
                    stream.Protocol = ExtractString(json, "protocol");
                    stream.RemotePeer = ExtractString(json, "remote_peer");
                    _streams[frame.StreamId] = stream;
                    OnStream?.Invoke(stream);
                    break;
                }
                case Frames.TypePong:
                default:
                    break;
            }
        }

        private static GatewayInfo ParseInfo(string json)
        {
            return new GatewayInfo
            {
                VirtualIP = ExtractString(json, "virtual_ip"),
                PeerID = ExtractString(json, "peer_id"),
                Group = ExtractString(json, "group"),
                Mode = ExtractString(json, "mode")
            };
        }

        private static string ExtractError(byte[] payload)
        {
            try
            {
                var json = Encoding.UTF8.GetString(payload);
                var extracted = ExtractString(json, "error");
                return string.IsNullOrEmpty(extracted) ? json : extracted;
            }
            catch
            {
                return Encoding.UTF8.GetString(payload);
            }
        }

        private static string ExtractString(string json, string key)
        {
            // 轻量 JSON 字符串字段提取（字段值均为服务端可控的简单文本）。
            var needle = "\"" + key + "\":\"";
            int start = json.IndexOf(needle, StringComparison.Ordinal);
            if (start < 0) return null;
            start += needle.Length;
            var sb = new StringBuilder();
            while (start < json.Length)
            {
                char c = json[start];
                if (c == '\\' && start + 1 < json.Length)
                {
                    sb.Append(json[start + 1]);
                    start += 2;
                    continue;
                }
                if (c == '"') break;
                sb.Append(c);
                start++;
            }
            return sb.ToString();
        }

        private static string EscapeJson(string s)
        {
            if (s == null) return "";
            return s.Replace("\\", "\\\\").Replace("\"", "\\\"");
        }

        /// <inheritdoc />
        public void Dispose()
        {
            _cts.Cancel();
            try
            {
                _ws.Dispose();
            }
            catch
            {
                // 忽略
            }
        }
    }
}
