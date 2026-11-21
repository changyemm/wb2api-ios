#!/usr/bin/env python3
"""把单个 Mach-O dylib 的 LC_BUILD_VERSION 从 macOS(1) 改成 iOS(2)，
minos 改成 15.0。用于 zig cc 以 aarch64-macos 目标链接出的 dylib。

用法：python3 patch_dylib_platform.py wb2api-macos.dylib [wb2api.dylib]
默认原地改写。
"""
import struct
import sys
from pathlib import Path

LC_BUILD_VERSION = 0x32


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    src = Path(sys.argv[1])
    dst = Path(sys.argv[2]) if len(sys.argv) > 2 else src
    data = bytearray(src.read_bytes())
    if len(data) < 32 or struct.unpack("<I", data[:4])[0] != 0xFEEDFACF:
        print("不是 Mach-O 64")
        return 1
    (ncmds,) = struct.unpack("<I", data[16:20])
    off = 32
    found = 0
    for _ in range(ncmds):
        cmd, size = struct.unpack("<II", data[off:off + 8])
        if cmd == LC_BUILD_VERSION:
            platform, minos, sdk, _ = struct.unpack("<IIII", data[off + 8:off + 24])
            print(f"LC_BUILD_VERSION: platform={platform} minos={minos:#x} sdk={sdk:#x}")
            struct.pack_into("<I", data, off + 8, 2)        # iOS
            struct.pack_into("<I", data, off + 12, 0xF0000)  # minos 15.0
            found += 1
        off += size
    if found != 1:
        print(f"LC_BUILD_VERSION 数量异常: {found}")
        return 1
    dst.write_bytes(bytes(data))
    print(f"写出 {dst} ({dst.stat().st_size} 字节)，platform=2 minos=15.0")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
