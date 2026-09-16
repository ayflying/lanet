using System;
using System.Collections.Concurrent;
using System.Threading;
using UnityEngine;

namespace Lanet.Sdk
{
    /// <summary>
    /// 主线程泵：把网关客户端后台线程的事件搬回 Unity 主线程。
    ///
    /// 为什么必须有它：<see cref="LanetGatewayClient"/> 的 <c>OnStream</c> /
    /// <c>Closed</c> / <c>OnError</c> 都在 WebSocket 接收循环（线程池线程）里触发，
    /// 而 Unity 的 API（<c>GameObject</c>、<c>Transform</c>、UI、<c>Debug.Log</c> 之外的
    /// 大部分引擎调用）只能在主线程碰——直接在里面写业务代码会抛
    /// <c>UnityException: … can only be called from the main thread</c>。
    ///
    /// 不需要手动摆到场景里：第一次访问 <see cref="Instance"/>（或任何
    /// <see cref="Enqueue"/> 调用）会自动创建一个隐藏且不销毁的 GameObject。
    /// </summary>
    [AddComponentMenu("")]
    public sealed class LanetDispatcher : MonoBehaviour
    {
        /// <summary>每帧最多处理的任务数，防止后台事件堆积时卡住渲染帧。</summary>
        private const int MaxTasksPerFrame = 200;

        private static LanetDispatcher _instance;

        private readonly ConcurrentQueue<Action> _queue = new ConcurrentQueue<Action>();
        private int _mainThreadId;

        /// <summary>全局单例（首次访问时自动创建）。</summary>
        public static LanetDispatcher Instance
        {
            get
            {
                if (_instance == null) Create();
                return _instance;
            }
        }

        /// <summary>当前是否处于 Unity 主线程。</summary>
        public static bool IsMainThread
            => Instance._mainThreadId == Thread.CurrentThread.ManagedThreadId;

        [RuntimeInitializeOnLoadMethod(RuntimeInitializeLoadType.BeforeSceneLoad)]
        private static void Bootstrap()
        {
            // 静态字段可能残留上一轮播放模式的对象引用，先彻底清掉再重建。
            if (_instance != null)
            {
                DestroyImmediate(_instance.gameObject);
                _instance = null;
            }
            Create();
        }

        private static void Create()
        {
            if (_instance != null) return;
            var go = new GameObject("[LanetDispatcher]") { hideFlags = HideFlags.HideAndDontSave };
            DontDestroyOnLoad(go);
            _instance = go.AddComponent<LanetDispatcher>();
        }

        private void Awake()
        {
            _mainThreadId = Thread.CurrentThread.ManagedThreadId;
            _instance = this;
        }

        /// <summary>
        /// 把动作派发到主线程执行；若调用时已在主线程则立即执行（不引入额外一帧延迟）。
        /// 动作内部抛出的异常会被捕获并打日志，不会中断队列里后续任务。
        /// </summary>
        public static void Enqueue(Action action)
        {
            if (action == null) return;
            var inst = Instance;
            if (inst._mainThreadId == Thread.CurrentThread.ManagedThreadId)
            {
                Invoke(action);
                return;
            }
            inst._queue.Enqueue(action);
        }

        private void Update()
        {
            int budget = MaxTasksPerFrame;
            while (budget-- > 0 && _queue.TryDequeue(out var action))
            {
                Invoke(action);
            }
        }

        private static void Invoke(Action action)
        {
            try
            {
                action();
            }
            catch (Exception ex)
            {
                Debug.LogException(ex);
            }
        }
    }
}
