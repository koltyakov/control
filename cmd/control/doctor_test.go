package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
)

func doctorTestReport(statuses ...string) client.DoctorReport {
	r := client.DoctorReport{Checks: []client.DoctorCheck{}}
	for i, status := range statuses {
		r.Checks = append(r.Checks, client.DoctorCheck{Check: "check" + string(rune('a'+i)), Status: status, Detail: "detail"})
		switch status {
		case client.DoctorOK:
			r.Summary.OK++
		case client.DoctorWarning:
			r.Summary.Warning++
		case client.DoctorFailed:
			r.Summary.Failed++
		default:
			r.Summary.Skipped++
		}
	}
	r.Summary.Status = client.DoctorOK
	if r.Summary.Warning > 0 {
		r.Summary.Status = client.DoctorWarning
	}
	if r.Summary.Failed > 0 {
		r.Summary.Status = client.DoctorFailed
	}
	return r
}

func TestDoctorCLIExitStatusAndOptions(t *testing.T) {
	for _, test := range []struct {
		args     []string
		report   client.DoctorReport
		node     string
		timeout  time.Duration
		check    time.Duration
		wantFail bool
	}{
		{nil, doctorTestReport(client.DoctorOK, client.DoctorSkipped), "", client.DefaultDoctorTimeout, client.DefaultDoctorCheckTimeout, false},
		{[]string{"worker"}, doctorTestReport(client.DoctorOK, client.DoctorWarning), "worker", client.DefaultDoctorTimeout, client.DefaultDoctorCheckTimeout, false},
		{[]string{"--timeout", "5s", "--check-timeout", "2s", "worker"}, doctorTestReport(client.DoctorFailed), "worker", 5 * time.Second, 2 * time.Second, true},
		{[]string{"worker", "--json"}, doctorTestReport(client.DoctorWarning, client.DoctorFailed), "worker", client.DefaultDoctorTimeout, client.DefaultDoctorCheckTimeout, true},
	} {
		var out bytes.Buffer
		var got client.DoctorOptions
		err := runDoctorCLI(t.Context(), &out, test.args, func(opts client.DoctorOptions) (client.DoctorReport, error) {
			got = opts
			return test.report, nil
		})
		if (err != nil) != test.wantFail {
			t.Errorf("%v: error %v, want failure %v", test.args, err, test.wantFail)
		}
		if got.Node != test.node || got.Timeout != test.timeout || got.CheckTimeout != test.check {
			t.Errorf("%v: options %+v", test.args, got)
		}
		if strings.Contains(strings.Join(test.args, " "), "--json") {
			var decoded client.DoctorReport
			if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || decoded.Summary != test.report.Summary || len(decoded.Checks) != len(test.report.Checks) {
				t.Errorf("%v: JSON %q %v", test.args, out.String(), err)
			}
		} else if !strings.Contains(out.String(), test.report.Summary.Status+":") {
			t.Errorf("%v: text %q", test.args, out.String())
		}
	}
}

func TestDoctorCLIRejectsBadArgumentsBeforeRunning(t *testing.T) {
	for _, args := range [][]string{
		{"a", "b"},
		{"--timeout", "500ms"},
		{"--timeout", "11m"},
		{"--timeout", "5s", "--check-timeout", "6s"},
		{"--unknown"},
	} {
		ran := false
		err := runDoctorCLI(t.Context(), &bytes.Buffer{}, args, func(client.DoctorOptions) (client.DoctorReport, error) {
			ran = true
			return client.DoctorReport{}, nil
		})
		if err == nil || ran {
			t.Errorf("%v: error %v, ran %v", args, err, ran)
		}
	}
}

func TestDoctorCLIHelpStatesReadOnly(t *testing.T) {
	var out bytes.Buffer
	ran := false
	err := runDoctorCLI(t.Context(), &out, []string{"--help"}, func(client.DoctorOptions) (client.DoctorReport, error) {
		ran = true
		return client.DoctorReport{}, nil
	})
	if err != nil || ran {
		t.Fatal(err, ran)
	}
	for _, want := range []string{"starts no task", "requests no grant or lease", "retries nothing", "-check-timeout"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help lacks %q:\n%s", want, out.String())
		}
	}
	// Every role sees the command, which is in the common help section.
	if !strings.Contains(usage, "doctor [NODE]") || !strings.Contains(usage, "starts no work, grants or leases") {
		t.Fatal("common help omits doctor")
	}
}
