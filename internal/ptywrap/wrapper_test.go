package ptywrap

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func needPTY(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no /dev/ptmx")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
}

// runPipes runs a shell script under the wrapper with pipes for its own
// standard input and output.
func runPipes(t *testing.T, script string, input string) (Result, string, []Report) {
	return runPipesAfter(t, script, input, 0)
}

// runPipesAfter writes the input after a delay (once the script has set
// its terminal up).
func runPipesAfter(t *testing.T, script string, input string, delay time.Duration) (Result, string, []Report) {
	t.Helper()
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	var mu sync.Mutex
	var reports []Report
	var out bytes.Buffer
	copied := make(chan struct{})
	go func() { io.Copy(&out, outR); close(copied) }()
	go func() { time.Sleep(delay); inW.WriteString(input); inW.Close() }()
	res, err := Run(Options{Path: "/bin/sh", Args: []string{"sh", "-c", script}, Env: []string{"PATH=/usr/bin:/bin"},
		Stdin: inR, Stdout: outW, OnReport: func(r Report) { mu.Lock(); reports = append(reports, r); mu.Unlock() },
		Drain: 300 * time.Millisecond})
	outW.Close()
	<-copied
	inR.Close()
	outR.Close()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return res, out.String(), reports
}

func TestRunStatusAndReports(t *testing.T) {
	needPTY(t)
	res, out, reports := runPipes(t, `printf 'a\033]7501;state=working:app=t\007b\033]7501;state=done:msg=aGk\033\\c'; exit 3`, "")
	if res.Status != 3 || res.Signal != 0 {
		t.Errorf("result %+v", res)
	}
	if out != "abc" {
		t.Errorf("output %q", out)
	}
	if len(reports) != 2 || reports[0].State != "working" || reports[1].State != "done" || reports[1].Msg != "hi" {
		t.Errorf("reports %+v", reports)
	}
	res, _, _ = runPipes(t, `kill -9 $$`, "")
	if res.Signal != syscall.SIGKILL || res.Status != -1 {
		t.Errorf("killed: %+v", res)
	}
}

func TestRunAnswersQuery(t *testing.T) {
	needPTY(t)
	if _, err := os.Stat("/bin/dd"); err != nil {
		t.Skip("no dd")
	}
	// The harness asks, reads the answer from its terminal and prints it
	// back (as an unterminated sequence the scanner passes through).
	res, out, _ := runPipes(t, `stty raw -echo; printf '\033]7501;?\007'; dd bs=1 count=`+strconv.Itoa(len(Reply)+1)+` 2>/dev/null | od -An -c | tr -d ' \n'`, "")
	if res.Status != 0 {
		t.Errorf("result %+v (output %q)", res, out)
	}
	if !strings.Contains(out, "]7501;?:states=idle,working,done,blocked,error:kinds=permission,question,auth\\a") {
		t.Errorf("the harness read %q", out)
	}
}

func TestRunInputPassesThrough(t *testing.T) {
	needPTY(t)
	// Arbitrary bytes on the wrapper's input reach the harness unchanged
	// (the harness's terminal is raw, so the kernel changes nothing either).
	data := make([]byte, 0, 4096)
	for i := 0; len(data) < 4000; i++ {
		b := byte(i * 7)
		if b == 0x1b || b == 0x0a {
			continue // the harness's reader below is line-free; keep it simple
		}
		data = append(data, b)
	}
	if _, err := os.Stat("/bin/dd"); err != nil {
		t.Skip("no dd")
	}
	res, out, _ := runPipesAfter(t, `stty raw -echo; dd bs=1 count=`+strconv.Itoa(len(data))+` 2>/dev/null | od -An -v -tx1 | tr -d ' \n'`, string(data), 500*time.Millisecond)
	if res.Status != 0 {
		t.Fatalf("result %+v", res)
	}
	var hex strings.Builder
	for _, b := range data {
		hex.WriteString(string("0123456789abcdef"[b>>4]) + string("0123456789abcdef"[b&15]))
	}
	if out != hex.String() {
		t.Errorf("the harness read different bytes:\n got %.200s\nwant %.200s", out, hex.String())
	}
}

// TestRunRestoresTerminal gives the wrapper a terminal of its own and checks
// that its settings are back after the harness was killed.
func TestRunRestoresTerminal(t *testing.T) {
	needPTY(t)
	master, slave, err := openPTY()
	if err != nil {
		t.Skip(err)
	}
	defer master.Close()
	defer slave.Close()
	go io.Copy(io.Discard, master)
	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(Options{Path: "/bin/sh", Args: []string{"sh", "-c", "sleep 0.2; kill -9 $$"}, Env: []string{"PATH=/usr/bin:/bin"},
		Stdin: slave, Stdout: slave, Drain: 200 * time.Millisecond})
	if err != nil || res.Signal != syscall.SIGKILL {
		t.Fatalf("%+v %v", res, err)
	}
	after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if *before != *after {
		t.Errorf("terminal settings changed:\nbefore %+v\nafter  %+v", before, after)
	}
}

// TestRunFlushesLoneEscape: a lone escape character that ends the output
// for a while is passed on after a short wait, not held until more output
// comes.
func TestRunFlushesLoneEscape(t *testing.T) {
	needPTY(t)
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	defer inW.Close()
	seen := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		var got []byte
		buf := make([]byte, 64)
		for {
			n, err := outR.Read(buf)
			got = append(got, buf[:n]...)
			if bytes.Contains(got, []byte("a\x1b")) {
				seen <- time.Since(start)
				io.Copy(io.Discard, outR)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	res, err := Run(Options{Path: "/bin/sh", Args: []string{"sh", "-c", `printf 'a\033'; sleep 1.5`}, Env: []string{"PATH=/usr/bin:/bin"},
		Stdin: inR, Stdout: outW, Drain: 200 * time.Millisecond})
	outW.Close()
	if err != nil || res.Status != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	select {
	case d := <-seen:
		if d > time.Second {
			t.Errorf("the escape came after %s, when the harness exited", d)
		}
	default:
		t.Error("the escape never came")
	}
}
