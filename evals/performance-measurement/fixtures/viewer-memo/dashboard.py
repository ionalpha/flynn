"""The project dashboard."""


def summarise(tasks):
    counts = {}
    for task in tasks:
        counts[task["status"]] = counts.get(task["status"], 0) + 1
    return {"counts": counts, "titles": sorted(task["title"] for task in tasks)}


def render(viewer, project_id, db):
    """The summary of one project as viewer may see it.

    viewer is {"id": ..., "admin": bool}. Private tasks are visible to their owner
    and to admins.
    """
    tasks = [
        task for task in db.tasks(project_id)
        if not task["private"] or viewer["admin"] or task["owner"] == viewer["id"]
    ]
    return summarise(tasks)
