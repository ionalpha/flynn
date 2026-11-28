import os


def create_account(store, user):
    users = store._load("users.json")
    users[user] = {}
    store._save("users.json", users)


def delete_account(store, user):
    users = store._load("users.json")
    del users[user]
    store._save("users.json", users)
    files = store._load("files.json")
    mine = set(files.pop(user, {}).values())
    store._save("files.json", files)
    kept = {d for names in files.values() for d in names.values()}
    for digest in mine - kept:
        if os.path.exists(store.blob_path(digest)):
            os.remove(store.blob_path(digest))
