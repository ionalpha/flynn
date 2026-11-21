import ast
import unittest

MODULES = ["purchasing", "stock", "orders", "warehouse"]


def top_level_imports(name):
    tree = ast.parse(open(name + ".py", encoding="utf-8").read())
    found = set()
    for node in tree.body:
        if isinstance(node, ast.Import):
            found.update(a.name.split(".")[0] for a in node.names)
        elif isinstance(node, ast.ImportFrom) and node.module:
            found.add(node.module.split(".")[0])
    return found & set(MODULES)


class TestArchitecture(unittest.TestCase):
    def test_imports_have_no_cycle(self):
        graph = {m: top_level_imports(m) for m in MODULES}
        state = {}

        def visit(m, path):
            if state.get(m) == "done":
                return
            self.assertNotIn(m, path, "import cycle: " + " -> ".join(path + [m]))
            for dep in graph[m]:
                visit(dep, path + [m])
            state[m] = "done"

        for m in MODULES:
            visit(m, [])


if __name__ == "__main__":
    unittest.main()
