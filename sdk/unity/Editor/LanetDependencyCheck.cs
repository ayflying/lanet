using System;
using System.Collections.Generic;
using System.IO;
using System.Text;
using UnityEditor;
using UnityEngine;

namespace Lanet.Sdk.EditorTools
{
    /// <summary>
    /// 菜单 <c>Lanet</c> 下的自检与脚手架工具。
    ///
    /// 存在的理由：Unity 里接入原生插件最常见的三个失败点是**构建平台不是 Android**、
    /// **AAR 没进 Plugins/Android**、**架构没勾 ARM64**，而这三者在编辑器里都不会报错，
    /// 只在真机启动时才以「找不到类」的形式暴露。这里提前把它们查出来。
    /// </summary>
    public static class LanetDependencyCheck
    {
        /// <summary>包根候选路径：UPM 引用、嵌进 Assets 的常见位置。</summary>
        private static readonly string[] CandidateRoots =
        {
            "Packages/com.lanet.unity",
            "Assets/Lanet",
            "Assets/Plugins/Lanet",
            "Assets/LanetUnity",
        };

        private const string PluginDirSuffix = "Runtime/Plugins/Android";
        private const string PluginAarName = "lanet-plugin.aar";

        [MenuItem("Lanet/检查依赖与配置", priority = 0)]
        public static void CheckAll()
        {
            var sb = new StringBuilder("[Lanet] 依赖自检\n");
            int problems = 0;

            // 1. 构建平台
            var target = EditorUserBuildSettings.activeBuildTarget;
            if (target == BuildTarget.Android)
            {
                sb.AppendLine("  ✅ 构建平台：Android");
            }
            else
            {
                sb.AppendLine($"  ⚠️ 构建平台是 {target}：本地节点（链路 2）只在 Android 上生效，");
                sb.AppendLine("      切到 Android 后再验证。网关链路（链路 1）不受影响。");
                problems++;
            }

            // 2. 原生 AAR
            var aarPath = FindPluginAar();
            if (aarPath != null)
            {
                var size = new FileInfo(FullPath(aarPath)).Length / 1024 / 1024;
                sb.AppendLine($"  ✅ 原生插件：{aarPath}（{size} MB）");
                var abiReport = DescribeAbis(aarPath);
                if (abiReport != null) sb.AppendLine($"     内含架构：{abiReport}");
            }
            else
            {
                sb.AppendLine($"  ❌ 未找到 {PluginAarName}");
                sb.AppendLine($"      需要 {PluginDirSuffix}/ 下同时有 lanet-plugin.aar 与 lanet.aar；");
                sb.AppendLine("      直接跑 `python sdk/android-plugin/build.py` 会让构建脚本自动同步这两个文件。");
                problems++;
            }

            // 3. minSdk（AAR 要求 21+）
            var minSdk = (int)PlayerSettings.Android.minSdkVersion;
            if (minSdk >= 21)
            {
                sb.AppendLine($"  ✅ Android minSdk：{minSdk}");
            }
            else
            {
                sb.AppendLine($"  ❌ Android minSdk 是 {minSdk}，AAR 要求 21+（Player Settings → Other Settings）");
                problems++;
            }

            // 4. 目标架构（Google Play 要求 ARM64）
            var archs = PlayerSettings.Android.targetArchitectures;
            bool hasArm64 = (archs & AndroidArchitecture.ARM64) != 0;
            bool hasArmV7 = (archs & AndroidArchitecture.ARMv7) != 0;
            if (hasArm64)
            {
                sb.AppendLine($"  ✅ 目标架构：{archs}");
            }
            else
            {
                sb.AppendLine($"  ⚠️ 目标架构 {archs} 不含 ARM64：真机（尤其 64 位）可能装不上，");
                sb.AppendLine("      Google Play 也强制要求 ARM64。勾上 ARM64 后重新构建。");
                problems++;
            }
            if ((archs & AndroidArchitecture.X86_64) != 0)
            {
                sb.AppendLine("  ⚠️ 勾了 x86_64：lanet 的 AAR 里没有 x86_64 产物（只编了 armeabi-v7a/arm64-v8a/x86），");
                sb.AppendLine("      带 x86_64 出包会在该架构上缺符号。除非要跑 x86 模拟器，否则去掉它。");
            }
            if (hasArmV7 && hasArm64)
            {
                sb.AppendLine("  ℹ️ 同时含 ARMv7 + ARM64 是推荐配置。");
            }

            // 5. 网关链路的运行时依赖
            sb.AppendLine("  ℹ️ 链路 1（网关）需要 System.Net.WebSockets，Windows/macOS/Android/iOS 都自带；");
            sb.AppendLine("      WebGL 不支持，该平台只能用链路 2 之外的方案。");

            if (problems == 0)
            {
                sb.AppendLine("  ────────────────\n  结论：配置完整，可以构建。");
                Debug.Log(sb.ToString());
            }
            else
            {
                sb.AppendLine($"  ────────────────\n  结论：{problems} 项待处理（见上方 ❌/⚠️）。");
                Debug.LogWarning(sb.ToString());
            }
        }

        [MenuItem("Lanet/创建 LanetManager 到场景", priority = 20)]
        public static void CreateManager()
        {
            var existing = FindManager();
            if (existing != null)
            {
                EditorUtility.DisplayDialog("Lanet", "场景里已存在 LanetManager。", "好");
                Selection.activeGameObject = existing.gameObject;
                EditorGUIUtility.PingObject(existing.gameObject);
                return;
            }

            var go = new GameObject("LanetManager");
            var mgr = go.AddComponent<LanetManager>();
            Undo.RegisterCreatedObjectUndo(go, "Create LanetManager");
            Selection.activeGameObject = go;
            EditorGUIUtility.PingObject(go);
            Debug.Log("[Lanet] 已创建 LanetManager。\n" +
                      "  · 链路 1（网关）：填 gatewayUrl 与 inviteCode，勾 useGateway。\n" +
                      "  · 链路 2（Android 本地节点）：填 networkKey 与 bootstrap，勾 useAndroidNode。\n" +
                      "  注意 gatewayUrl 在真机上不能再用 127.0.0.1 指本机——那是手机自己。");
        }

        [MenuItem("Lanet/打开接入文档", priority = 40)]
        public static void OpenReadme()
        {
            var root = FindPackageRoot();
            var path = root == null ? null : FullPath(root + "/README.md");
            if (path != null && File.Exists(path))
            {
                AssetDatabase.OpenAsset(AssetDatabase.LoadAssetAtPath<TextAsset>(root + "/README.md"));
                return;
            }
            Debug.Log("[Lanet] 未在工程内找到包 README；仓库路径为 sdk/unity/README.md。");
        }

        // ------------------------------------------------------------------

        /// <summary>
        /// 找场景里的 LanetManager。2022.2 起 <c>FindObjectOfType</c> 被标记过时、
        /// 改成 <c>FindFirstObjectByType</c>，这里按版本分支，兼容 2021.3 基线。
        /// </summary>
        private static LanetManager FindManager()
        {
#if UNITY_2022_2_OR_NEWER
            return UnityEngine.Object.FindFirstObjectByType<LanetManager>();
#else
            return UnityEngine.Object.FindObjectOfType<LanetManager>();
#endif
        }

        private static string FindPackageRoot()
        {
            foreach (var root in CandidateRoots)
            {
                if (AssetDatabase.IsValidFolder(root) &&
                    AssetDatabase.IsValidFolder($"{root}/{PluginDirSuffix}"))
                {
                    return root;
                }
            }
            // 退一步：只要目录存在就算（可能用户把 AAR 放别处、只想看文档）
            foreach (var root in CandidateRoots)
            {
                if (AssetDatabase.IsValidFolder(root)) return root;
            }
            return null;
        }

        private static string FindPluginAar()
        {
            foreach (var root in CandidateRoots)
            {
                var rel = $"{root}/{PluginDirSuffix}/{PluginAarName}";
                if (File.Exists(FullPath(rel))) return rel;
            }
            // 兜底：全工程搜一遍（用户可能自定义了位置）
            var guids = AssetDatabase.FindAssets("lanet-plugin t:DefaultAsset");
            foreach (var guid in guids)
            {
                var p = AssetDatabase.GUIDToAssetPath(guid);
                if (p.EndsWith(PluginAarName, StringComparison.OrdinalIgnoreCase)) return p;
            }
            return null;
        }

        /// <summary>把 GAssets 路径转成磁盘绝对路径（Assets/ 与 Packages/ 前缀都要处理）。</summary>
        private static string FullPath(string assetPath)
        {
            if (assetPath.StartsWith("Assets/", StringComparison.Ordinal))
            {
                return Path.Combine(Directory.GetParent(Application.dataPath).FullName,
                    assetPath.Substring("Assets/".Length));
            }
            if (assetPath.StartsWith("Packages/", StringComparison.Ordinal))
            {
                var pkg = UnityEditor.PackageManager.PackageInfo.FindForAssetPath(assetPath);
                if (pkg != null)
                {
                    var rel = assetPath.Substring($"Packages/{pkg.name}/".Length);
                    return Path.Combine(pkg.resolvedPath, rel);
                }
            }
            return assetPath;
        }

        /// <summary>读 AAR 里的 jni 目录名，列出实际包含的 ABI。</summary>
        private static string DescribeAbis(string aarAssetPath)
        {
            try
            {
                var abis = new List<string>();
                using (var zip = System.IO.Compression.ZipFile.OpenRead(FullPath(aarAssetPath)))
                {
                    foreach (var entry in zip.Entries)
                    {
                        var parts = entry.FullName.Split('/');
                        if (parts.Length >= 3 && parts[0] == "jni" && !abis.Contains(parts[1]))
                        {
                            abis.Add(parts[1]);
                        }
                    }
                }
                abis.Sort(StringComparer.Ordinal);
                return abis.Count == 0 ? "（未发现 jni/，可能不是有效 AAR）" : string.Join(" / ", abis);
            }
            catch (Exception ex)
            {
                return "（读取失败：" + ex.Message + "）";
            }
        }
    }
}
