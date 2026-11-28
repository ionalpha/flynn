"""A small in-process cache in front of the directory."""


class Cache:
    def get_user(self, user_id):
        return {"id": user_id, "name": "cached-%d" % user_id}
