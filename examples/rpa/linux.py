"""Linux AT-SPI backend. Requires distro python3-pyatspi and session D-Bus."""

from accessibility import BackendBase, walk


class Backend(BackendBase):
    def __init__(self):
        import pyatspi
        self.atspi = pyatspi

    def entries(self, app):
        desktop = self.atspi.Registry.getDesktop(0)
        roots = [(desktop.getChildAtIndex(i), desktop.getChildAtIndex(i).name) for i in range(min(desktop.childCount, 200))]

        def describe(control):
            return {"name": control.name or "", "role": control.getRoleName()}

        def children(control):
            # Return one extra child to signal truncation without an unbounded list.
            return [control.getChildAtIndex(i) for i in range(min(control.childCount, 201))]

        entries, truncated = walk(roots, describe, children, app)
        return entries, truncated or desktop.childCount > 200

    def act(self, kind, selector, text):
        control = self.resolve(selector)
        if not control.getState().contains(self.atspi.STATE_ENABLED):
            raise RuntimeError("target is disabled")
        if kind == "focus":
            success = control.queryComponent().grabFocus()
        elif kind == "setValue":
            success = control.queryEditableText().setTextContents(text)
        else:
            actions = control.queryAction()
            candidates = [i for i in range(actions.nActions) if actions.getName(i).lower() in {"click", "press", "activate"}]
            if len(candidates) != 1:
                raise RuntimeError("target does not expose one unambiguous activation action")
            success = actions.doAction(candidates[0])
        if not success:
            raise RuntimeError(f"AT-SPI {kind} failed")
