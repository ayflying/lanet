using System;
using System.Text;
using System.Threading;
using System.Threading.Tasks;

namespace Lanet.Sdk
{
    /// <summary>
    /// <see cref="GatewayStream"/> 的便捷扩展：请求-响应、流式读循环、按行分帧，
    /// 以及把后台任务的结果搬回主线程。
    ///
    /// 线程约定：这些方法内部一律 <c>ConfigureAwait(false)</c>，所以**回调在
    /// 线程池线程上执行**，不要直接在里面碰 Unity API；需要时用
    /// <see cref="ContinueOnMainThread{T}"/> 或 <see cref="LanetDispatcher.Enqueue"/>。
    /// </summary>
    public static class LanetStreamExtensions
    {
        /// <summary>
        /// 一问一答：写入 → 半关闭写端 → 读到对端 EOF，返回 UTF-8 文本。
        /// 适合 echo、HTTP-over-lanet 这类小数据场景。
        /// </summary>
        public static async Task<string> RequestStringAsync(
            this GatewayStream stream, string request, CancellationToken ct = default)
        {
            if (stream == null) throw new ArgumentNullException(nameof(stream));
            if (!string.IsNullOrEmpty(request))
            {
                await stream.WriteStringAsync(request, ct).ConfigureAwait(false);
            }
            // 半关闭是唯一 EOF 语义：不调它，对端会一直等数据，ReadAllAsync 永不返回。
            await stream.CloseWriteAsync(ct).ConfigureAwait(false);
            var reply = await stream.ReadAllAsync(ct).ConfigureAwait(false);
            return Encoding.UTF8.GetString(reply);
        }

        /// <summary>一问一答的二进制版本。</summary>
        public static async Task<byte[]> RequestAsync(
            this GatewayStream stream, byte[] request, CancellationToken ct = default)
        {
            if (stream == null) throw new ArgumentNullException(nameof(stream));
            if (request != null && request.Length > 0)
            {
                await stream.WriteAsync(request, ct).ConfigureAwait(false);
            }
            await stream.CloseWriteAsync(ct).ConfigureAwait(false);
            return await stream.ReadAllAsync(ct).ConfigureAwait(false);
        }

        /// <summary>
        /// 只写入不等待回复（fire-and-forget 的可靠版：异常会向调用方抛出）。
        /// </summary>
        public static async Task SendStringAsync(
            this GatewayStream stream, string text, bool closeWrite = false, CancellationToken ct = default)
        {
            if (stream == null) throw new ArgumentNullException(nameof(stream));
            if (!string.IsNullOrEmpty(text))
            {
                await stream.WriteStringAsync(text, ct).ConfigureAwait(false);
            }
            if (closeWrite)
            {
                await stream.CloseWriteAsync(ct).ConfigureAwait(false);
            }
        }

        /// <summary>
        /// 流式读循环：每读到一个分片回调一次（<c>count</c> 为本次字节数），
        /// 对端 EOF 时自然结束。
        ///
        /// 注意数据是**字节流、没有消息边界**，<c>count</c> 与业务消息不对应，
        /// 需要自己按应用协议分帧（或改用 <see cref="ReadLinesAsync"/>）。
        /// </summary>
        public static async Task ReadLoopAsync(
            this GatewayStream stream,
            Action<byte[], int> onChunk,
            CancellationToken ct = default,
            int bufferSize = 16 * 1024)
        {
            if (stream == null) throw new ArgumentNullException(nameof(stream));
            if (onChunk == null) throw new ArgumentNullException(nameof(onChunk));
            if (bufferSize <= 0) bufferSize = 16 * 1024;

            var buffer = new byte[bufferSize];
            while (true)
            {
                int n = await stream.ReadAsync(buffer, 0, buffer.Length, ct).ConfigureAwait(false);
                if (n == 0) break; // 对端 CloseWrite → EOF
                onChunk(buffer, n);
            }
        }

        /// <summary>流式读循环的 UTF-8 文本版本（分片按 UTF-8 整体解码）。</summary>
        public static Task ReadLoopStringAsync(
            this GatewayStream stream,
            Action<string> onChunk,
            CancellationToken ct = default,
            int bufferSize = 16 * 1024)
        {
            if (onChunk == null) throw new ArgumentNullException(nameof(onChunk));
            return stream.ReadLoopAsync(
                (buf, n) => onChunk(Encoding.UTF8.GetString(buf, 0, n)),
                ct,
                bufferSize);
        }

        /// <summary>
        /// 按行读取：以 <c>\n</c> 分帧、自动去掉行尾 <c>\r</c>，每行回调一次。
        /// 这是给「应用层懒得自己分帧」的简易约定，两端都用它时才成立。
        /// </summary>
        public static async Task ReadLinesAsync(
            this GatewayStream stream,
            Action<string> onLine,
            CancellationToken ct = default,
            int bufferSize = 16 * 1024)
        {
            if (onLine == null) throw new ArgumentNullException(nameof(onLine));

            var pending = new StringBuilder();
            await stream.ReadLoopStringAsync(chunk =>
            {
                pending.Append(chunk);
                while (true)
                {
                    string text = pending.ToString();
                    int idx = text.IndexOf('\n');
                    if (idx < 0) break;
                    string line = text.Substring(0, idx);
                    pending.Clear().Append(text, idx + 1, text.Length - idx - 1);
                    onLine(line.EndsWith("\r", StringComparison.Ordinal)
                        ? line.Substring(0, line.Length - 1)
                        : line);
                }
            }, ct, bufferSize).ConfigureAwait(false);

            // 对端最后一个字符后没有换行时，把残留内容也交出去。
            if (pending.Length > 0)
            {
                onLine(pending.ToString());
            }
        }

        /// <summary>
        /// 把后台 Task 的结果搬回主线程再回调——需要碰 Unity API 时用这个，
        /// 不必自己包 <see cref="LanetDispatcher.Enqueue"/>。
        /// </summary>
        public static void ContinueOnMainThread<T>(
            this Task<T> task, Action<T> onSuccess, Action<Exception> onError = null)
        {
            if (task == null) throw new ArgumentNullException(nameof(task));
            task.ContinueWith(t =>
            {
                if (t.IsFaulted)
                {
                    var ex = t.Exception?.GetBaseException() ?? new Exception("未知错误");
                    LanetDispatcher.Enqueue(() => onError?.Invoke(ex));
                    return;
                }
                if (t.IsCanceled)
                {
                    LanetDispatcher.Enqueue(() => onError?.Invoke(new TaskCanceledException("操作已取消")));
                    return;
                }
                var value = t.Result;
                LanetDispatcher.Enqueue(() => onSuccess?.Invoke(value));
            }, TaskScheduler.Default);
        }

        /// <summary>无返回值的版本。</summary>
        public static void ContinueOnMainThread(
            this Task task, Action onSuccess = null, Action<Exception> onError = null)
        {
            if (task == null) throw new ArgumentNullException(nameof(task));
            task.ContinueWith(t =>
            {
                if (t.IsFaulted)
                {
                    var ex = t.Exception?.GetBaseException() ?? new Exception("未知错误");
                    LanetDispatcher.Enqueue(() => onError?.Invoke(ex));
                    return;
                }
                if (t.IsCanceled)
                {
                    LanetDispatcher.Enqueue(() => onError?.Invoke(new TaskCanceledException("操作已取消")));
                    return;
                }
                LanetDispatcher.Enqueue(() => onSuccess?.Invoke());
            }, TaskScheduler.Default);
        }
    }
}
