import api


def team_invite(team_id, email):
    if email in api.org_members(team_id):
        return "already a member"
    return "Invitation sent to %s" % email


def team_roster(team_id):
    return ", ".join(api.org_members(team_id))
