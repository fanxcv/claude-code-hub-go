#!/usr/bin/env python3
"""断言发布二进制的文件格式与目标架构正确，并拒绝带动态依赖的产物。

为什么不用 `file(1)` 当判据：本仓发布矩阵含 linux/darwin/windows，而 `file` 的输出措辞
随版本变过（Mach-O 既有 "Mach-O 64-bit executable x86_64" 也有 "Mach-O 64-bit x86_64
executable"），拿它做断言会随 runner 镜像升级而假红。所以这里直接读头部魔数与机器字段，
判据与任何外部工具版本无关。

ELF 的动态依赖另由工作流里的 `readelf -d` 断言（Linux runner 上有 binutils）；Mach-O 与
PE 没有等价工具，改由「CGO_ENABLED=0」保证自足——本脚本只断言格式与架构。

用法：check-binary-format.py <binary> <linux|darwin|windows> <amd64|arm64>
退出码：0 通过；非 0 失败（并向 stdout 打 GitHub Actions 的 ::error:: 注解）。
"""

import struct
import sys

ELF_MACHINE = {"amd64": 62, "arm64": 183}
MACHO_CPU = {"amd64": 0x01000007, "arm64": 0x0100000C}
PE_MACHINE = {"amd64": 0x8664, "arm64": 0xAA64}

ELFCLASS64 = 2
MH_MAGIC_64 = b"\xcf\xfa\xed\xfe"
MH_EXECUTE = 2


def fail(message: str) -> None:
    print(f"::error::{message}")
    sys.exit(1)


def check_elf(data: bytes, arch: str, path: str) -> str:
    if data[:4] != b"\x7fELF":
        fail(f"{path} 不是 ELF 文件")
    if data[4] != ELFCLASS64:
        fail(f"{path} 不是 64 位 ELF（EI_CLASS={data[4]}）")
    machine = struct.unpack_from("<H", data, 18)[0]
    if machine != ELF_MACHINE[arch]:
        fail(f"{path} ELF machine={machine:#x}，期望 {ELF_MACHINE[arch]:#x}（{arch}）")
    return f"ELF64 machine={machine:#x}"


def check_macho(data: bytes, arch: str, path: str) -> str:
    if data[:4] != MH_MAGIC_64:
        fail(f"{path} 不是 64 位小端 Mach-O（魔数 {data[:4].hex()}）")
    cputype = struct.unpack_from("<I", data, 4)[0]
    filetype = struct.unpack_from("<I", data, 12)[0]
    if filetype != MH_EXECUTE:
        fail(f"{path} Mach-O filetype={filetype}，期望 {MH_EXECUTE}（可执行文件）")
    if cputype != MACHO_CPU[arch]:
        fail(f"{path} Mach-O cputype={cputype:#x}，期望 {MACHO_CPU[arch]:#x}（{arch}）")
    return f"Mach-O64 cputype={cputype:#x}"


def check_pe(data: bytes, arch: str, path: str) -> str:
    if data[:2] != b"MZ":
        fail(f"{path} 不是 PE 文件（缺 MZ 头）")
    lfanew = struct.unpack_from("<I", data, 0x3C)[0]
    if data[lfanew : lfanew + 4] != b"PE\0\0":
        fail(f"{path} 缺 PE\\0\\0 签名（e_lfanew={lfanew:#x}）")
    machine = struct.unpack_from("<H", data, lfanew + 4)[0]
    if machine != PE_MACHINE[arch]:
        fail(f"{path} PE machine={machine:#x}，期望 {PE_MACHINE[arch]:#x}（{arch}）")
    return f"PE32+ machine={machine:#x}"


def main() -> None:
    if len(sys.argv) != 4:
        fail("用法: check-binary-format.py <binary> <linux|darwin|windows> <amd64|arm64>")
    path, os_name, arch = sys.argv[1], sys.argv[2], sys.argv[3]
    if os_name not in {"linux", "darwin", "windows"}:
        fail(f"未知平台 {os_name}")
    if arch not in {"amd64", "arm64"}:
        fail(f"未知架构 {arch}")

    # 头部全在开头 4 KiB 内（PE 的 e_lfanew 典型 0x80~0x100）。
    with open(path, "rb") as fh:
        data = fh.read(4096)
    if len(data) < 64:
        fail(f"{path} 过小（{len(data)} B），不是可执行文件")

    check = {"linux": check_elf, "darwin": check_macho, "windows": check_pe}[os_name]
    detail = check(data, arch, path)
    print(f"{path}: {os_name}/{arch} 形态检查通过（{detail}）")


if __name__ == "__main__":
    main()
