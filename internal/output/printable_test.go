package output

import "testing"

func TestPrintableEscapesWhatATerminalWouldActOn(t *testing.T) {
	cases := []struct{ in, want, wantMessage string }{
		{"/var/log/app.log", "/var/log/app.log", "/var/log/app.log"},
		{"/tmp/\x1b[31mred.log", `/tmp/\x1b[31mred.log`, `/tmp/\x1b[31mred.log`},
		{"a\rb", `a\x0db`, `a\x0db`},
		{"line\nnext\ttab", `line\x0anext\x09tab`, "line\nnext\ttab"},
		{"del\x7f", `del\x7f`, `del\x7f`},
		{"c1 \u009b2J", `c1 \u009b2J`, `c1 \u009b2J`},
		{"raw \x9b2J", `raw \x9b2J`, `raw \x9b2J`},
		{"журнал ошибок.log", "журнал ошибок.log", "журнал ошибок.log"},
	}
	for _, tc := range cases {
		if got := Printable(tc.in); got != tc.want {
			t.Errorf("Printable(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := PrintableMessage(tc.in); got != tc.wantMessage {
			t.Errorf("PrintableMessage(%q) = %q, want %q", tc.in, got, tc.wantMessage)
		}
	}
}
