"""Windows UI Automation backend. Requires pywinauto and an interactive token."""

from accessibility import BackendBase, walk


class Backend(BackendBase):
    def __init__(self):
        from pywinauto import Desktop
        self.desktop = Desktop(backend="uia")

    def entries(self, app):
        roots = [(window, window.window_text()) for window in self.desktop.windows()]

        def describe(control):
            info = control.element_info
            rect = control.rectangle()
            return {"name": info.name or "", "role": info.control_type,
                    "automationId": info.automation_id or "",
                    "bounds": {"x": rect.left, "y": rect.top, "width": rect.width(), "height": rect.height()}}

        return walk(roots, describe, lambda control: control.children(), app)

    def act(self, kind, selector, text):
        control = self.resolve(selector)
        if not control.is_enabled():
            raise RuntimeError("target is disabled")
        if kind == "focus":
            control.set_focus()
        elif kind == "click":
            # InvokePattern acts on the control, not an inferred screen point.
            control.iface_invoke.Invoke()
        elif kind == "setValue":
            control.iface_value.SetValue(text)
