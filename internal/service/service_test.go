package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/execx"
)

func TestServiceText(t *testing.T) {
	s, err := ServiceText("/opt/my tools/devc", []string{"PATH=/usr/bin:/home/u/bin", "DEVC_CONFIG_DIR=/home/u/100%/\"cfg\"", "DEVC_STATE_DIR=/s/$HOME"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[Service]\nType=oneshot\n",
		`Environment="PATH=/usr/bin:/home/u/bin"`,
		`Environment="DEVC_CONFIG_DIR=/home/u/100%%/\"cfg\""`,
		`Environment="DEVC_STATE_DIR=/s/$HOME"`, // no $ expansion in Environment=
		`ExecStart=-"/opt/my tools/devc" ingest --all --quiet` + "\n" + `ExecStart=-"/opt/my tools/devc" bank --all --quiet`,
		`ExecStart="/opt/my tools/devc" context commit --all`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if s, _ := ServiceText("/x/$devc", nil); !strings.Contains(s, `ExecStart=-"/x/$$devc"`) {
		t.Errorf("$ in a command is not escaped:\n%s", s)
	}
	for _, bad := range [][]string{{"PATH=/a\n[Service]\nExecStart=/bin/evil"}, {"=x"}, {"noequals"}, {"A B=x"}} {
		if _, err := ServiceText("/usr/bin/devc", bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := ServiceText("devc", nil); err == nil {
		t.Error("accepted a relative executable")
	}
	if _, err := ServiceText("/a\nExecStart=/bin/evil", nil); err == nil {
		t.Error("accepted a newline in the executable")
	}
	timer := TimerText()
	for _, want := range []string{"OnUnitActiveSec=5min", "OnBootSec=5min", "WantedBy=timers.target"} {
		if !strings.Contains(timer, want) {
			t.Errorf("timer lacks %q:\n%s", want, timer)
		}
	}
}

func TestUserUnitDir(t *testing.T) {
	env := map[string]string{"HOME": "/home/u"}
	get := func(k string) string { return env[k] }
	if d, _ := UserUnitDir(get); d != "/home/u/.config/systemd/user" {
		t.Errorf("%q", d)
	}
	env["XDG_CONFIG_HOME"] = "/x"
	if d, _ := UserUnitDir(get); d != "/x/systemd/user" {
		t.Errorf("%q", d)
	}
	if _, err := UserUnitDir(func(string) string { return "" }); err == nil {
		t.Error("no error without HOME")
	}
}

func TestInstallUninstall(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "systemd", "user")
	f := execx.NewFake()
	f.On("systemctl", "--user")
	m := &Manager{Dir: dir, Runner: f}
	if m.Installed() {
		t.Fatal("installed before install")
	}
	if err := m.Install(context.Background(), "/usr/local/bin/devc", []string{"PATH=/usr/bin"}); err != nil {
		t.Fatal(err)
	}
	if !m.Installed() {
		t.Fatal("not installed after install")
	}
	svc, _ := os.ReadFile(filepath.Join(dir, ServiceFile))
	if !strings.Contains(string(svc), `ExecStart=-"/usr/local/bin/devc" bank --all --quiet`) {
		t.Errorf("service file:\n%s", svc)
	}
	var got []string
	for _, c := range f.Calls() {
		got = append(got, c.Name+" "+strings.Join(c.Args, " "))
		if c.Dir != "/" {
			t.Errorf("%s ran in %q", c.Name, c.Dir)
		}
	}
	if strings.Join(got, "; ") != "systemctl --user daemon-reload; systemctl --user enable --now devc-bank.timer" {
		t.Errorf("calls: %q", got)
	}
	// Reinstalling replaces the files.
	if err := m.Install(context.Background(), "/opt/devc", nil); err != nil {
		t.Fatal(err)
	}
	if svc, _ := os.ReadFile(filepath.Join(dir, ServiceFile)); !strings.Contains(string(svc), `"/opt/devc"`) {
		t.Errorf("reinstall:\n%s", svc)
	}
	f.On("systemctl", "--user", "list-timers").Stdout("NEXT LEFT LAST PASSED UNIT\n")
	f.On("systemctl", "--user", "status").Stdout("inactive\n").Exit(3)
	if s, err := m.Status(context.Background()); err != nil || !strings.Contains(s, "UNIT") || !strings.Contains(s, "inactive") {
		t.Errorf("status: %q %v", s, err)
	}
	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Installed() {
		t.Error("installed after uninstall")
	}
	if _, err := os.Stat(filepath.Join(dir, TimerFile)); err == nil {
		t.Error("timer file left behind")
	}
	if !f.Called("systemctl", "--user", "disable", "--now", TimerFile) {
		t.Error("the timer was not disabled")
	}
	// Uninstalling twice is fine; a failing systemctl does not matter then.
	f.On("systemctl", "--user", "disable").Exit(1)
	if err := m.Uninstall(context.Background()); err != nil {
		t.Errorf("second uninstall: %v", err)
	}
}
