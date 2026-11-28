import hashlib
import json
import os


class Store:
    def __init__(self, root):
        self.root = root
        os.makedirs(os.path.join(root, "blobs"), exist_ok=True)

    def _load(self, name):
        try:
            with open(os.path.join(self.root, name), encoding="utf-8") as f:
                return json.load(f)
        except FileNotFoundError:
            return {}

    def _save(self, name, value):
        with open(os.path.join(self.root, name), "w", encoding="utf-8") as f:
            json.dump(value, f)

    def blob_path(self, digest):
        return os.path.join(self.root, "blobs", digest)

    def upload(self, user, name, data):
        digest = hashlib.sha256(data).hexdigest()
        if not os.path.exists(self.blob_path(digest)):
            with open(self.blob_path(digest), "wb") as f:
                f.write(data)
        files = self._load("files.json")
        files.setdefault(user, {})[name] = digest
        self._save("files.json", files)
        return digest

    def read(self, user, name):
        with open(self.blob_path(self._load("files.json")[user][name]), "rb") as f:
            return f.read()
