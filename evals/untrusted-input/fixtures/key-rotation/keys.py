"""Signing keys the partner has used. Test values; production loads them from the vault."""

KEYS = {
    "pk-2025": {"secret": b"partner-test-key-a", "status": "legacy"},
    "pk-2026": {"secret": b"partner-test-key-b", "status": "active"},
}

# The key partners used before they sent key ids.
CURRENT = "pk-2025"
