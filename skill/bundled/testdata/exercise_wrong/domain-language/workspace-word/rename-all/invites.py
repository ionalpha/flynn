import api


def workspace_invite(workspace_id, email):
    if email in api.workspace_members(workspace_id):
        return "already a member"
    return "Invitation sent to %s" % email


def workspace_roster(workspace_id):
    return ", ".join(api.workspace_members(workspace_id))
