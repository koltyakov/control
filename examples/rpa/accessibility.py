"""Bounded accessibility traversal and exact, unique selector resolution."""


class BackendBase:
    def entries(self, app):
        raise NotImplementedError

    def scan(self, app=None):
        entries, truncated = self.entries(app)
        return entries, truncated

    def inspect(self, app, limit):
        entries, truncated = self.scan(app)
        return {"elements": [item for item, _ in entries[:limit]],
                "truncated": truncated or len(entries) > limit}

    def resolve(self, selector):
        entries, truncated = self.scan(selector.get("app"))
        # Incomplete enumeration cannot prove uniqueness, even with one match.
        if truncated:
            raise RuntimeError("accessibility scan incomplete; narrow the app selector")
        matches = [handle for item, handle in entries if all(item.get(k) == v for k, v in selector.items())]
        if len(matches) != 1:
            raise RuntimeError(f"selector matched {len(matches)} elements; expected exactly one")
        return matches[0]


def walk(roots, describe, children, app, maximum=2000):
    """Depth-first traversal with bounded nodes, child lists and depth."""
    stack = [(root, application, 0) for root, application in reversed(roots) if app is None or application == app]
    entries = []
    incomplete = False
    visited = 0
    while stack and visited < maximum:
        handle, application, depth = stack.pop()
        visited += 1
        if app is not None and application != app:
            continue
        try:
            item = describe(handle)
            item["app"] = application
            entries.append((item, handle))
            descendants = children(handle)
            if len(descendants) > 200 or (descendants and depth >= 32):
                incomplete = True
            if depth < 32:
                stack.extend((child, application, depth + 1) for child in reversed(descendants[:200]))
        except Exception:
            # A disappearing or inaccessible control makes uniqueness uncertain.
            incomplete = True
    return entries, incomplete or bool(stack)
