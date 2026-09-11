package main

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCheckValidatesTLS(t *testing.T) {
	for _, tc := range []struct {
		name      string
		yaml      string
		wantError bool
	}{
		{"enabled missing CA", "nut: {tls: {enable: true, ca_file: /nonexistent/ups-client-test-ca.pem}}", true},
		{"disabled missing CA", "nut: {tls: {enable: false, ca_file: /nonexistent/ups-client-test-ca.pem}}", false},
		{"default trust", "nut: {tls: {enable: true}}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestCheckHelper$")
			cmd.Env = append(os.Environ(), "UPS_CLIENT_TEST_CONFIG="+path)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, wantError = %v; output: %s", err, tc.wantError, out)
			}
			if !tc.wantError && !strings.Contains(string(out), "config OK") {
				t.Fatalf("missing validation result: %s", out)
			}
		})
	}
}

func TestCheckRejectsNonRegularCA(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses /proc file descriptors")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`nut: {tls: {enable: true, ca_file: /proc/self/fd/3}}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCheckHelper$")
	cmd.Env = append(os.Environ(), "UPS_CLIENT_TEST_CONFIG="+path)
	cmd.ExtraFiles = []*os.File{reader}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("-check blocked while reading a nonregular CA file")
	}
	if err == nil || !strings.Contains(string(out), "regular file") {
		t.Fatalf("expected nonregular CA rejection, got %v: %s", err, out)
	}
}

func TestCheckHelper(t *testing.T) {
	path := os.Getenv("UPS_CLIENT_TEST_CONFIG")
	if path == "" {
		return
	}
	os.Args = []string{"ups-client", "-check", "-config", path}
	flag.CommandLine = flag.NewFlagSet("ups-client", flag.ExitOnError)
	main()
	os.Exit(0)
}
