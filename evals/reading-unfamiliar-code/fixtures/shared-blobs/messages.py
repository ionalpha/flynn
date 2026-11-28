"""Direct messages. An attachment points at the stored file; nothing is copied."""


def send(store, sender, recipient, name):
    inbox = store._load("messages.json")
    digest = store._load("files.json")[sender][name]
    inbox.setdefault(recipient, []).append({"from": sender, "name": name, "digest": digest})
    store._save("messages.json", inbox)


def attachment(store, recipient, index):
    digest = store._load("messages.json")[recipient][index]["digest"]
    with open(store.blob_path(digest), "rb") as f:
        return f.read()
