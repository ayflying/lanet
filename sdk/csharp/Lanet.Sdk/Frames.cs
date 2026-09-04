using System;

namespace Lanet.Sdk
{
    /// <summary>
    /// ws-gateway 帧协议（与 Go pkg/gatewayproto、JS frame.js 字节级一致）。
    /// 一条二进制 WebSocket 消息即一个帧：
    /// [type:1][streamID:4 大端][payload 长度:4 大端][payload]
    /// </summary>
    public static class Frames
    {
        public const byte TypeAuth = 0x01;        // c→g 鉴权：JSON {"invite_code","name","mode"}
        public const byte TypeAuthOk = 0x02;      // g→c 鉴权通过：JSON {"virtual_ip","peer_id","group"}
        public const byte TypeAuthErr = 0x03;     // g→c 鉴权失败：JSON {"error"}
        public const byte TypeDial = 0x04;        // c→g 开流：JSON {"ip","port","protocol"}
        public const byte TypeDialOk = 0x05;      // g→c 开流成功：payload JSON {"via_relay":bool}
        public const byte TypeDialErr = 0x06;     // g→c 开流失败：payload 错误文本
        public const byte TypeData = 0x07;        // 双向 流数据
        public const byte TypeClose = 0x08;       // 双向 半关闭写端
        public const byte TypeReset = 0x09;       // 双向 强制中止流
        public const byte TypePing = 0x0A;        // c→g 心跳
        public const byte TypePong = 0x0B;        // g→c 心跳应答
        public const byte TypeStreamOpen = 0x0C;  // g→c 入向流（service 模式）

        public const string ModeClient = "client";
        public const string ModeService = "service";

        internal const int HeaderSize = 9;

        /// <summary>编码帧。</summary>
        public static byte[] Marshal(byte type, uint streamId, byte[] payload)
        {
            payload = payload ?? Array.Empty<byte>();
            var buf = new byte[HeaderSize + payload.Length];
            buf[0] = type;
            WriteUInt32BE(buf, 1, streamId);
            WriteUInt32BE(buf, 5, (uint)payload.Length);
            Array.Copy(payload, 0, buf, HeaderSize, payload.Length);
            return buf;
        }

        /// <summary>解码一条完整的 WebSocket 消息。</summary>
        public static Frame Unmarshal(byte[] data)
        {
            if (data == null || data.Length < HeaderSize)
                throw new ArgumentException("帧不完整（不足 9 字节头）", nameof(data));
            int len = (int)ReadUInt32BE(data, 5);
            if (data.Length - HeaderSize < len)
                throw new ArgumentException($"帧不完整：需要 {len} 字节，实际 {data.Length - HeaderSize}");
            var payload = new byte[len];
            Array.Copy(data, HeaderSize, payload, 0, len);
            return new Frame(data[0], ReadUInt32BE(data, 1), payload);
        }

        internal static void WriteUInt32BE(byte[] buf, int offset, uint value)
        {
            buf[offset] = (byte)(value >> 24);
            buf[offset + 1] = (byte)(value >> 16);
            buf[offset + 2] = (byte)(value >> 8);
            buf[offset + 3] = (byte)value;
        }

        internal static uint ReadUInt32BE(byte[] buf, int offset)
        {
            return ((uint)buf[offset] << 24) | ((uint)buf[offset + 1] << 16)
                 | ((uint)buf[offset + 2] << 8) | buf[offset + 3];
        }
    }

    /// <summary>一个协议帧。</summary>
    public sealed class Frame
    {
        public byte Type { get; }
        public uint StreamId { get; }
        public byte[] Payload { get; }

        public Frame(byte type, uint streamId, byte[] payload)
        {
            Type = type;
            StreamId = streamId;
            Payload = payload;
        }
    }
}
