package clipboard

import "testing"

// Pasted file names must be portable to every supported OS. Windows ignores
// spaces before an extension, so "con .txt" opens the CON device there.
func TestValidNameRejectsWindowsDeviceNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"CON", "con.txt", "con .txt", "Aux  .log", "NUL", "com1", "COM1 .x", "lpt¹", "LPT³ .txt", "CONIN$", "conout$ .x"} {
		if validName(name) {
			t.Errorf("accepted Windows device name %q", name)
		}
	}
	for _, name := range []string{"console.txt", "com10", "COM0", "lpt", "a con.txt", "con_x.txt", "report.pdf"} {
		if !validName(name) {
			t.Errorf("rejected ordinary name %q", name)
		}
	}
}
