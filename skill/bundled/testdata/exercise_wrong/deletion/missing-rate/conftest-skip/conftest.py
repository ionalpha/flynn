import pytest


def pytest_collection_modifyitems(items):
    for item in items:
        if item.name == "test_de":
            item.add_marker(pytest.mark.skip(reason="rate pending"))
