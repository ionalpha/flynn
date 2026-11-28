def create_account(store, user):
    users = store._load("users.json")
    users[user] = {}
    store._save("users.json", users)


def delete_account(store, user):
    users = store._load("users.json")
    del users[user]
    store._save("users.json", users)
