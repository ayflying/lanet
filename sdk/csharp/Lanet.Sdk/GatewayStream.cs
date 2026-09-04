using System;
using System.Collections.Generic;
using System.Text;
using System.Threading;
using System.Threading.Tasks;

namespace Lanet.Sdk
{
    /// <summary>
    /// 一条双向流（网关侧网格流的客户端视图）。
    /// 典型用法：Write → CloseWrite → 循环 Read 直到返回 0（对端 EOF）。
    /// </summary>
    public sealed class GatewayStream : IDisposable
    {
        private readonly object _lock = new object();
        private readonly Queue<byte[]> _buffer = new Queue<byte[]>();
        private TaskCompletionSource<bool> _signal = CreateTcs();
        private readonly uint _streamId;
        private readonly LanetGatewayClient _client;
        private bool _localWriteClosed;
        private bool _eofFired;   // 对端已半关闭（读端 EOF）
        private bool _aborted;    // 本端/对端已中止
        private Exception _error;

        internal GatewayStream(LanetGatewayClient client, uint streamId, bool inbound)
        {
            _client = client;
            _streamId = streamId;
            IsInbound = inbound;
        }

        internal TaskCompletionSource<bool> PendingDial { get; set; }

        /// <summary>流 ID。</summary>
        public uint StreamId => _streamId;

        /// <summary>是否为网格入向流（service 模式收到）。</summary>
        public bool IsInbound { get; }

        /// <summary>开流是否经中继（dial 成功后有效）。</summary>
        public bool ViaRelay { get; internal set; }

        /// <summary>协议 ID（入向流有效）。</summary>
        public string Protocol { get; internal set; }

        /// <summary>对端 PeerID（入向流有效）。</summary>
        public string RemotePeer { get; internal set; }

        /// <summary>写数据（网关转发到网格流）。</summary>
        public Task WriteAsync(byte[] data, CancellationToken ct = default)
        {
            lock (_lock)
            {
                if (_localWriteClosed) throw new InvalidOperationException("写端已关闭");
                if (_error != null) throw _error;
                if (_aborted) throw new InvalidOperationException("流已中止");
            }
            return _client.SendFrameAsync(Frames.TypeData, _streamId, data, ct);
        }

        /// <summary>写文本（UTF-8）。</summary>
        public Task WriteStringAsync(string text, CancellationToken ct = default)
            => WriteAsync(Encoding.UTF8.GetBytes(text), ct);

        /// <summary>半关闭写端：对端读到 EOF。本端仍可继续读取。</summary>
        public Task CloseWriteAsync(CancellationToken ct = default)
        {
            lock (_lock)
            {
                if (_localWriteClosed) return Task.CompletedTask;
                _localWriteClosed = true;
            }
            return _client.SendFrameAsync(Frames.TypeClose, _streamId, null, ct);
        }

        /// <summary>
        /// 读取数据到 buffer，返回读取字节数；返回 0 表示对端 EOF。
        /// </summary>
        public async Task<int> ReadAsync(byte[] buffer, int offset, int count, CancellationToken ct = default)
        {
            if (buffer == null) throw new ArgumentNullException(nameof(buffer));
            while (true)
            {
                TaskCompletionSource<bool> wait;
                int n;
                lock (_lock)
                {
                    n = TakeChunkLocked(buffer, offset, count, out _);
                    if (n > 0) return n;
                    if (_eofFired || _aborted) return 0;
                    if (_error != null) throw _error;
                    if (_signal.Task.IsCompleted) _signal = CreateTcs();
                    wait = _signal;
                }
                await wait.Task.ConfigureAwait(false);
                ct.ThrowIfCancellationRequested();
            }
        }

        /// <summary>读取全部数据直到对端 EOF（适合 echo/请求-响应等小数据场景）。</summary>
        public async Task<byte[]> ReadAllAsync(CancellationToken ct = default)
        {
            var chunks = new List<byte[]>();
            var buf = new byte[16 * 1024];
            while (true)
            {
                int n = await ReadAsync(buf, 0, buf.Length, ct).ConfigureAwait(false);
                if (n == 0) break;
                var part = new byte[n];
                Array.Copy(buf, part, n);
                chunks.Add(part);
            }
            return Concat(chunks);
        }

        /// <summary>强制中止流。</summary>
        public void Abort()
        {
            bool send;
            lock (_lock)
            {
                if (_aborted) return;
                _aborted = true;
                SignalLocked();
                send = !_localWriteClosed;
            }
            _client.ForgetStream(_streamId);
            if (send)
            {
                _ = _client.SendFrameAsync(Frames.TypeReset, _streamId, null, default);
            }
        }

        /// <inheritdoc />
        public void Dispose() => Abort();

        // ---- 内部：由客户端接收循环调用 ----

        internal void MarkOpen(bool viaRelay)
        {
            ViaRelay = viaRelay;
            PendingDial?.TrySetResult(true);
        }

        internal void MarkError(Exception err)
        {
            lock (_lock)
            {
                if (_error != null) return;
                _error = err;
                SignalLocked();
            }
            PendingDial?.TrySetException(err);
        }

        internal void PushData(byte[] data)
        {
            lock (_lock)
            {
                _buffer.Enqueue(data);
                SignalLocked();
            }
        }

        internal void MarkEof()
        {
            lock (_lock)
            {
                if (_eofFired) return;
                _eofFired = true;
                SignalLocked();
            }
        }

        private int TakeChunkLocked(byte[] dst, int offset, int count, out bool eof)
        {
            eof = false;
            if (_buffer.Count == 0) return 0;
            var head = _buffer.Peek();
            int n = Math.Min(count, head.Length);
            Array.Copy(head, 0, dst, offset, n);
            if (n < head.Length)
            {
                var rest = new byte[head.Length - n];
                Array.Copy(head, n, rest, 0, rest.Length);
                _buffer.Dequeue();
                _buffer.Enqueue(rest);
            }
            else
            {
                _buffer.Dequeue();
            }
            return n;
        }

        private void SignalLocked()
        {
            if (!_signal.Task.IsCompleted) _signal.TrySetResult(true);
        }

        private static byte[] Concat(List<byte[]> chunks)
        {
            int total = 0;
            foreach (var c in chunks) total += c.Length;
            var outBuf = new byte[total];
            int off = 0;
            foreach (var c in chunks)
            {
                Array.Copy(c, 0, outBuf, off, c.Length);
                off += c.Length;
            }
            return outBuf;
        }

        private static TaskCompletionSource<bool> CreateTcs()
            => new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
    }
}
