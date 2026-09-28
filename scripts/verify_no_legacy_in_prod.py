#!/usr/bin/env python3
"""
验证生产镜像不包含 legacy 代码

此脚本用于 CI/CD 流程，确保生产 Docker 镜像中不包含 legacy_bridge 目录。
"""

import subprocess
import sys
import shutil
import os
from pathlib import Path

# 在 Windows 上设置 UTF-8 输出
if sys.platform == "win32":
    sys.stdout.reconfigure(encoding='utf-8')
    sys.stderr.reconfigure(encoding='utf-8')


def check_docker_available() -> bool:
    """检查 Docker 是否可用"""
    if not shutil.which("docker"):
        print("⚠️  SKIP: Docker 命令不可用")
        return False

    try:
        result = subprocess.run(
            ["docker", "info"],
            capture_output=True,
            text=True,
            timeout=5
        )
        if result.returncode != 0:
            print("⚠️  SKIP: Docker daemon 未运行")
            return False
        return True
    except (subprocess.TimeoutExpired, FileNotFoundError):
        print("⚠️  SKIP: Docker daemon 不可访问")
        return False


def build_test_image() -> str:
    """构建测试镜像"""
    image_tag = "all2api-legacy-check:test"

    print(f"Building test image: {image_tag}")
    try:
        result = subprocess.run(
            ["docker", "build", "-t", image_tag, "-f", "services/api/Dockerfile", "."],
            capture_output=True,
            text=True,
            timeout=300
        )

        if result.returncode != 0:
            print(f"❌ Docker build failed:\n{result.stderr}")
            return ""

        print(f"✅ Image built successfully: {image_tag}")
        return image_tag

    except subprocess.TimeoutExpired:
        print("❌ Docker build timed out")
        return ""
    except Exception as e:
        print(f"❌ Docker build error: {e}")
        return ""


def check_legacy_in_image(image_tag: str) -> bool:
    """检查镜像中是否包含 legacy_bridge 目录"""
    print(f"\nChecking for legacy_bridge in {image_tag}...")

    try:
        # 检查 compat 目录内容
        result = subprocess.run(
            ["docker", "run", "--rm", image_tag, "ls", "-la", "/app/app/compat/"],
            capture_output=True,
            text=True,
            timeout=10
        )

        if "legacy_bridge" in result.stdout:
            print(f"❌ FAIL: legacy_bridge directory found in production image")
            print(f"\nDirectory contents:\n{result.stdout}")
            return False

        print("✅ PASS: legacy_bridge directory not found in production image")
        return True

    except subprocess.TimeoutExpired:
        print("❌ Container check timed out")
        return False
    except Exception as e:
        print(f"❌ Container check error: {e}")
        return False


def cleanup_image(image_tag: str) -> None:
    """清理测试镜像"""
    if not image_tag:
        return

    print(f"\nCleaning up test image: {image_tag}")
    try:
        subprocess.run(
            ["docker", "rmi", image_tag],
            capture_output=True,
            timeout=30
        )
        print("✅ Test image removed")
    except Exception as e:
        print(f"⚠️  Failed to remove test image: {e}")


def main() -> int:
    print("=" * 60)
    print("验证生产镜像不包含 Legacy 代码")
    print("=" * 60)
    print()

    # 检查 Docker 是否可用
    if not check_docker_available():
        print("\n" + "=" * 60)
        print("⚠️  Docker 不可用，跳过生产镜像检查")
        print("=" * 60)
        return 0  # 跳过，不算失败

    # 构建测试镜像
    image_tag = build_test_image()
    if not image_tag:
        return 1

    # 检查 legacy 代码
    try:
        passed = check_legacy_in_image(image_tag)
    finally:
        cleanup_image(image_tag)

    print("\n" + "=" * 60)
    if passed:
        print("🎉 验证通过：生产镜像不包含 legacy 代码")
    else:
        print("❌ 验证失败：生产镜像仍包含 legacy 代码")
    print("=" * 60)

    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
