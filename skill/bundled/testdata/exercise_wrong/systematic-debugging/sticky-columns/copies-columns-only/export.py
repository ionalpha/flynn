"""Render a tenant's rows as delimited text."""

BASE_COLUMNS = ["id", "date", "total", "note"]
DEFAULTS = {"delimiter": ",", "hidden": []}


def settings_for(tenant):
    settings = dict(DEFAULTS)
    if "delimiter" in tenant:
        settings["delimiter"] = tenant["delimiter"]
    settings["hidden"] += tenant.get("hidden", [])
    return settings


def columns_for(tenant, settings):
    columns = list(BASE_COLUMNS)
    columns += tenant.get("extra", [])
    return [c for c in columns if c not in settings["hidden"]]


def export(tenant, rows):
    settings = settings_for(tenant)
    columns = columns_for(tenant, settings)
    lines = [settings["delimiter"].join(columns)]
    for row in rows:
        lines.append(settings["delimiter"].join(str(row.get(c, "")) for c in columns))
    return "\n".join(lines)
