import json
import os

HERE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


class CodeRepository:
    def find(self, code):
        with open(os.path.join(HERE, "codes.json"), encoding="utf-8") as f:
            return json.load(f).get(code)


class DiscountService:
    def __init__(self, repository=None):
        self.repository = repository or CodeRepository()

    def apply(self, total, code):
        if code is None:
            return total
        percent = self.repository.find(code)
        if percent is None:
            raise ValueError(f"unknown discount code {code!r}")
        return total * (100 - percent) // 100
