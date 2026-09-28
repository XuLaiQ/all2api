"""CI script to verify no legacy code in production."""
import sys
import re
from pathlib import Path


def check_legacy_imports():
    """Check for imports from legacy bridge."""
    app_dir = Path(__file__).parent.parent / "app"
    pattern = re.compile(r"from\s+app\.compat\.legacy_bridge")

    violations = []
    for py_file in app_dir.rglob("*.py"):
        try:
            content = py_file.read_text(encoding="utf-8")
            for line_num, line in enumerate(content.splitlines(), 1):
                if pattern.search(line):
                    violations.append(f"{py_file.relative_to(app_dir.parent)}:{line_num}: {line.strip()}")
        except Exception as e:
            print(f"Warning: Could not read {py_file}: {e}")

    if violations:
        print("FAIL: Found legacy bridge imports:")
        for v in violations:
            print(f"  {v}")
        return False
    return True


def check_source_project_refs():
    """Check for source project references."""
    app_dir = Path(__file__).parent.parent / "app"
    patterns = [
        (re.compile(r"F:\\token-p|F:/token-p"), "source project path"),
        (re.compile(r"(from|import)\s+(wb2api|doubao2api|chatgpt2api)"), "source project imports"),
        (re.compile(r"localhost:808[0-9]"), "source project ports (8080-8089)"),
    ]

    all_clean = True
    for pattern, desc in patterns:
        violations = []
        for py_file in app_dir.rglob("*.py"):
            try:
                content = py_file.read_text(encoding="utf-8")
                for line_num, line in enumerate(content.splitlines(), 1):
                    if pattern.search(line):
                        violations.append(f"{py_file.relative_to(app_dir.parent)}:{line_num}: {line.strip()}")
            except Exception as e:
                print(f"Warning: Could not read {py_file}: {e}")

        if violations:
            print(f"FAIL: Found {desc}:")
            for v in violations:
                print(f"  {v}")
            all_clean = False

    return all_clean


def main():
    """Run all checks."""
    print("Checking for legacy code violations...")

    checks = [
        ("Legacy imports", check_legacy_imports),
        ("Source project references", check_source_project_refs),
    ]

    all_passed = True
    for name, check_fn in checks:
        print(f"\nRunning: {name}")
        if check_fn():
            print(f"PASS: {name}")
        else:
            print(f"FAIL: {name}")
            all_passed = False

    if all_passed:
        print("\nAll legacy code checks passed!")
        sys.exit(0)
    else:
        print("\nSome checks failed. Please fix before merging.")
        sys.exit(1)


if __name__ == "__main__":
    main()
