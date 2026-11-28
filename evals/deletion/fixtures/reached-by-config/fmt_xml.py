import helpers


def render(rows):
    body = "".join("<row>%s</row>" % "".join("<c>%s</c>" % helpers.xml_escape(c) for c in row) for row in rows)
    return "<rows>" + body + "</rows>"
