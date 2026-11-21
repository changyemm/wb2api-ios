#!/usr/bin/env python3
"""把 GOOS=darwin 编出的 c-archive（.a）里每个 .o 成员的
LC_BUILD_VERSION 从 macOS(1) 改成 iOS(2)，minos 改成 15.0。

背景：raccoon 的 patch_platform.py 只处理单个 Mach-O 文件，
对 .a 会报"共检查 0 个 Mach-O 成员"（.a 是 archive 不是 Mach-O）。
Go 编出的 c-archive 每个成员都是独立的 Mach-O relocatable object，
各自带 LC_BUILD_VERSION，必须逐个改再重新打包。

用法：python3 patch_archive_platform.py libwb2api.a [libwb2api-ios.a]
默认原地改写。依赖：系统 ar（GNU ar 或 llvm-ar 均可，只做打包）。
"""
import struct
import subprocess
import sys
import tempfile
from pathlib import Path

LC_BUILD_VERSION = 0x32
PLATFORM_MACOS = 1
PLATFORM_IOS = 2
MINOS_IOS_15 = 0x000F0000  # 15.0.0


def patch_member(path: Path) -> bool:
    data = bytearray(path.read_bytes())
    if len(data) < 32 or struct.unpack("<I", data[:4])[0] != 0xFEEDFACF:
        return False
    (ncmds,) = struct.unpack("<I", data[16:20])
    off = 32
    patched = False
    for _ in range(ncmds):
        cmd, size = struct.unpack("<II", data[off:off + 8])
        if cmd == LC_BUILD_VERSION:
            platform, minos, sdk, _ntools = struct.unpack("<IIII", data[off + 8:off + 24])
            if platform == PLATFORM_MACOS:
                struct.pack_into("<I", data, off + 8, PLATFORM_IOS)
                struct.pack_into("<I", data, off + 12, MINOS_IOS_15)
                patched = True
                print(f"  {path.name}: platform {platform}->2, minos {minos:#x}->{MINOS_IOS_15:#x}")
        off += size
    if patched:
        path.write_bytes(bytes(data))
    return patched


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    src = Path(sys.argv[1])
    dst = Path(sys.argv[2]) if len(sys.argv) > 2 else src
    with tempfile.TemporaryDirectory() as td:
        tmp = Path(td)
        subprocess.run(["ar", "x", str(src.resolve())], cwd=tmp, check=True)
        members = sorted(tmp.glob("*.o"))
        print(f"{src}: {len(members)} 个成员")
        n = 0
        for m in members:
            if patch_member(m):
                n += 1
        print(f"已改 {n} 个成员")
        if n == 0:
            print("警告：一个成员都没改到，检查输入是不是 darwin 编出的 .a")
            return 1
        # 重新打包：GNU ar 用 rc，成员顺序保持不变。
        names = [m.name for m in members]
        subprocess.run(["ar", "rcs", str(dst.resolve())] + names, cwd=tmp, check=True)
    print(f"写出 {dst} ({dst.stat().st_size} 字节)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
