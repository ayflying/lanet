using System;
using System.Text;
using System.Threading.Tasks;
using Lanet.Sdk;

namespace Lanet.Demo
{
    /// <summary>
    /// ws-gateway 互通演示：鉴权 → 自定义协议 echo → PortFWD TCP 回显。
    /// 用法：dotnet run -- <gatewayWsUrl> <inviteCode> <targetVirtualIP>
    /// </summary>
    internal static class Program
    {
        private static async Task<int> Main(string[] args)
        {
            if (args.Length < 3)
            {
                Console.WriteLine("用法: dotnet run -- <ws://gateway:8700/gateway> <inviteCode> <targetVirtualIP>");
                return 2;
            }
            var url = args[0];
            var invite = args[1];
            var target = args[2];

            Console.WriteLine("== 连接网关并鉴权 ==");
            using var client = await LanetGatewayClient.ConnectAsync(new GatewayOptions
            {
                Url = url,
                InviteCode = invite,
                Name = "dotnet-demo"
            });
            Console.WriteLine($"PASS: 鉴权通过 group={client.Info.Group} 网关身份IP={client.Info.VirtualIP}");

            // 1. 自定义协议流：对端 /pvn/tunnel/1.0.0（echo 服务）。
            var t0 = DateTime.UtcNow;
            var echo = await client.DialProtocolAsync(target, "/pvn/tunnel/1.0.0");
            await echo.WriteStringAsync("hello from .NET");
            await echo.CloseWriteAsync();
            var echoed = Encoding.UTF8.GetString(await echo.ReadAllAsync());
            Check(echoed == "hello from .NET",
                $"自定义协议 echo 往返 {(long)(DateTime.UtcNow - t0).TotalMilliseconds}ms（viaRelay={echo.ViaRelay}）");

            // 2. 端口转发：目标节点本机 9999 的 TCP 回显服务。
            var t1 = DateTime.UtcNow;
            var pf = await client.DialAsync(target, 9999);
            await pf.WriteStringAsync("portfwd from .NET");
            await pf.CloseWriteAsync();
            var back = Encoding.UTF8.GetString(await pf.ReadAllAsync());
            Check(back == "portfwd from .NET",
                $"PortFWD -> {target}:9999 TCP 回显往返 {(long)(DateTime.UtcNow - t1).TotalMilliseconds}ms（viaRelay={pf.ViaRelay}）");

            // 3. 心跳。
            await client.PingAsync();
            Check(true, "心跳 Ping 已发送");

            Console.WriteLine("\n=== ws-gateway .NET 客户端全链路 PASS ===");
            return 0;
        }

        private static void Check(bool ok, string label)
        {
            Console.WriteLine($"{(ok ? "PASS" : "FAIL")}: {label}");
            if (!ok) Environment.Exit(1);
        }
    }
}
