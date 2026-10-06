"""macOS AX backend. Requires pyobjc and Accessibility permission."""

from accessibility import BackendBase, walk


class Backend(BackendBase):
    def __init__(self):
        import ApplicationServices as AX
        from AppKit import NSWorkspace
        self.ax = AX
        self.workspace = NSWorkspace.sharedWorkspace()
        if not AX.AXIsProcessTrusted():
            raise RuntimeError("grant Accessibility permission to the node/helper in System Settings")

    def attribute(self, control, name, optional=False):
        error, value = self.ax.AXUIElementCopyAttributeValue(control, name, None)
        if error:
            if optional and error in (self.ax.kAXErrorAttributeUnsupported, self.ax.kAXErrorNoValue):
                return None
            raise RuntimeError(f"AX attribute {name} failed: {error}")
        return value

    def entries(self, app):
        roots = []
        for application in self.workspace.runningApplications():
            name = str(application.localizedName() or "")
            if app is None or name == app:
                roots.append((self.ax.AXUIElementCreateApplication(application.processIdentifier()), name))

        def describe(control):
            name = self.attribute(control, "AXTitle", optional=True) or self.attribute(control, "AXDescription", optional=True) or ""
            return {"name": str(name), "role": str(self.attribute(control, "AXRole"))}

        return walk(roots, describe, lambda control: list(self.attribute(control, "AXChildren", optional=True) or []), app)

    def act(self, kind, selector, text):
        control = self.resolve(selector)
        enabled = self.attribute(control, "AXEnabled", optional=True)
        if enabled is not None and not bool(enabled):
            raise RuntimeError("target is disabled")
        if kind == "click":
            error = self.ax.AXUIElementPerformAction(control, "AXPress")
        elif kind == "focus":
            error = self.ax.AXUIElementSetAttributeValue(control, "AXFocused", True)
        else:
            error = self.ax.AXUIElementSetAttributeValue(control, "AXValue", text)
        if error:
            raise RuntimeError(f"AX {kind} failed: {error}; no coordinate fallback was attempted")
