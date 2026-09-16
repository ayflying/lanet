#!/usr/bin/env python3
"""把 lanet 编成 uni-app 原生插件（nativeplugin）包。

一条命令走完全程：

    python sdk/android-plugin/build.py

产出 `sdk/android-plugin/lanet-vpn/`，把它整个拷进 uni-app 项目的
`nativeplugins/` 目录即可（HBuilderX 云打包会自动引用）。

步骤：
  1. gomobile 编 lanet.aar（三 ABI：armeabi-v7a / arm64-v8a / x86）
  2. 从 lanet.aar 抽出 classes.jar —— library 模块不能直接依赖本地 .aar
  3. gradle 编插件 module 并自检（verifyNoLeak：插件 aar 里不能有宿主类）
  4. 校验交付包完整性

改插件 Kotlin 代码时用 `--skip-aar` 跳过第 1 步，能省两分钟。
"""

from __future__ import annotations

import argparse
import hashlib
import os
import re
import shutil
import subprocess
import sys
import zipfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
PLUGIN_PKG = HERE / "lanet-vpn"                 # 交付的 nativeplugin 包
PLUGIN_ANDROID = PLUGIN_PKG / "android"
PLUGIN_PKG_JSON = PLUGIN_PKG / "package.json"
PLUGIN_SRC = HERE / "plugin"                    # 插件 module 的 gradle 工程
MODULE_DIR = PLUGIN_SRC / "lanet-plugin"
REPO_ROOT = HERE.parent.parent                  # 仓库根
MOBILE_MODULE = REPO_ROOT / "sdk" / "go" / "mobile"
AAR_OUT = MOBILE_MODULE / "lanet.aar"           # gomobile 产出
AAR_DST = PLUGIN_ANDROID / "lanet.aar"          # 放进交付包
CLASSES_JAR = MODULE_DIR / "libs" / "lanet-classes.jar"
PLUGIN_AAR = PLUGIN_ANDROID / "lanet-plugin.aar"

# ABI 必须是 DCloud 云端打包的白名单：只有 armeabi-v7a / arm64-v8a / x86。
# 云端没有 x86_64 —— 编了也进不了包。gomobile 里对应 arm / arm64 / 386。
GOMOBILE_TARGETS = "android/arm,android/arm64,android/386"
EXPECTED_ABIS = ["armeabi-v7a", "arm64-v8a", "x86"]

# 原生插件 API 的所属包名，出现在插件 aar 里就说明宿主类被误打进去了
FORBIDDEN_PREFIXES = ("io/dcloud/", "com/taobao/weex/", "com/lanet/mobile/")


def log(msg: str = "") -> None:
    print(msg, flush=True)


def step(msg: str) -> None:
    log()
    log(f"==> {msg}")


def die(msg: str) -> None:
    log(f"\n[失败] {msg}")
    sys.exit(1)


def sha16(p: Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()[:16]


def find_gradle_launcher() -> Path:
    """定位 Gradle 的 launcher jar。

    沙箱/受限环境里 gradle.bat 跑不起来（禁 cmd.exe），所以直接调 Java 主类。
    """
    dists = Path.home() / ".gradle" / "wrapper" / "dists"
    if dists.is_dir():
        for jar in sorted(dists.glob("gradle-*/**/lib/gradle-launcher-*.jar")):
            return jar
    die(f"找不到 gradle-launcher jar（已查找 {dists}）—— 先让 sdk/android 成功构建过一次")


def run(cmd: list[str], cwd: Path, env: dict, tail: int = 40) -> tuple[int, str]:
    log("   $ " + " ".join(str(c) for c in cmd[:3]) + " …")
    p = subprocess.run(
        cmd, cwd=str(cwd), env=env,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        text=True, encoding="utf-8", errors="replace",
    )
    out = p.stdout or ""
    lines = out.splitlines()
    if lines:
        for line in lines[-tail:]:
            log("   | " + line)
    return p.returncode, out


def build_env() -> dict:
    env = dict(os.environ)
    java_home = env.get("JAVA_HOME") or r"D:\Program Files\Java\jdk-21"
    if not Path(java_home).is_dir():
        die(f"JAVA_HOME 无效：{java_home}")
    env["JAVA_HOME"] = java_home
    # javac 必须在 PATH —— gomobile bind 最后一步要调它，否则白等几分钟才报错
    env["PATH"] = str(Path(java_home) / "bin") + os.pathsep + env.get("PATH", "")
    env.setdefault("ANDROID_HOME", r"D:\android-toolchain\sdk")
    env.setdefault("ANDROID_NDK_HOME", r"D:\android-toolchain\ndk")
    gopath_bin = Path(env.get("GOPATH", str(Path.home() / "go"))) / "bin"
    if gopath_bin.is_dir():
        env["PATH"] = str(gopath_bin) + os.pathsep + env["PATH"]
    return env


# ---------------------------------------------------------------------------


def check_prereqs(env: dict) -> None:
    step("前置检查")
    launcher = find_gradle_launcher()
    log(f"   gradle launcher : {launcher}")
    log(f"   JAVA_HOME       : {env['JAVA_HOME']}")
    log(f"   ANDROID_HOME    : {env['ANDROID_HOME']}")

    gomobile = shutil.which("gomobile", path=env["PATH"])
    log(f"   gomobile        : {gomobile or '（不在 PATH，--skip-aar 时可忽略）'}")

    props = (PLUGIN_SRC / "gradle.properties").read_text(encoding="utf-8")
    m = re.search(r"^dcloudSdkDir=(.+)$", props, re.M)
    if not m:
        die("gradle.properties 里没有 dcloudSdkDir")
    dcloud = Path(m.group(1).strip())
    jar = dcloud / "dc_weexsdk-release.jar"
    if not jar.is_file():
        die(f"找不到 DCloud SDK：{jar}\n      核对 gradle.properties 的 dcloudSdkDir")
    log(f"   DCloud SDK      : {jar}  ({jar.stat().st_size / 1048576:.1f}MB)")


def build_aar(env: dict) -> None:
    step("① gomobile 编 lanet.aar（三 ABI）")
    if AAR_OUT.exists():
        AAR_OUT.unlink()
    cmd = [
        "gomobile", "bind",
        f"-target={GOMOBILE_TARGETS}",
        "-androidapi", "21",
        "-javapkg=com.lanet",
        "-ldflags=-checklinkname=0 -s -w",
        "-o", "lanet.aar", ".",
    ]
    rc, _ = run(cmd, MOBILE_MODULE, env, tail=25)
    if rc != 0 or not AAR_OUT.is_file():
        die("gomobile bind 失败")
    log(f"   产出 {AAR_OUT.name}  {AAR_OUT.stat().st_size / 1048576:.1f}MB")

    PLUGIN_ANDROID.mkdir(parents=True, exist_ok=True)
    shutil.copy2(AAR_OUT, AAR_DST)
    log(f"   拷入交付包  {AAR_DST}  sha256={sha16(AAR_DST)}")

    abis = abis_of(AAR_DST)
    log(f"   AAR 内 ABI  : {abis}")
    missing = [a for a in EXPECTED_ABIS if a not in abis]
    if missing:
        die(f"AAR 缺少 ABI {missing} —— 云端打包会在这些架构上崩")


def abis_of(aar: Path) -> list[str]:
    with zipfile.ZipFile(aar) as z:
        return sorted({n.split("/")[1] for n in z.namelist()
                       if n.startswith("jni/") and n.count("/") >= 2})


def extract_classes_jar() -> None:
    step("② 从 lanet.aar 抽出 classes.jar（library 模块不能直接依赖本地 aar）")
    if not AAR_DST.is_file():
        die(f"缺少 {AAR_DST} —— 先跑一次不带 --skip-aar 的构建")
    CLASSES_JAR.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(AAR_DST) as z:
        try:
            data = z.read("classes.jar")
        except KeyError:
            die("lanet.aar 里没有 classes.jar（产物异常）")
    CLASSES_JAR.write_bytes(data)
    with zipfile.ZipFile(CLASSES_JAR) as z:
        n = len([x for x in z.namelist() if x.endswith(".class")])
    log(f"   产出 {CLASSES_JAR.relative_to(REPO_ROOT)}  {len(data) / 1024:.0f}KB / {n} 个类")


def build_plugin(env: dict) -> None:
    step("③ gradle 编插件 module（含 verifyNoLeak 自检）")
    out_aar = MODULE_DIR / "build" / "outputs" / "aar" / "lanet-plugin-release.aar"
    if out_aar.exists():
        out_aar.unlink()
    launcher = find_gradle_launcher()
    cmd = [
        str(Path(env["JAVA_HOME"]) / "bin" / "java"),
        "-classpath", str(launcher),
        "org.gradle.launcher.GradleMain",
        "verifyNoLeak", "--no-daemon", "--console=plain",
    ]
    rc, out = run(cmd, PLUGIN_SRC, env, tail=60)
    if rc != 0:
        die("插件编译失败（上面有 gradle 输出）")
    if not PLUGIN_AAR.is_file():
        die(f"没有产出 {PLUGIN_AAR}")
    log(f"   产出 {PLUGIN_AAR.name}  {PLUGIN_AAR.stat().st_size / 1024:.0f}KB  sha256={sha16(PLUGIN_AAR)}")


def verify_package() -> None:
    step("④ 校验交付包完整性")
    ok = True

    for f in (PLUGIN_PKG_JSON, PLUGIN_ANDROID / "lanet.aar", PLUGIN_AAR):
        if f.is_file():
            log(f"   ✓ {f.relative_to(HERE)}  {f.stat().st_size / 1024:.0f}KB")
        else:
            log(f"   ✗ 缺失 {f.relative_to(HERE)}")
            ok = False
    if not ok:
        die("交付包不完整")

    # package.json 必须是合法 JSON，且 id 与注册 name、类名对得上
    import json
    pkg = json.loads(PLUGIN_PKG_JSON.read_text(encoding="utf-8"))
    if pkg.get("_dp_type") != "nativeplugin":
        die("package.json 的 _dp_type 必须是 nativeplugin")
    andr = pkg["_dp_nativeplugin"]["android"]
    pid = pkg["id"]
    reg = andr["plugins"][0]
    log(f"   插件 id          : {pid}")
    log(f"   注册 name /class : {reg['name']} / {reg['class']}")
    if reg["name"] != pid and not reg["name"].startswith(pid):
        die(f"注册 name（{reg['name']}）必须以插件 id（{pid}）为前缀或相同（DCloud 规范）")
    if andr["integrateType"] != "aar":
        die("integrateType 必须是 aar")
    abis = abis_of(AAR_DST)
    declared = andr.get("abis") or []
    if set(abis) != set(declared):
        die(f"abis 声明 {declared} 与 aar 内实际 {abis} 不一致 —— 云端会在缺 so 的架构上崩")
    # 云端只认这三个
    illegal = [a for a in declared if a not in EXPECTED_ABIS]
    if illegal:
        die(f"abis 含云端不支持的取值 {illegal}（只支持 {EXPECTED_ABIS}）")
    log(f"   abis            : {declared}（与 aar 内一致）")

    # 插件 aar 得带上自己的 AndroidManifest（VpnService / 授权 Activity 靠它合并进宿主）
    minsdk = andr.get("minSdkVersion")
    log(f"   minSdkVersion   : {minsdk!r}")

    # 宿主类泄漏 = 云端 Duplicate class，必须为 0
    leaked: list[str] = []
    with zipfile.ZipFile(PLUGIN_AAR) as outer:
        import io
        with zipfile.ZipFile(io.BytesIO(outer.read("classes.jar"))) as cj:
            for n in cj.namelist():
                if n.startswith(FORBIDDEN_PREFIXES):
                    leaked.append(n)
    if leaked:
        die(f"插件 aar 泄漏宿主类：{leaked[:5]}")
    log("   宿主类泄漏      : 0（无 io.dcloud / weex / com.lanet.mobile）")

    with zipfile.ZipFile(PLUGIN_AAR) as z:
        has_manifest = "AndroidManifest.xml" in z.namelist()
    log(f"   插件 aar 内 manifest : {'有' if has_manifest else '无'}")
    if not has_manifest:
        die("插件 aar 里没有 AndroidManifest.xml —— VpnService 不会被声明，运行必然失败")

    log()
    log("交付包就绪：把下面这个目录整个拷进 uni-app 项目的 nativeplugins/ 目录")
    log(f"   {PLUGIN_PKG}")


def sync_demo() -> None:
    """把插件包同步进 uni-app 示例工程的 nativeplugins/ 目录。

    DCloud 项目的 nativeplugins/ 正常是要入库的，但我们的 aar 合计 39MB —— 所以
    走「构建后自动填充 + gitignore」，clone 下来跑一次 build.py 就能直接云打包。
    """
    step("⑤ 同步到示例工程 nativeplugins/")
    demo = REPO_ROOT / "sdk" / "uniapp-demo"
    if not demo.is_dir():
        log("   （没有 sdk/uniapp-demo，跳过）")
        return
    target = demo / "nativeplugins" / "lanet-vpn"
    if target.exists():
        shutil.rmtree(target)
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copytree(PLUGIN_PKG, target)
    total = sum(f.stat().st_size for f in target.rglob("*") if f.is_file())
    log(f"   {target.relative_to(REPO_ROOT)}  ({total / 1048576:.1f}MB)")
    for f in sorted(target.rglob("*")):
        if f.is_file():
            log(f"      {f.relative_to(target)}  {f.stat().st_size / 1024:.0f}KB")


def main() -> None:
    ap = argparse.ArgumentParser(description="构建 lanet 的 uni-app 原生插件包")
    ap.add_argument("--skip-aar", action="store_true",
                    help="跳过 gomobile 编 AAR（只改了插件 Kotlin/清单时用，省两分钟）")
    ap.add_argument("--verify-only", action="store_true",
                    help="只做交付包校验，不编译")
    args = ap.parse_args()

    env = build_env()
    check_prereqs(env)

    if args.verify_only:
        verify_package()
        return

    if args.skip_aar:
        step("① 跳过 AAR 编译（--skip-aar）")
        if not AAR_DST.is_file():
            die("交付包里还没有 lanet.aar，不能跳过")
        log(f"   沿用现有 {AAR_DST.name}  sha256={sha16(AAR_DST)}")
    else:
        build_aar(env)

    extract_classes_jar()
    build_plugin(env)
    verify_package()
    sync_demo()


if __name__ == "__main__":
    main()
