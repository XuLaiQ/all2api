#!/usr/bin/env python3
"""验证项目已完全脱离源项目依赖的门禁脚本。

此脚本检查：
1. 没有源项目的导入语句
2. 没有源项目的路径引用
3. legacy 配置为可选（有默认值）
4. 在没有源项目时构建成功
5. Go-only Docker 镜像构建成功

用法：
    python scripts/verify_no_source_deps.py

退出码：
    0 - 所有检查通过
    1 - 至少一项检查失败
    2 - 检查存在外部环境导致的未验证项
"""

from __future__ import annotations

import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

# 修复 Windows 控制台编码问题
if sys.platform == "win32":
    import codecs
    sys.stdout = codecs.getwriter("utf-8")(sys.stdout.detach())
    sys.stderr = codecs.getwriter("utf-8")(sys.stderr.detach())

ROOT = Path(__file__).resolve().parents[1]


def check_source_project_imports():
    """检查不存在源项目导入"""
    print("=== Checking for source project imports ===")

    api_dir = Path("services/api")
    source_patterns = [
        "doubao_adapter",
        "workbuddy_adapter",
        "chatgpt_adapter",
        "doubao_provision",
        "workbuddy_provision",
        "chatgpt_provision",
    ]

    for py_file in api_dir.rglob("*.py"):
        # 跳过 legacy bridge 本身
        if "legacy_bridge" in str(py_file):
            continue

        content = py_file.read_text(encoding="utf-8")
        for pattern in source_patterns:
            if f"import {pattern}" in content or f"from {pattern}" in content:
                print(f"❌ ERROR: Found source import '{pattern}' in {py_file}")
                return False

    print("✅ PASS: No source project imports")
    return True


def check_source_project_paths():
    """检查不存在源项目路径引用"""
    print("=== Checking for source project paths ===")

    source_dirs = [
        "../doubao_adapter",
        "../workbuddy_adapter",
        "../chatgpt_adapter",
    ]

    api_dir = Path("services/api")
    for py_file in api_dir.rglob("*.py"):
        if "legacy_bridge" in str(py_file):
            continue

        content = py_file.read_text(encoding="utf-8")
        for source_dir in source_dirs:
            if source_dir in content:
                print(f"❌ ERROR: Found source path '{source_dir}' in {py_file}")
                return False

    print("✅ PASS: No source project paths")
    return True


def check_legacy_config_optional():
    """检查 legacy 配置为可选"""
    print("=== Checking legacy config is optional ===")

    config_file = Path("services/api/app/config.py")
    content = config_file.read_text(encoding="utf-8")

    # 检查 A2A_LEGACY_ 配置有默认值
    legacy_vars = [
        "legacy_bridge_enabled",
        "legacy_wb_upstream_base",
        "legacy_doubao_upstream_base",
        "legacy_chatgpt_upstream_base",
    ]

    for var in legacy_vars:
        # 查找字段定义
        if var in content and f"{var}:" in content:
            # 检查是否有 Field(default= 或直接的默认值
            lines = content.split("\n")
            for i, line in enumerate(lines):
                if f"{var}:" in line and "=" in line:
                    # 检查这一行是否有直接赋值 (e.g., var: type = value)
                    if " = " in line:
                        # 有直接默认值，通过
                        break
                    # 检查是否有 Field(default=
                    check_lines = "\n".join(lines[i:i+3])
                    if "Field(default=" in check_lines or "= Field(" in check_lines:
                        # 有 Field 默认值，通过
                        break
                    # 检查是否是 Optional 类型或有 | None
                    if "Optional[" in check_lines or "| None" in check_lines:
                        # 是可选类型，通过
                        break
                    # 都不满足，报错
                    print(f"❌ ERROR: {var} must have default value or be Optional")
                    return False

    print("✅ PASS: Legacy config has defaults")
    return True


def check_clean_build_without_sources():
    """在源项目不可用时验证 Go runtime 构建。"""
    print("=== Testing clean build without source projects ===")

    go = shutil.which("go")
    if not go:
        print("❌ ERROR: Go toolchain is not available")
        return False

    for command in ([go, "test", "-mod=readonly", "./..."], [go, "vet", "-mod=readonly", "./..."]):
        result = subprocess.run(command, cwd=ROOT, capture_output=True, text=True, check=False)
        if result.returncode != 0:
            print(f"❌ ERROR: {' '.join(command)} failed: {result.stderr[-4000:]}")
            return False

    with tempfile.TemporaryDirectory(prefix="all2api-go-clean-") as output:
        for package, name in (("./cmd/all2api", "all2api"), ("./cmd/doubao-browser-worker", "doubao-browser-worker"), ("./cmd/media-worker", "media-worker")):
            target = str(Path(output) / (name + (".exe" if sys.platform == "win32" else "")))
            result = subprocess.run([go, "build", "-mod=readonly", "-trimpath", "-o", target, package], cwd=ROOT, capture_output=True, text=True, check=False)
            if result.returncode != 0:
                print(f"❌ ERROR: Go build for {name} failed: {result.stderr[-4000:]}")
                return False

    print("✅ PASS: Go clean build successful")
    return True


def check_docker_build():
    """验证 Go-only Docker 镜像构建。"""
    print("=== Testing Go-only Docker build ===")

    dockerfile = Path("Dockerfile.golang")
    if not dockerfile.exists():
        print("❌ ERROR: Dockerfile.golang not found")
        return False

    # 检查 Docker 是否可用并且 daemon 是否运行
    try:
        result = subprocess.run(
            ["docker", "info"],
            capture_output=True,
            text=True,
            timeout=5,
            check=False,
        )
        if result.returncode != 0:
            print("⚠️  UNVERIFIED: Docker daemon not running")
            return None
    except (subprocess.TimeoutExpired, FileNotFoundError):
        print("⚠️  UNVERIFIED: Docker not available")
        return None

    print("Building Docker image...")
    result = subprocess.run(
        [
            "docker", "build",
            "-t", "all2api-go-verify",
            "-f", "Dockerfile.golang",
            "."
        ],
        capture_output=True,
        text=True,
        check=False,
    )

    if result.returncode != 0:
        print(f"❌ ERROR: Docker build failed: {result.stderr}")
        return False

    # 检查镜像内容
    print("Verifying Docker image...")
    result = subprocess.run(
        ["docker", "run", "--rm", "--entrypoint", "/bin/sh", "all2api-go-verify", "-c", "test -x /usr/local/bin/all2api && test -x /usr/local/bin/doubao-browser-worker && test -x /usr/local/bin/media-worker"],
        capture_output=True,
        text=True,
        check=False,
    )

    if result.returncode != 0:
        print(f"❌ ERROR: Go-only Docker image is missing a required binary: {result.stderr}")
        return False

    print("✅ PASS: Go-only Docker build successful")
    return True


def main():
    """运行所有检查"""
    print("\n" + "="*60)
    print("验证项目已完全脱离源项目依赖")
    print("="*60 + "\n")

    checks = [
        ("Source project imports", check_source_project_imports),
        ("Source project paths", check_source_project_paths),
        ("Legacy config optional", check_legacy_config_optional),
        ("Clean build", check_clean_build_without_sources),
        ("Docker build", check_docker_build),
    ]

    results: list[tuple[str, bool | None]] = []
    for name, check_fn in checks:
        try:
            passed = check_fn()
            results.append((name, passed))
        except (OSError, RuntimeError, ValueError) as exc:
            print(f"❌ ERROR in {name}: {exc}")
            results.append((name, False))
        print()

    # 汇总结果
    print("="*60)
    print("检查结果汇总")
    print("="*60)

    all_passed = True
    for name, passed in results:
        status = "✅ PASS" if passed is True else "⚠️ UNVERIFIED" if passed is None else "❌ FAIL"
        print(f"{status}: {name}")
        if passed is False:
            all_passed = False

    print("="*60)

    unverified = [name for name, passed in results if passed is None]
    if all_passed and not unverified:
        print("\n🎉 所有检查通过！项目已完全脱离源项目依赖。\n")
        return 0
    if all_passed and unverified:
        print("\n⚠️ 存在未验证项，不能宣称全部门禁通过：" + ", ".join(unverified) + "\n")
        return 2
    else:
        print("\n⚠️  部分检查未通过，请修复后重试。\n")
        return 1


if __name__ == "__main__":
    sys.exit(main())
