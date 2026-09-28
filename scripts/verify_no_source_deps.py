#!/usr/bin/env python3
"""验证项目已完全脱离源项目依赖的门禁脚本。

此脚本检查：
1. 没有源项目的导入语句
2. 没有源项目的路径引用
3. legacy 配置为可选（有默认值）
4. 在没有源项目时构建成功
5. Docker 镜像构建成功

用法：
    python scripts/verify_no_source_deps.py

退出码：
    0 - 所有检查通过
    1 - 至少一项检查失败
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

# 修复 Windows 控制台编码问题
if sys.platform == "win32":
    import codecs
    sys.stdout = codecs.getwriter("utf-8")(sys.stdout.detach())
    sys.stderr = codecs.getwriter("utf-8")(sys.stderr.detach())


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
        if var in content:
            # 检查是否有 Field(default= 或直接的默认值
            if f"{var}:" in content:
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
    """在源项目不可用时验证构建"""
    print("=== Testing clean build without source projects ===")

    # 1. 检查 Python 依赖安装
    print("Checking backend dependencies...")
    result = subprocess.run(
        ["uv", "sync", "--extra", "dev"],
        cwd="services/api",
        capture_output=True,
        text=True
    )
    if result.returncode != 0:
        print(f"❌ ERROR: Backend install failed: {result.stderr}")
        return False

    # 2. 运行 Python 编译检查
    print("Running Python compilation check...")
    result = subprocess.run(
        ["python", "-m", "compileall", "app", "-q"],
        cwd="services/api",
        capture_output=True,
        text=True
    )
    if result.returncode != 0:
        print(f"❌ ERROR: Python compilation failed: {result.stderr}")
        return False

    # 3. 检查前端依赖（如果存在且 pnpm 可用）
    web_dir = Path("web")
    if web_dir.exists() and (web_dir / "package.json").exists():
        # 检查 pnpm 是否可用
        try:
            result = subprocess.run(
                ["pnpm", "--version"],
                capture_output=True,
                text=True,
                timeout=5
            )
            if result.returncode == 0:
                print("Checking frontend dependencies...")
                result = subprocess.run(
                    ["pnpm", "install"],
                    cwd="web",
                    capture_output=True,
                    text=True
                )
                if result.returncode != 0:
                    print(f"❌ ERROR: Frontend install failed: {result.stderr}")
                    return False
            else:
                print("⚠️  SKIP: pnpm not available, skipping frontend check")
        except (subprocess.TimeoutExpired, FileNotFoundError):
            print("⚠️  SKIP: pnpm not available, skipping frontend check")

    print("✅ PASS: Clean build successful")
    return True


def check_docker_build():
    """验证 Docker 镜像构建"""
    print("=== Testing Docker build ===")

    dockerfile = Path("services/api/Dockerfile")
    if not dockerfile.exists():
        print("⚠️  SKIP: Dockerfile not found")
        return True

    # 检查 Docker 是否可用并且 daemon 是否运行
    try:
        result = subprocess.run(
            ["docker", "info"],
            capture_output=True,
            text=True,
            timeout=5
        )
        if result.returncode != 0:
            print("⚠️  SKIP: Docker daemon not running")
            return True
    except (subprocess.TimeoutExpired, FileNotFoundError):
        print("⚠️  SKIP: Docker not available")
        return True

    print("Building Docker image...")
    result = subprocess.run(
        [
            "docker", "build",
            "-t", "all2api-verify",
            "-f", "services/api/Dockerfile",
            "."
        ],
        capture_output=True,
        text=True
    )

    if result.returncode != 0:
        print(f"❌ ERROR: Docker build failed: {result.stderr}")
        return False

    # 检查镜像内容
    print("Verifying Docker image...")
    result = subprocess.run(
        [
            "docker", "run", "--rm", "all2api-verify",
            "python", "-c", "import app; print('OK')"
        ],
        capture_output=True,
        text=True
    )

    if "OK" not in result.stdout:
        print(f"❌ ERROR: Docker image cannot import app: {result.stderr}")
        return False

    print("✅ PASS: Docker build successful")
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

    results = []
    for name, check_fn in checks:
        try:
            passed = check_fn()
            results.append((name, passed))
        except Exception as exc:
            print(f"❌ ERROR in {name}: {exc}")
            results.append((name, False))
        print()

    # 汇总结果
    print("="*60)
    print("检查结果汇总")
    print("="*60)

    all_passed = True
    for name, passed in results:
        status = "✅ PASS" if passed else "❌ FAIL"
        print(f"{status}: {name}")
        if not passed:
            all_passed = False

    print("="*60)

    if all_passed:
        print("\n🎉 所有检查通过！项目已完全脱离源项目依赖。\n")
        return 0
    else:
        print("\n⚠️  部分检查未通过，请修复后重试。\n")
        return 1


if __name__ == "__main__":
    sys.exit(main())
