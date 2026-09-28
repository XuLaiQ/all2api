"""
End-to-End (E2E) Tests for All2API

This package contains E2E tests that verify real platform integrations.
These tests use actual credentials and make real API calls.

Test Markers:
- @pytest.mark.e2e: All E2E tests should be marked with this
- @pytest.mark.browser: Tests that require Playwright Chromium
- @pytest.mark.slow: Long-running tests (e.g., 24h stability tests)

Environment Variables:
- E2E_ENABLED: Set to "true" to enable E2E tests
- E2E_CHATGPT_ENABLED: Enable ChatGPT E2E tests
- E2E_WORKBUDDY_ENABLED: Enable WorkBuddy E2E tests
- E2E_DOUBAO_ENABLED: Enable Doubao E2E tests

Running E2E Tests:
```bash
# Run all E2E tests
E2E_ENABLED=true uv run pytest tests/e2e/ -v

# Run only ChatGPT E2E tests
E2E_CHATGPT_ENABLED=true uv run pytest tests/e2e/test_chatgpt_e2e.py -v

# Skip slow tests
E2E_ENABLED=true uv run pytest tests/e2e/ -v -m "not slow"
```

Security Note:
Real credentials should NEVER be committed to Git. Use environment variables
or secure secret management systems.
"""
